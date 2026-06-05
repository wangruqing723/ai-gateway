import http  from 'http';
import https from 'https';

// 内存缓存：key=图片内容指纹，value=文字描述
const imageCache = new Map();

function getImageHash(imageBlock) {
  const src = imageBlock.source || {};
  const raw = src.type === 'base64'
    ? `${src.media_type}_${(src.data||'').length}_${(src.data||'').slice(0,80)}_${(src.data||'').slice(-40)}`
    : (src.url || src.image_url?.url || '');
  let h = 5381;
  for (let i = 0; i < raw.length; i++) { h = ((h << 5) + h) ^ raw.charCodeAt(i); h >>>= 0; }
  return h.toString(36);
}

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
 * @param {object} vision      - { baseUrl, apiKey, model }
 */
function callVision(imageBlock, vision) {
  const hash = getImageHash(imageBlock);
  if (imageCache.has(hash)) return Promise.resolve(imageCache.get(hash));

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
          const text = JSON.parse(raw)?.choices?.[0]?.message?.content;
          if (text) { imageCache.set(hash, text); resolve(text); }
          else reject(new Error(`视觉 API 响应异常 (HTTP ${res.statusCode}): ${raw.slice(0, 200)}`));
        } catch (e) { reject(new Error(`解析视觉响应失败: ${e.message}`)); }
      });
    });
    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

// 处理单条 content 数组，递归替换 image 块
async function processBlocks(blocks, vision, idxRef, log) {
  const out = [];
  for (const block of blocks) {
    if (block.type === 'image') {
      idxRef.n++;
      const n = idxRef.n;
      try {
        const desc = await callVision(block, vision);
        if (desc.fromCache) log(`⚡ 图片 #${n} 命中缓存`);
        else log(`✓ 图片 #${n} 识别完成 (${desc.length} 字)`);
        out.push({ type: 'text', text: `[图片描述 #${n}]\n${desc}\n[/图片描述 #${n}]` });
      } catch (err) {
        log(`✗ 图片 #${n} 识别失败: ${err.message}`);
        out.push({ type: 'text', text: `[图片 #${n} 识别失败: ${err.message}]` });
      }
    } else if (block.type === 'tool_result' && Array.isArray(block.content)) {
      // tool_result 内嵌图片
      const inner = await processBlocks(block.content, vision, idxRef, log);
      out.push({ ...block, content: inner });
    } else {
      out.push(block);
    }
  }
  return out;
}

/**
 * 遍历所有消息，将图片块替换为视觉模型生成的文字描述
 * @param {object[]} messages - Anthropic 格式消息数组
 * @param {object}   vision   - visionProvider 配置
 * @param {Function} log      - 日志函数
 */
export async function translateImages(messages, vision, log = () => {}) {
  const idxRef = { n: 0 };
  const out = [];

  for (const msg of messages) {
    if (!Array.isArray(msg.content)) { out.push(msg); continue; }

    const hasImg = msg.content.some(b =>
      b.type === 'image' ||
      (b.type === 'tool_result' && Array.isArray(b.content) && b.content.some(c => c.type === 'image'))
    );
    if (!hasImg) { out.push(msg); continue; }

    const newContent = await processBlocks(msg.content, vision, idxRef, log);
    out.push({ ...msg, content: newContent });
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
