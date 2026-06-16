import http  from 'http';
import https from 'https';
import { createStreamTransformer, convertAnthropicResponse, convertOpenAIChatResponse } from './converter.js';

// 流式传输活跃超时默认值：超过此时间没有新数据则终止连接（可由配置覆盖）
const DEFAULT_STREAM_ACTIVITY_TIMEOUT = 60_000; // 60 秒

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
 *   streamActivityTimeout - 流式活跃超时（毫秒），超过此时间无新数据则主动断开
 * @returns {Promise} 响应完成后 resolve（释放队列 slot）
 */
export function forwardRequest(opts) {
  const { clientReq, clientRes, upstreamBody, provider, clientFormat, originalModel, isStreaming, log, startTime, timeout = 60000, streamActivityTimeout = DEFAULT_STREAM_ACTIVITY_TIMEOUT } = opts;

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
    // 流式活跃超时主动断开上游时置位，供 proxyReq error handler 识别，避免把预期内的保护动作误报为转发失败
    const streamCtx = { aborted: false };

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
      }, log, streamCtx, streamActivityTimeout);
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
      // 流式活跃超时主动断开上游：属于预期内的保护动作，handleStream 已打印明确日志并完成客户端收尾，这里不再重复误报
      if (streamCtx.aborted) {
        resolve();
        return;
      }
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
      if (!clientRes.headersSent) {
        clientRes.writeHead(502, { 'content-type': 'application/json' });
        clientRes.end(JSON.stringify({ error: { type: 'parse_error', message: `上游响应解析失败: ${e.message}` } }));
      }
      return;
    }

    const result = providerFormat === 'anthropic'
      ? convertAnthropicResponse(data, clientFormat, originalModel)
      : convertOpenAIChatResponse(data, clientFormat, originalModel);

    const body = JSON.stringify(result);
    if (!clientRes.headersSent) {
      clientRes.writeHead(200, {
        'content-type':   'application/json',
        'content-length': Buffer.byteLength(body),
      });
    }
    clientRes.end(body);
  });

  // 上游响应流出错时，确保客户端收到响应，防止队列 slot 泄漏
  proxyRes.on('error', (err) => {
    log(`上游响应流错误: ${err.message}`);
    if (!clientRes.headersSent) {
      clientRes.writeHead(502, { 'content-type': 'application/json' });
      clientRes.end(JSON.stringify({ error: { type: 'upstream_error', message: err.message } }));
    } else if (!clientRes.writableEnded) {
      clientRes.end();
    }
  });
}

// ── 流式响应处理 ──────────────────────────────

function handleStream(proxyRes, clientRes, providerFormat, clientFormat, originalModel, reqHeaders, onDone, log, streamCtx, streamActivityTimeout = DEFAULT_STREAM_ACTIVITY_TIMEOUT) {
  let done = false;
  function finish() {
    if (done) return;
    done = true;
    clearTimeout(activityTimer);
    if (onDone) onDone();
  }

  // 向客户端补发一个合规的 SSE 收尾事件，避免上游中断时客户端只收到截断流
  function gracefulSseClose(message) {
    if (clientRes.writableEnded) return;
    try {
      if (clientFormat === 'anthropic') {
        // Anthropic 流式协议：中途异常以 error 事件结束
        clientRes.write(`event: error\n`);
        clientRes.write(`data: ${JSON.stringify({ type: 'error', error: { type: 'timeout_error', message } })}\n\n`);
      } else {
        // OpenAI Chat / Responses：以 [DONE] 标记流结束
        clientRes.write('data: [DONE]\n\n');
      }
    } catch { /* 客户端可能已断开，忽略写入异常 */ }
    clientRes.end();
  }

  // 活跃超时：上游停止发送数据超过指定时间未推送新数据，主动断开并优雅收尾
  function onActivityTimeout() {
    if (done) return;
    // 标记为主动断开，供 proxyReq error handler 识别，避免误报为"转发失败"
    if (streamCtx) streamCtx.aborted = true;
    if (log) log(`上游流式响应超时，已主动断开 (${streamActivityTimeout / 1000}秒无数据)`);
    gracefulSseClose(`上游流式响应超时 (${streamActivityTimeout / 1000}秒无数据)`);
    proxyRes.destroy();
    finish();
  }

  let activityTimer = setTimeout(onActivityTimeout, streamActivityTimeout);
  activityTimer.unref();

  function resetActivityTimer() {
    clearTimeout(activityTimer);
    activityTimer = setTimeout(onActivityTimeout, streamActivityTimeout);
    activityTimer.unref();
  }

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

    proxyRes.on('data', resetActivityTimer);

    proxyRes.on('end', finish);

    // 上游流出错时也要释放队列 slot
    proxyRes.on('error', (err) => {
      if (!clientRes.writableEnded) clientRes.end();
      finish();
    });

    // 客户端断开时清理上游连接，释放队列 slot
    clientRes.on('close', () => {
      proxyRes.destroy();
      finish();
    });

    return;
  }

  // 不同格式：逐行解析并转换 SSE 事件
  const transform = createStreamTransformer(providerFormat, clientFormat);
  let buffer = '';

  proxyRes.on('data', (chunk) => {
    resetActivityTimer();
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
    if (!clientRes.writableEnded) clientRes.end();
    finish();
  });

  proxyRes.on('error', (err) => {
    if (!clientRes.writableEnded) clientRes.end();
    finish();
  });

  // 客户端断开时清理上游连接，释放队列 slot
  clientRes.on('close', () => {
    proxyRes.destroy();
    finish();
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
  // 上游错误响应流本身出错时，确保客户端能收到响应，及时回收 socket
  proxyRes.on('error', (err) => {
    log(`   错误响应流异常: ${err.message}`);
    if (!clientRes.headersSent) {
      clientRes.writeHead(502, { 'content-type': 'application/json' });
      clientRes.end(JSON.stringify({ error: { type: 'upstream_error', message: err.message } }));
    } else if (!clientRes.writableEnded) {
      clientRes.end();
    }
  });
}
