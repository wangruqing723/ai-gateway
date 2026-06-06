import http  from 'http';
import https from 'https';
import { getImageHash, getCachedDescription, setCachedDescription } from './cache.js';
import { enqueue } from './queue.js';

// 正在识别中的图片 Promise 缓存，防止并发重复识别
const pendingRecognitions = new Map();

// Anthropic image 块 → OpenAI image_url 块
function toOpenAIImageBlock(block) {
  const src = block.source || {};
  if (src.type === 'base64') {
    return { type: 'image_url', image_url: { url: `data:${src.media_type};base64,${src.data}` } };
  }
  if (src.type === 'url') {
    return { type: 'image_url', image_url: { url: src.url } };
  }
  throw new Error(`不支持的图片 source 类型: ${src.type}`);
}

/**
 * 调用视觉模型描述图片（OpenAI /chat/completions 格式）
 * @param {object} imageBlock  - Anthropic 格式的 image 块
 * @param {object} vision      - { baseUrl, apiKey, model, name, maxConcurrent }
 * @returns {Promise<{text: string, fromCache: boolean}>}
 */
async function callVision(imageBlock, vision) {
  const hash = getImageHash(imageBlock);

  // 查询 SQLite 缓存
  const cached = await getCachedDescription(hash);
  if (cached) return { text: cached, fromCache: true };

  // 如果这张图片正在被其他请求识别，等待同一个 Promise
  if (pendingRecognitions.has(hash)) {
    return pendingRecognitions.get(hash);
  }

  // 通过队列控制并发
  const providerName = vision.name || 'vision';
  const maxConcurrent = vision.maxConcurrent || 5;

  const recognitionPromise = enqueue(providerName, maxConcurrent, () => {
    return new Promise((resolve, reject) => {
      let openAIBlock;
      try { openAIBlock = toOpenAIImageBlock(imageBlock); }
      catch (e) { return reject(e); }

      const prompt = '请对这张图片进行全面详细的描述，包括所有可见文字（原文）、数据与图表数值、代码内容、UI布局与元素等，确保纯文本模型仅凭此描述即可完整理解图片。';
      const body = JSON.stringify({
        model: vision.model,
        max_completion_tokens: 3000,
        messages: [{ role: 'user', content: [openAIBlock, { type: 'text', text: prompt }] }],
      });

      const visionUrl = new URL(vision.baseUrl.startsWith('http') ? vision.baseUrl : `https://${vision.baseUrl}`);
      const basePath  = visionUrl.pathname.replace(/\/$/, '').replace(/\/v1$/, '');
      const isHttps   = visionUrl.protocol === 'https:';

      const req = (isHttps ? https : http).request({
        hostname: visionUrl.hostname,
        port:     visionUrl.port || (isHttps ? 443 : 80),
        path:     basePath + '/v1/chat/completions',
        method:   'POST',
        headers:  {
          'content-type':   'application/json',
          'content-length': Buffer.byteLength(body),
          'authorization':  `Bearer ${vision.apiKey}`,
        },
      }, (res) => {
        let raw = '';
        res.on('data', c => raw += c);
        res.on('end', () => {
          try {
            const response = JSON.parse(raw);
            const message = response?.choices?.[0]?.message;

            // 兼容不同模型：优先使用 content，其次使用 reasoning_content
            const text = message?.content || message?.reasoning_content;

            if (text) {
              // 存入 SQLite 缓存
              setCachedDescription(hash, text);
              resolve({ text, fromCache: false });
            } else {
              reject(new Error(`视觉 API 响应异常 (HTTP ${res.statusCode}): ${raw.slice(0, 200)}`));
            }
          } catch (e) { reject(new Error(`解析视觉响应失败: ${e.message}`)); }
        });
      });
      req.on('error', reject);
      req.write(body);
      req.end();
    });
  });

  // 将 Promise 存入缓存，并在完成时清除
  pendingRecognitions.set(hash, recognitionPromise);
  recognitionPromise.finally(() => {
    pendingRecognitions.delete(hash);
  });

  return recognitionPromise;
}

// 处理单条 content 数组，递归替换 image 块
// skipImageRecognition: true 时，只删除图片不进行识别
async function processBlocks(blocks, vision, idxRef, log, stats, skipImageRecognition = false) {
  const out = [];
  for (const block of blocks) {
    if (block.type === 'image') {
      idxRef.n++;
      const n = idxRef.n;

      if (skipImageRecognition) {
        stats.skipped++;
        continue;
      }

      try {
        const result = await callVision(block, vision);
        if (result.fromCache) {
          stats.cached++;
        } else {
          stats.recognized++;
          log(`  图片 #${n} 识别完成 (${result.text.length} 字) [${vision.model}]`);
        }
        out.push({ type: 'text', text: `[图片描述 #${n}]\n${result.text}\n[/图片描述 #${n}]` });
      } catch (err) {
        stats.failed++;
        log(`  图片 #${n} 识别失败: ${err.message}`);
        out.push({ type: 'text', text: `[图片 #${n} 识别失败: ${err.message}]` });
      }
    } else if (block.type === 'tool_result' && Array.isArray(block.content)) {
      // tool_result 内嵌图片
      const inner = await processBlocks(block.content, vision, idxRef, log, stats, skipImageRecognition);
      out.push({ ...block, content: inner });
    } else {
      out.push(block);
    }
  }
  return out;
}

/**
 * 遍历所有消息，将图片块替换为视觉模型生成的文字描述
 * 所有图片都会处理（历史图片保留上下文），缓存命中时速度很快
 * @param {object[]} messages - Anthropic 格式消息数组
 * @param {object}   vision   - visionProvider 配置
 * @param {Function} log      - 日志函数
 */
export async function translateImages(messages, vision, log = () => {}) {
  const idxRef = { n: 0 };
  const stats = { cached: 0, recognized: 0, failed: 0, skipped: 0 };
  const out = [];

  for (const msg of messages) {
    if (!Array.isArray(msg.content)) { out.push(msg); continue; }

    const hasImg = msg.content.some(b =>
      b.type === 'image' ||
      (b.type === 'tool_result' && Array.isArray(b.content) && b.content.some(c => c.type === 'image'))
    );
    if (!hasImg) { out.push(msg); continue; }

    const newContent = await processBlocks(msg.content, vision, idxRef, log, stats, false);
    out.push({ ...msg, content: newContent });
  }

  // 打印汇总日志
  const total = stats.cached + stats.recognized + stats.failed;
  if (total > 0) {
    const parts = [];
    if (stats.cached > 0) parts.push(`${stats.cached} 缓存`);
    if (stats.recognized > 0) parts.push(`${stats.recognized} 识别`);
    if (stats.failed > 0) parts.push(`${stats.failed} 失败`);
    if (stats.skipped > 0) parts.push(`${stats.skipped} 跳过`);
    log(`  图片: ${parts.join(', ')} [${vision.model}]`);
  }

  return out;
}

/**
 * 检测消息数组中是否包含图片块
 */
export function hasImages(messages) {
  if (!Array.isArray(messages)) return false;
  return messages.some(msg => {
    if (!Array.isArray(msg.content)) return false;
    return msg.content.some(b =>
      b.type === 'image' ||
      (b.type === 'tool_result' && Array.isArray(b.content) && b.content.some(c => c.type === 'image'))
    );
  });
}
