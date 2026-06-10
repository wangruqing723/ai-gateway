/**
 * per-provider 并发控制队列
 * 防止同时向同一个 provider 发送过多请求，避免触发 rate limit
 * 支持并发限制 + 速率限制（每秒最多 N 个请求）
 */

// 每个 provider 的队列状态
const providerQueues = new Map();

/**
 * 初始化 provider 队列
 * @param {string} providerName
 * @param {number} maxConcurrent - 最大并发数
 * @param {number} maxPerSecond - 每秒最多请求数（默认不限制）
 */
function ensureQueue(providerName, maxConcurrent, maxPerSecond) {
  if (!providerQueues.has(providerName)) {
    providerQueues.set(providerName, {
      running: 0,
      maxConcurrent: maxConcurrent || 5,
      maxPerSecond: maxPerSecond || 0,  // 0 表示不限制
      queue: [],
      requestTimes: [],  // 记录最近的请求时间戳
      activeTasks: new Map(),  // 运行中任务的开始时间，用于卡死检测
    });
  }
}

/**
 * 检查是否满足速率限制
 * @param {object} state - 队列状态
 * @returns {{ allowed: boolean, delay: number }}
 */
function checkRateLimit(state) {
  if (!state.maxPerSecond) {
    return { allowed: true, delay: 0 };
  }

  const now = Date.now();
  const oneSecondAgo = now - 1000;

  // 清理超过 1 秒的记录
  state.requestTimes = state.requestTimes.filter(t => t > oneSecondAgo);

  // 检查是否超过限制
  if (state.requestTimes.length >= state.maxPerSecond) {
    // 计算需要等待的时间（等到最早的请求超过 1 秒）
    const oldestRequest = state.requestTimes[0];
    const delay = oldestRequest + 1000 - now + 10;  // 加 10ms 确保间隔
    return { allowed: false, delay: Math.max(0, delay) };
  }

  return { allowed: true, delay: 0 };
}

/**
 * 记录请求时间
 * @param {object} state - 队列状态
 */
function recordRequest(state) {
  if (state.maxPerSecond) {
    state.requestTimes.push(Date.now());
  }
}

/**
 * 从队列中取出下一个任务执行
 * @param {string} providerName
 */
function processNext(providerName) {
  const state = providerQueues.get(providerName);
  if (!state || state.queue.length === 0) return;

  // 检查并发限制
  if (state.running >= state.maxConcurrent) return;

  // 检查速率限制
  const rateLimit = checkRateLimit(state);
  if (!rateLimit.allowed) {
    // 延迟后重试
    setTimeout(() => processNext(providerName), rateLimit.delay);
    return;
  }

  // 取出任务执行
  const { task, resolve, reject, timeout } = state.queue.shift();

  // 清除超时定时器
  if (timeout) {
    clearTimeout(timeout);
  }

  state.running++;
  recordRequest(state);

  const taskId = Symbol('task');
  state.activeTasks.set(taskId, Date.now());

  task()
    .then(resolve)
    .catch(reject)
    .finally(() => {
      state.running--;
      state.activeTasks.delete(taskId);
      processNext(providerName);
    });
}

/**
 * 将任务加入 provider 队列
 * @param {string} providerName - provider 名称
 * @param {number} maxConcurrent - 最大并发数
 * @param {Function} task - 返回 Promise 的任务函数
 * @param {number} maxPerSecond - 每秒最多请求数（可选）
 * @param {number} maxQueueWait - 队列最大等待时间（毫秒，默认 30 秒）
 * @returns {Promise}
 */
export function enqueue(providerName, maxConcurrent, task, maxPerSecond = 0, maxQueueWait = 30000) {
  ensureQueue(providerName, maxConcurrent, maxPerSecond);

  const state = providerQueues.get(providerName);

  // 更新配置（可能配置变了）
  state.maxConcurrent = maxConcurrent || 5;
  if (maxPerSecond) {
    state.maxPerSecond = maxPerSecond;
  }

  return new Promise((resolve, reject) => {
    // 检查是否可以立即执行
    if (state.running < state.maxConcurrent && state.queue.length === 0) {
      const rateLimit = checkRateLimit(state);

      if (rateLimit.allowed) {
        // 可以立即执行
        state.running++;
        recordRequest(state);

        const taskId = Symbol('task');
        state.activeTasks.set(taskId, Date.now());

        task()
          .then(resolve)
          .catch(reject)
          .finally(() => {
            state.running--;
            state.activeTasks.delete(taskId);
            processNext(providerName);
          });
        return;
      }
    }

    // 加入队列等待，添加超时机制
    const queueItem = { task, resolve, reject, enqueueTime: Date.now() };

    // 设置队列等待超时
    if (maxQueueWait > 0) {
      queueItem.timeout = setTimeout(() => {
        // 从队列中移除
        const index = state.queue.indexOf(queueItem);
        if (index > -1) {
          state.queue.splice(index, 1);
          reject(new Error(`队列等待超时 (${maxQueueWait / 1000}秒)，请稍后重试`));
        }
      }, maxQueueWait);
    }

    state.queue.push(queueItem);
  });
}

/**
 * 获取队列状态（用于日志和健康检查）
 * @param {string} providerName
 * @returns {{ running: number, queued: number, maxConcurrent: number, maxPerSecond: number, longestRunningMs: number }}
 */
export function getQueueStatus(providerName) {
  const state = providerQueues.get(providerName);
  if (!state) return { running: 0, queued: 0, maxConcurrent: 0, maxPerSecond: 0, longestRunningMs: 0 };

  // 计算最长运行时间，用于检测卡死任务
  let longestRunningMs = 0;
  const now = Date.now();
  for (const startTime of state.activeTasks.values()) {
    const duration = now - startTime;
    if (duration > longestRunningMs) longestRunningMs = duration;
  }

  return {
    running: state.running,
    queued: state.queue.length,
    maxConcurrent: state.maxConcurrent,
    maxPerSecond: state.maxPerSecond,
    longestRunningMs,
  };
}
