import http  from 'http';
import https from 'https';
import { createStreamTransformer, convertAnthropicResponse, convertOpenAIChatResponse } from './converter.js';

/**
 * 转发请求到上游 provider，处理流式/非流式响应，并转换回客户端格式
 * @param {object} opts
 *   clientReq     - 原始客户端请求对象
 *   clientRes     - 原始客户端响应对象
 *   upstreamBody  - 已转换好的上游请求 JSON 对象
 *   provider      - { baseUrl, apiKey, format }
 *   clientFormat  - 'anthropic' | 'openai-chat' | 'openai-responses'
 *   originalModel - 客户端发来的原始模型名
 *   isStreaming    - 是否流式
 *   log           - 日志函数
 *   startTime     - 请求开始时间
 *   timeout       - 请求超时（毫秒）
 * @returns {Promise} 响应完成后 resolve（释放队列 slot）
 */
export function forwardRequest(opts) {
  const { clientReq, clientRes, upstreamBody, provider, clientFormat, originalModel, isStreaming, log, startTime, timeout = 60000 } = opts;

  const upstreamUrl  = new URL(provider.baseUrl.startsWith('http') ? provider.baseUrl : `https://${provider.baseUrl}`);
  const basePath     = upstreamUrl.pathname.replace(/\/$/, '').replace(/\/v1$/, '');
  const isHttps      = upstreamUrl.protocol === 'https:';
  const upstreamPath = basePath + (provider.format === 'anthropic' ? '/v1/messages' : '/v1/chat/completions');

  const bodyStr = JSON.stringify(upstreamBody);

  const headers = {
    'content-type':   'application/json',
    'content-length': Buffer.byteLength(bodyStr),
    'accept':         'application/json, text/event-stream',
  };
  if (provider.format === 'anthropic') {
    headers['x-api-key']         = provider.apiKey;
    headers['anthropic-version'] = '2023-06-01';
  } else {
    headers['authorization'] = `Bearer ${provider.apiKey}`;
  }

  // 返回 Promise，响应完成后 resolve
  return new Promise((resolve, reject) => {
    const proxyReq = (isHttps ? https : http).request({
      hostname: upstreamUrl.hostname,
      port:     upstreamUrl.port || (isHttps ? 443 : 80),
      path:     upstreamPath,
      method:   'POST',
      headers,
      timeout:  timeout,
    }, (proxyRes) => {
      const elapsed = startTime ? Date.now() - startTime : 0;

      // 精简的响应日志
      if (proxyRes.statusCode >= 400) {
        log(`← HTTP ${proxyRes.statusCode} [${elapsed}ms]`);
      } else {
        log(`← ${proxyRes.statusCode} [${elapsed}ms]`);
      }

      // 错误响应：立即释放 slot
      if (proxyRes.statusCode >= 400) {
        handleError(proxyRes, clientRes, proxyRes.statusCode, log);
        resolve();
        return;
      }

      // 非流式响应：响应到达即可释放
      if (!isStreaming) {
        handleResponse(proxyRes, clientRes, provider.format, clientFormat, originalModel, log);
        resolve();
        return;
      }

      // 流式响应：等待传输完成后再释放
      handleStream(proxyRes, clientRes, provider.format, clientFormat, originalModel, clientReq.headers, () => {
        resolve();
      });
    });

    // 请求超时
    proxyReq.on('timeout', () => {
      proxyReq.destroy();
      const err = new Error('请求超时');
      log(`转发失败: ${err.message}`);
      if (!clientRes.headersSent) {
        clientRes.writeHead(504, { 'content-type': 'application/json' });
        clientRes.end(JSON.stringify({ error: { type: 'timeout_error', message: err.message } }));
      }
      reject(err);
    });

    // 请求错误
    proxyReq.on('error', (err) => {
      log(`转发失败: ${err.message}`);
      if (!clientRes.headersSent) {
        clientRes.writeHead(502, { 'content-type': 'application/json' });
        clientRes.end(JSON.stringify({ error: { type: 'proxy_error', message: err.message } }));
      }
      reject(err);
    });

    proxyReq.write(bodyStr);
    proxyReq.end();
  });
}

// ── 非流式响应处理 ────────────────────────────

function handleResponse(proxyRes, clientRes, providerFormat, clientFormat, originalModel, log) {
  let raw = '';
  proxyRes.on('data', c => raw += c);
  proxyRes.on('end', () => {
    let data;
    try { data = JSON.parse(raw); }
    catch (e) {
      clientRes.writeHead(502, { 'content-type': 'application/json' });
      clientRes.end(JSON.stringify({ error: { type: 'parse_error', message: `上游响应解析失败: ${e.message}` } }));
      return;
    }

    const result = providerFormat === 'anthropic'
      ? convertAnthropicResponse(data, clientFormat, originalModel)
      : convertOpenAIChatResponse(data, clientFormat, originalModel);

    const body = JSON.stringify(result);
    clientRes.writeHead(200, {
      'content-type':   'application/json',
      'content-length': Buffer.byteLength(body),
    });
    clientRes.end(body);
  });
}

// ── 流式响应处理 ──────────────────────────────

function handleStream(proxyRes, clientRes, providerFormat, clientFormat, originalModel, reqHeaders, onDone) {
  const sseHeaders = {
    'content-type':  'text/event-stream',
    'cache-control': 'no-cache',
    'connection':    'keep-alive',
  };
  if (clientFormat === 'anthropic') {
    sseHeaders['anthropic-version'] = '2023-06-01';
  }
  clientRes.writeHead(200, sseHeaders);

  // 相同格式：直接 pipe，保留完整 SSE 格式，打字机效果正常
  const isPassthrough =
    (providerFormat === 'anthropic' && clientFormat === 'anthropic') ||
    (providerFormat === 'openai'    && clientFormat === 'openai-chat');

  if (isPassthrough) {
    proxyRes.pipe(clientRes);
    proxyRes.on('end', () => {
      if (onDone) onDone();
    });
    return;
  }

  // 不同格式：逐行解析并转换 SSE 事件
  const transform = createStreamTransformer(providerFormat, clientFormat);
  let buffer = '';

  proxyRes.on('data', (chunk) => {
    buffer += chunk.toString('utf-8');
    const lines = buffer.split('\n');
    buffer = lines.pop();

    for (const line of lines) {
      const trimmed = line.trimEnd();
      if (!trimmed) { clientRes.write('\n'); continue; }
      for (const out of transform(trimmed)) {
        if (out) clientRes.write(out);
      }
    }
  });

  proxyRes.on('end', () => {
    if (buffer.trim()) {
      for (const out of transform(buffer.trim())) {
        if (out) clientRes.write(out);
      }
    }
    clientRes.end();
    if (onDone) onDone();
  });

  proxyRes.on('error', () => {
    clientRes.end();
    if (onDone) onDone();
  });
}

// ── 错误响应处理 ──────────────────────────────

function handleError(proxyRes, clientRes, statusCode, log) {
  let raw = '';
  proxyRes.on('data', c => raw += c);
  proxyRes.on('end', () => {
    log(`   错误响应: ${raw.slice(0, 300)}`);
    if (!clientRes.headersSent) {
      clientRes.writeHead(statusCode, { 'content-type': 'application/json' });
      clientRes.end(raw);
    }
  });
}
