import http  from 'http';
import https from 'https';
import { createStreamTransformer, convertAnthropicResponse, convertOpenAIChatResponse } from './converter.js';

/**
 * 转发请求到上游 provider，处理流式/非流式响应，并转换回客户端格式
 *
 * @param {object} opts
 *   clientReq     - 原始客户端请求对象（用于获取头）
 *   clientRes     - 原始客户端响应对象
 *   upstreamBody  - 已转换好的上游请求 JSON 对象
 *   provider      - { baseUrl, apiKey, format }
 *   clientFormat  - 'anthropic' | 'openai-chat' | 'openai-responses'
 *   originalModel - 客户端发来的原始模型名（用于响应里填回去）
 *   isStreaming    - 是否流式
 *   log           - 日志函数
 */
export function forwardRequest(opts) {
  const { clientReq, clientRes, upstreamBody, provider, clientFormat, originalModel, isStreaming, log } = opts;

  const upstreamUrl   = new URL(provider.baseUrl.startsWith('http') ? provider.baseUrl : `https://${provider.baseUrl}`);
  const basePath      = upstreamUrl.pathname.replace(/\/$/, '').replace(/\/v1$/, '');
  const isHttps       = upstreamUrl.protocol === 'https:';
  const upstreamPath  = basePath + (provider.format === 'anthropic' ? '/v1/messages' : '/v1/chat/completions');
  const bodyStr       = JSON.stringify(upstreamBody);

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

  const proxyReq = (isHttps ? https : http).request({
    hostname: upstreamUrl.hostname,
    port:     upstreamUrl.port || (isHttps ? 443 : 80),
    path:     upstreamPath,
    method:   'POST',
    headers,
  }, (proxyRes) => {
    log(`← HTTP ${proxyRes.statusCode} (${provider.format} → ${clientFormat})`);

    if (proxyRes.statusCode >= 400) {
      handleError(proxyRes, clientRes, proxyRes.statusCode, log);
      return;
    }

    if (isStreaming) {
      handleStream(proxyRes, clientRes, provider.format, clientFormat, originalModel, clientReq.headers);
    } else {
      handleResponse(proxyRes, clientRes, provider.format, clientFormat, originalModel, log);
    }
  });

  proxyReq.on('error', (err) => {
    log(`转发失败: ${err.message}`);
    if (!clientRes.headersSent) {
      clientRes.writeHead(502, { 'content-type': 'application/json' });
      clientRes.end(JSON.stringify({ error: { type: 'proxy_error', message: err.message } }));
    }
  });

  proxyReq.write(bodyStr);
  proxyReq.end();
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

    let result;
    if (providerFormat === 'anthropic') {
      result = convertAnthropicResponse(data, clientFormat, originalModel);
    } else {
      result = convertOpenAIChatResponse(data, clientFormat, originalModel);
    }

    const body = JSON.stringify(result);
    clientRes.writeHead(200, {
      'content-type':   'application/json',
      'content-length': Buffer.byteLength(body),
    });
    clientRes.end(body);
  });
}

// ── 流式响应处理 ──────────────────────────────

function handleStream(proxyRes, clientRes, providerFormat, clientFormat, originalModel, reqHeaders) {
  // 设置 SSE 响应头
  const sseHeaders = {
    'content-type':  'text/event-stream',
    'cache-control': 'no-cache',
    'connection':    'keep-alive',
  };
  // Anthropic 客户端需要额外头
  if (clientFormat === 'anthropic') {
    sseHeaders['anthropic-version'] = '2023-06-01';
  }
  clientRes.writeHead(200, sseHeaders);

  const transform = createStreamTransformer(providerFormat, clientFormat);
  let buffer = '';

  proxyRes.on('data', (chunk) => {
    buffer += chunk.toString('utf-8');
    const lines = buffer.split('\n');
    buffer = lines.pop(); // 保留不完整的最后一行

    for (const line of lines) {
      const trimmed = line.trimEnd();
      if (!trimmed) {
        clientRes.write('\n');
        continue;
      }
      const outLines = transform(trimmed);
      for (const out of outLines) {
        if (out) clientRes.write(out);
      }
    }
  });

  proxyRes.on('end', () => {
    if (buffer.trim()) {
      const outLines = transform(buffer.trim());
      for (const out of outLines) {
        if (out) clientRes.write(out);
      }
    }
    clientRes.end();
  });

  proxyRes.on('error', () => clientRes.end());
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
