/**
 * per-provider 并发控制队列
 * 防止同时向同一个 provider 发送过多请求，避免触发 rate limit
 */

// 每个 provider 的队列状态: { running: number, queue: Array }
const providerQueues = new Map();

/**
 * 初始化 provider 队列
 * @param {string} providerName
 * @param {number} maxConcurrent - 最大并发数
 */
function ensureQueue(providerName, maxConcurrent) {
  if (!providerQueues.has(providerName)) {
    providerQueues.set(providerName, {
      running: 0,
      maxConcurrent: maxConcurrent || 5,
      queue: [],
    });
  }
}

/**
 * 从队列中取出下一个任务执行
 * @param {string} providerName
 */
function processNext(providerName) {
  const state = providerQueues.get(providerName);
  if (!state || state.queue.length === 0) return;

  // 队列中有等待的任务，检查是否可以执行
  if (state.running < state.maxConcurrent) {
    const { task, resolve, reject } = state.queue.shift();
    state.running++;

    task()
      .then(resolve)
      .catch(reject)
      .finally(() => {
        state.running--;
        processNext(providerName);
      });
  }
}

/**
 * 将任务加入 provider 队列
 * @param {string} providerName - provider 名称
 * @param {number} maxConcurrent - 最大并发数
 * @param {Function} task - 返回 Promise 的任务函数
 * @returns {Promise}
 */
export function enqueue(providerName, maxConcurrent, task) {
  ensureQueue(providerName, maxConcurrent);

  const state = providerQueues.get(providerName);

  // 更新 maxConcurrent（可能配置变了）
  state.maxConcurrent = maxConcurrent || 5;

  return new Promise((resolve, reject) => {
    // 检查是否可以立即执行
    if (state.running < state.maxConcurrent && state.queue.length === 0) {
      state.running++;

      task()
        .then(resolve)
        .catch(reject)
        .finally(() => {
          state.running--;
          processNext(providerName);
        });
    } else {
      // 加入队列等待
      state.queue.push({ task, resolve, reject });
    }
  });
}

/**
 * 获取队列状态（用于日志）
 * @param {string} providerName
 * @returns {{ running: number, queued: number, maxConcurrent: number }}
 */
export function getQueueStatus(providerName) {
  const state = providerQueues.get(providerName);
  if (!state) return { running: 0, queued: 0, maxConcurrent: 0 };

  return {
    running: state.running,
    queued: state.queue.length,
    maxConcurrent: state.maxConcurrent,
  };
}
