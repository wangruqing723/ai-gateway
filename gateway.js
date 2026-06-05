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

// ── CLI 命令 ────────────────────────────────
if (process.argv.includes('--init')) { initConfig(); process.exit(0); }

// ── 加载配置 ────────────────────────────────
const config = loadConfig();

// ── 日志 ────────────────────────────────────
function log(msg) { process.stderr.write(`[ai-gateway] ${msg}\n`); }
function mask(v)  { if (!v) return '(未配置)'; if (v.length <= 8) return '****'; return v.slice(0,4) + '****' + v.slice(-4); }

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

  // 识别客户端格式
  const clientFormat = detectClientFormat(urlPath);
  if (!clientFormat) {
    // 其他路径直接 404
    sendError(clientRes, 404, `未知端点: ${urlPath}`);
    return;
  }

  log(`→ ${clientReq.method} ${urlPath} [${clientFormat}]`);

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

  log(`  model: ${originalModel} → ${targetModel} (${provider.name} / ${provider.format})`);

  // 图片翻译（仅在路由配置了 vision 且消息包含图片时）
  if (visionProvider && hasImages(internal.messages)) {
    log(`  检测到图片，调用 ${visionProvider.model} 识别...`);
    try {
      internal.messages = await translateImages(internal.messages, visionProvider, log);
    } catch (e) {
      log(`  图片翻译异常: ${e.message}`);
    }
  }

  // 转换为上游格式
  const upstreamBody = provider.format === 'anthropic'
    ? toAnthropicBody(internal, targetModel)
    : toOpenAIChatBody(internal, targetModel);

  // 转发
  forwardRequest({
    clientReq,
    clientRes,
    upstreamBody,
    provider,
    clientFormat,
    originalModel,
    isStreaming: internal.stream,
    log,
  });
});

// ── 错误处理 ────────────────────────────────
server.on('error', (err) => {
  if (err.code === 'EADDRINUSE') {
    log(`端口 ${config.port} 已被占用，请修改配置或关闭占用进程`);
  } else {
    log(`服务器错误: ${err.message}`);
  }
  process.exit(1);
});

// ── 启动 ─────────────────────────────────────
server.listen(config.port, '127.0.0.1', () => {
  log('═══════════════════════════════════════════');
  log('  ai-gateway 启动成功');
  log('───────────────────────────────────────────');
  log(`  监听地址 : http://127.0.0.1:${config.port}`);
  log(`  配置文件 : ${config._path}`);
  log(`  Providers:`);
  for (const [name, p] of Object.entries(config.providers)) {
    log(`    ${name}: ${p.baseUrl} [${p.format}] key=${mask(p.apiKey)}`);
  }
  log(`  Routes  : ${config.routes.length} 条`);
  for (const r of config.routes) {
    const vis = r.vision ? ` + vision(${r.vision.model})` : '';
    log(`    ${r.match.padEnd(30)} → ${r.provider}/${r.model}${vis}`);
  }
  log('───────────────────────────────────────────');
  log('  接受格式: Anthropic · OpenAI Chat · OpenAI Responses');
  log('═══════════════════════════════════════════');
});
