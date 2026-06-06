#!/usr/bin/env node
/**
 * ai-gateway
 * 轻量本地 API Gateway，支持 Claude Code / Claude Desktop / Codex
 * 接受 Anthropic / OpenAI Chat / OpenAI Responses 三种格式输入
 */

import http from 'http';
import { loadConfig, initConfig } from './lib/config.js';
import { matchRoute, resolveApiKey } from './lib/router.js';
import { translateImages, hasImages } from './lib/vision.js';
import { forwardRequest } from './lib/proxy.js';
import {
  detectClientFormat,
  fromAnthropic, fromOpenAIChat, fromOpenAIResponses,
  toAnthropicBody, toOpenAIChatBody,
} from './lib/converter.js';
import { initCache, cleanupCache, clearAllCache, getCacheStats, closeCache } from './lib/cache.js';
import { enqueue, getQueueStatus } from './lib/queue.js';

// ── CLI 命令 ────────────────────────────────
if (process.argv.includes('--init')) { initConfig(); process.exit(0); }

// ── 统一缓存管理命令 ──────────────────────────
// 用法: node gateway.js cache [stats|clear|cleanup]
const cacheCmd = process.argv[2] === 'cache' ? process.argv[3] : null;
const legacyCmd = process.argv.includes('--cache-stats') ? 'stats'
  : process.argv.includes('--clear-cache') ? 'clear'
  : process.argv.includes('--cleanup-cache') ? 'cleanup'
  : null;

const cacheAction = cacheCmd || legacyCmd;

if (cacheAction) {
  (async () => {
    await initCache();

    switch (cacheAction) {
      case 'stats': {
        const stats = await getCacheStats();
        console.log('📊 缓存统计:');
        console.log(`  记录数: ${stats.total}`);
        if (stats.total > 0) {
          console.log(`  缓存大小: ${formatSize(stats.contentSize)}`);
          console.log(`  数据库大小: ${formatSize(stats.dbSize)}`);
        }
        break;
      }
      case 'clear': {
        const deleted = await clearAllCache();
        console.log(`✅ 已清空缓存，删除 ${deleted} 条记录`);
        break;
      }
      case 'cleanup': {
        const result = await cleanupCache();
        console.log(`🧹 缓存清理完成:`);
        console.log(`  删除: ${result.deleted} 条`);
        console.log(`  剩余: ${result.remaining} 条`);
        break;
      }
      default:
        console.log('❌ 未知的缓存命令');
        console.log('用法: node gateway.js cache [stats|clear|cleanup]');
    }

    closeCache();
    process.exit(0);
  })();
}

// ── 加载配置 ────────────────────────────────
const config = loadConfig();

// ── 初始化缓存 ──────────────────────────────
await initCache();
// 启动时自动清理过期缓存
const cacheCleanupResult = await cleanupCache(
  config.cache?.maxAgeDays || 7,
  config.cache?.maxRecords || 1000
);
if (cacheCleanupResult.deleted > 0) {
  process.stderr.write(`[ai-gateway] 缓存清理: 删除 ${cacheCleanupResult.deleted} 条过期记录\n`);
}

// ── 工具函数 ────────────────────────────────
function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

// ── 日志 ────────────────────────────────────
let requestCounter = 0;
function generateRequestId() { return `r${++requestCounter}`.padStart(6, 'r0'); }

function getBeijingTime() {
  return new Date().toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: 3,
    hour12: false
  }).replace(/\//g, '-');
}

function log(reqId, msg) {
  process.stderr.write(`[${getBeijingTime()}] [${reqId}] ${msg}\n`);
}
function mask(v) { if (!v) return '(未配置)'; if (v.length <= 8) return '****'; return v.slice(0,4) + '****' + v.slice(-4); }

// ── 工具函数 ────────────────────────────────

function collectBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', c => chunks.push(c));
    req.on('end',  () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendError(res, statusCode, message) {
  if (res.headersSent) return;
  const body = JSON.stringify({ error: { type: 'gateway_error', message } });
  res.writeHead(statusCode, { 'content-type': 'application/json' });
  res.end(body);
}

// ── 主请求处理器 ────────────────────────────

const server = http.createServer(async (clientReq, clientRes) => {
  const urlPath = (clientReq.url || '').split('?')[0];

  // 健康检测（Claude Code/Desktop 启动时发 HEAD /）
  if (clientReq.method === 'HEAD') {
    clientRes.writeHead(200);
    clientRes.end();
    return;
  }

  // 健康检查端点
  if (urlPath === '/health' && clientReq.method === 'GET') {
    const queueStatuses = {};
    for (const name of Object.keys(config.providers)) {
      queueStatuses[name] = getQueueStatus(name);
    }

    const cacheStats = await getCacheStats();

    const health = {
      status: 'ok',
      uptime: Math.floor(process.uptime()),
      timeout: config.timeout,
      queues: queueStatuses,
      cache: {
        total: cacheStats.total,
        contentSize: cacheStats.contentSize,
      },
    };

    const body = JSON.stringify(health, null, 2);
    clientRes.writeHead(200, {
      'content-type': 'application/json',
      'content-length': Buffer.byteLength(body),
    });
    clientRes.end(body);
    return;
  }

  // 识别客户端格式
  const clientFormat = detectClientFormat(urlPath);
  if (!clientFormat) {
    // 其他路径直接 404
    sendError(clientRes, 404, `未知端点: ${urlPath}`);
    return;
  }

  // 生成请求 ID
  const reqId = generateRequestId();
  const startTime = Date.now();

  log(reqId, `→ ${clientReq.method} ${urlPath} [${clientFormat}]`);

  // 读取请求体
  let rawBody;
  try {
    rawBody = await collectBody(clientReq);
  } catch (e) {
    sendError(clientRes, 400, `读取请求失败: ${e.message}`);
    return;
  }

  // 解析 JSON
  let body;
  try {
    body = JSON.parse(rawBody.toString('utf-8'));
  } catch (e) {
    sendError(clientRes, 400, `请求体 JSON 解析失败: ${e.message}`);
    return;
  }

  // 规范化为内部格式
  let internal;
  if (clientFormat === 'anthropic')         internal = fromAnthropic(body);
  else if (clientFormat === 'openai-chat')  internal = fromOpenAIChat(body);
  else                                      internal = fromOpenAIResponses(body);

  const originalModel = internal.model;

  // 路由匹配
  const matched = matchRoute(originalModel, config);
  if (!matched) {
    sendError(clientRes, 400, `没有匹配 model "${originalModel}" 的路由规则`);
    return;
  }

  const { provider, targetModel, visionProvider } = matched;

  // 解析 API Key
  provider.apiKey = resolveApiKey(provider, clientReq.headers);
  if (visionProvider) visionProvider.apiKey = resolveApiKey(visionProvider, clientReq.headers);

  // 检测是否需要调用视觉模型（配置了 vision 且消息包含图片）
  const needVision = visionProvider && hasImages(internal.messages);

  // 精简的请求开始日志（调用视觉模型时只显示视觉模型）
  const displayModel = needVision ? visionProvider.model : targetModel;
  log(reqId, `→ ${originalModel} → ${displayModel} [${internal.messages.length} msgs, stream=${internal.stream}]`);

  // 图片翻译（仅在路由配置了 vision 且消息包含图片时）
  if (needVision) {
    try {
      internal.messages = await translateImages(internal.messages, visionProvider, log.bind(null, reqId));
    } catch (e) {
      log(reqId, `  图片翻译异常: ${e.message}`);
    }
  }

  // 转换为上游格式
  const upstreamBody = provider.format === 'anthropic'
    ? toAnthropicBody(internal, targetModel)
    : toOpenAIChatBody(internal, targetModel);

  // 获取队列状态
  const queueStatus = getQueueStatus(provider.name);
  const maxConcurrent = provider.maxConcurrent || 5;
  const maxPerSecond = provider.maxPerSecond || 0;

  // 调试日志：显示速率限制配置
  if (maxPerSecond > 0) {
    log(reqId, `  速率限制: ${maxPerSecond}/秒`);
  }

  // 只有在有排队时才显示队列状态
  if (queueStatus.queued > 0 || queueStatus.running >= maxConcurrent) {
    log(reqId, `  队列: ${queueStatus.running}/${maxConcurrent} 运行, ${queueStatus.queued} 等待`);
  }

  // 通过队列转发请求（并发控制 + 速率限制）
  enqueue(provider.name, maxConcurrent, () => {
    return forwardRequest({
      clientReq,
      clientRes,
      upstreamBody,
      provider,
      clientFormat,
      originalModel,
      isStreaming: internal.stream,
      log: log.bind(null, reqId),
      startTime,
      timeout: config.timeout,
    });
  }, maxPerSecond).catch(err => {
    log(reqId, `  队列处理异常: ${err.message}`);
  });
});

// ── 错误处理 ────────────────────────────────
server.on('error', (err) => {
  if (err.code === 'EADDRINUSE') {
    log('system', `端口 ${config.port} 已被占用，请修改配置或关闭占用进程`);
  } else {
    log('system', `服务器错误: ${err.message}`);
  }
  closeCache();
  process.exit(1);
});

// 优雅退出
process.on('SIGINT', () => {
  log('system', '收到 SIGINT 信号，正在关闭...');
  closeCache();
  process.exit(0);
});

process.on('SIGTERM', () => {
  log('system', '收到 SIGTERM 信号，正在关闭...');
  closeCache();
  process.exit(0);
});

// ── 启动 ─────────────────────────────────────
server.listen(config.port, config.host, async () => {
  const cacheStats = await getCacheStats();

  log('system', '═══════════════════════════════════════════');
  log('system', '  ai-gateway 启动成功');
  log('system', '───────────────────────────────────────────');
  log('system', `  监听地址 : http://${config.host}:${config.port}`);
  log('system', `  配置文件 : ${config._path}`);
  log('system', `  Providers:`);
  for (const [name, p] of Object.entries(config.providers)) {
    log('system', `    ${name}: ${p.baseUrl} [${p.format}] key=${mask(p.apiKey)} 并发=${p.maxConcurrent || 5}`);
  }
  log('system', `  Routes  : ${config.routes.length} 条`);
  for (const r of config.routes) {
    const vis = r.vision ? ` + vision(${r.vision.model})` : '';
    log('system', `    ${r.match.padEnd(30)} → ${r.provider}/${r.model}${vis}`);
  }
  log('system', '───────────────────────────────────────────');
  if (cacheStats.total > 0) {
    log('system', `  图片缓存 : ${cacheStats.total} 条 (${formatSize(cacheStats.contentSize)})`);
  } else {
    log('system', `  图片缓存 : 无`);
  }
  log('system', `  请求超时 : ${config.timeout / 1000} 秒`);
  log('system', '───────────────────────────────────────────');
  log('system', '  接受格式: Anthropic · OpenAI Chat · OpenAI Responses');
  log('system', '  健康检查: GET /health');
  log('system', '═══════════════════════════════════════════');
});
