import { randomUUID } from 'crypto';

// ─────────────────────────────────────────────
// 检测客户端格式
// ─────────────────────────────────────────────
export function detectClientFormat(url) {
  const path = url.split('?')[0];
  if (path.endsWith('/v1/messages'))           return 'anthropic';
  if (path.endsWith('/v1/chat/completions'))   return 'openai-chat';
  if (path.endsWith('/v1/responses'))          return 'openai-responses';
  return null; // 不拦截
}

// ─────────────────────────────────────────────
// → 内部格式（统一用 Anthropic-like 结构）
// ─────────────────────────────────────────────

/**
 * Anthropic 请求 → 内部格式（几乎原样保留）
 */
export function fromAnthropic(body) {
  return {
    model:     body.model,
    messages:  body.messages  || [],
    system:    body.system    || null,
    stream:    body.stream    || false,
    maxTokens: body.max_tokens || 4096,
    tools:     body.tools     || null,
    extra:     body,   // 保留原始字段（temperature 等）
  };
}

/**
 * OpenAI Chat 请求 → 内部格式
 */
export function fromOpenAIChat(body) {
  const messages = [];
  let system = null;

  for (const msg of (body.messages || [])) {
    if (msg.role === 'system') {
      system = typeof msg.content === 'string' ? msg.content : msg.content?.[0]?.text || '';
      continue;
    }
    messages.push({
      role:    msg.role,
      content: normalizeOpenAIContent(msg.content),
    });
  }

  return {
    model:     body.model,
    messages,
    system,
    stream:    body.stream     || false,
    maxTokens: body.max_tokens || 4096,
    tools:     body.tools      || null,
    extra:     body,
  };
}

/**
 * OpenAI Responses API 请求 → 内部格式
 */
export function fromOpenAIResponses(body) {
  const messages = [];

  // input 可以是字符串或数组
  const input = Array.isArray(body.input)
    ? body.input
    : (typeof body.input === 'string' ? [{ role: 'user', content: body.input }] : []);

  for (const item of input) {
    if (item.type === 'message' || item.role) {
      messages.push({
        role:    item.role,
        content: normalizeResponsesContent(item.content),
      });
    }
  }

  return {
    model:     body.model,
    messages,
    system:    body.instructions || null,
    stream:    body.stream        || false,
    maxTokens: body.max_output_tokens || 4096,
    tools:     body.tools         || null,
    extra:     body,
  };
}

// ─────────────────────────────────────────────
// 内部格式 → 上游 Provider 格式
// ─────────────────────────────────────────────

/**
 * 内部格式 → Anthropic 请求体
 */
export function toAnthropicBody(internal, targetModel) {
  const body = {
    model:      targetModel,
    messages:   internal.messages,
    max_tokens: internal.maxTokens,
    stream:     internal.stream,
  };
  if (internal.system) body.system = internal.system;
  if (internal.tools)  body.tools  = internal.tools;
  // 透传 temperature 等额外字段
  for (const k of ['temperature', 'top_p', 'top_k', 'stop_sequences', 'metadata']) {
    if (internal.extra[k] !== undefined) body[k] = internal.extra[k];
  }
  return body;
}

/**
 * 内部格式 → OpenAI Chat 请求体
 */
export function toOpenAIChatBody(internal, targetModel) {
  const messages = [];
  if (internal.system) messages.push({ role: 'system', content: internal.system });

  for (const msg of internal.messages) {
    messages.push({
      role:    msg.role,
      content: toOpenAIContent(msg.content),
    });
  }

  const body = {
    model:      targetModel,
    messages,
    max_tokens: internal.maxTokens,
    stream:     internal.stream,
  };
  if (internal.tools) body.tools = internal.tools;
  for (const k of ['temperature', 'top_p', 'frequency_penalty', 'presence_penalty']) {
    if (internal.extra[k] !== undefined) body[k] = internal.extra[k];
  }
  return body;
}

// ─────────────────────────────────────────────
// 响应转换（非流式）
// ─────────────────────────────────────────────

/**
 * Anthropic 响应 → 指定客户端格式
 */
export function convertAnthropicResponse(data, clientFormat, originalModel) {
  if (clientFormat === 'anthropic') return data;
  if (clientFormat === 'openai-chat') return anthropicToOpenAIChatResponse(data, originalModel);
  if (clientFormat === 'openai-responses') return anthropicToResponsesResponse(data, originalModel);
  return data;
}

/**
 * OpenAI Chat 响应 → 指定客户端格式
 */
export function convertOpenAIChatResponse(data, clientFormat, originalModel) {
  if (clientFormat === 'openai-chat') return data;
  if (clientFormat === 'anthropic') return openAIChatToAnthropicResponse(data, originalModel);
  if (clientFormat === 'openai-responses') return openAIChatToResponsesResponse(data, originalModel);
  return data;
}

// ─────────────────────────────────────────────
// SSE 流式转换
// ─────────────────────────────────────────────

/**
 * 创建流式 SSE 转换器
 * 返回 transform(line) 函数，把上游 SSE 行转成目标格式的行（数组）
 */
export function createStreamTransformer(providerFormat, clientFormat) {
  // 相同格式直接透传
  if (providerFormat === clientFormat ||
      (providerFormat === 'openai' && clientFormat === 'openai-chat')) {
    return (line) => [line];
  }

  const state = { msgId: `chatcmpl-${randomUUID().slice(0,8)}`, respId: `resp_${randomUUID().slice(0,8)}`, model: '', started: false, done: false };

  if (providerFormat === 'anthropic' && clientFormat === 'openai-chat') {
    return makeAnthropicToChat(state);
  }
  if (providerFormat === 'anthropic' && clientFormat === 'openai-responses') {
    return makeAnthropicToResponses(state);
  }
  if (providerFormat === 'openai' && clientFormat === 'anthropic') {
    return makeOpenAIToAnthropic(state);
  }
  if (providerFormat === 'openai' && clientFormat === 'openai-responses') {
    return makeOpenAIToResponses(state);
  }

  return (line) => [line]; // fallback
}

// ─────────────────────────────────────────────
// 内部工具函数
// ─────────────────────────────────────────────

function normalizeOpenAIContent(content) {
  if (typeof content === 'string') return [{ type: 'text', text: content }];
  if (!Array.isArray(content)) return [];
  return content.map(block => {
    if (block.type === 'text')       return { type: 'text', text: block.text };
    if (block.type === 'image_url')  return openAIImageToAnthropic(block.image_url);
    return block;
  });
}

function normalizeResponsesContent(content) {
  if (typeof content === 'string') return [{ type: 'text', text: content }];
  if (!Array.isArray(content)) return [];
  return content.map(block => {
    if (block.type === 'input_text')  return { type: 'text', text: block.text };
    if (block.type === 'input_image') return responsesImageToAnthropic(block);
    if (block.type === 'output_text') return { type: 'text', text: block.text };
    return block;
  });
}

function openAIImageToAnthropic(imageUrl) {
  const url = typeof imageUrl === 'string' ? imageUrl : imageUrl.url;
  const m = url.match(/^data:([^;]+);base64,(.+)$/);
  if (m) return { type: 'image', source: { type: 'base64', media_type: m[1], data: m[2] } };
  return { type: 'image', source: { type: 'url', url } };
}

function responsesImageToAnthropic(block) {
  if (block.image_url) return openAIImageToAnthropic(block.image_url);
  if (block.url)       return { type: 'image', source: { type: 'url', url: block.url } };
  return block;
}

function toOpenAIContent(content) {
  if (!Array.isArray(content)) return content;
  if (content.every(b => b.type === 'text')) {
    return content.map(b => b.text).join('\n');
  }
  return content.map(block => {
    if (block.type === 'text')  return { type: 'text', text: block.text };
    if (block.type === 'image') {
      const src = block.source;
      const url = src.type === 'base64' ? `data:${src.media_type};base64,${src.data}` : src.url;
      return { type: 'image_url', image_url: { url } };
    }
    return block;
  });
}

function extractText(content) {
  if (!Array.isArray(content)) return String(content || '');
  return content.filter(b => b.type === 'text').map(b => b.text).join('');
}

// ── 非流式响应转换 ──────────────────────────

function anthropicToOpenAIChatResponse(data, model) {
  return {
    id:      `chatcmpl-${randomUUID().slice(0,8)}`,
    object:  'chat.completion',
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{
      index:         0,
      message:       { role: 'assistant', content: extractText(data.content) },
      finish_reason: data.stop_reason === 'end_turn' ? 'stop' : (data.stop_reason || 'stop'),
    }],
    usage: {
      prompt_tokens:     data.usage?.input_tokens  || 0,
      completion_tokens: data.usage?.output_tokens || 0,
      total_tokens:      (data.usage?.input_tokens || 0) + (data.usage?.output_tokens || 0),
    },
  };
}

function anthropicToResponsesResponse(data, model) {
  const text = extractText(data.content);
  return {
    id:         `resp_${randomUUID().slice(0,8)}`,
    object:     'response',
    created_at: Math.floor(Date.now() / 1000),
    model,
    status:     'completed',
    output: [{
      type:    'message',
      id:      `msg_${randomUUID().slice(0,8)}`,
      role:    'assistant',
      content: [{ type: 'output_text', text }],
    }],
    usage: {
      input_tokens:  data.usage?.input_tokens  || 0,
      output_tokens: data.usage?.output_tokens || 0,
    },
  };
}

function openAIChatToAnthropicResponse(data, model) {
  const choice = data.choices?.[0] || {};
  const text   = choice.message?.content || '';
  return {
    id:           `msg_${randomUUID().slice(0,8)}`,
    type:         'message',
    role:         'assistant',
    model,
    content:      [{ type: 'text', text }],
    stop_reason:  choice.finish_reason === 'stop' ? 'end_turn' : choice.finish_reason,
    usage: {
      input_tokens:  data.usage?.prompt_tokens     || 0,
      output_tokens: data.usage?.completion_tokens || 0,
    },
  };
}

function openAIChatToResponsesResponse(data, model) {
  const choice = data.choices?.[0] || {};
  const text   = choice.message?.content || '';
  return {
    id:         `resp_${randomUUID().slice(0,8)}`,
    object:     'response',
    created_at: Math.floor(Date.now() / 1000),
    model,
    status:     'completed',
    output: [{
      type: 'message', id: `msg_${randomUUID().slice(0,8)}`, role: 'assistant',
      content: [{ type: 'output_text', text }],
    }],
    usage: {
      input_tokens:  data.usage?.prompt_tokens     || 0,
      output_tokens: data.usage?.completion_tokens || 0,
    },
  };
}

// ── 流式 SSE 转换器工厂 ──────────────────────

function parseSseLine(line) {
  if (line.startsWith('data: ')) {
    const raw = line.slice(6);
    if (raw === '[DONE]') return { done: true };
    try { return { data: JSON.parse(raw) }; } catch { return null; }
  }
  return null;
}

function sse(event, data) {
  return `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

function chatDelta(id, model, delta, finishReason = null) {
  return `data: ${JSON.stringify({
    id, object: 'chat.completion.chunk',
    created: Math.floor(Date.now() / 1000), model,
    choices: [{ index: 0, delta, finish_reason: finishReason }],
  })}\n\n`;
}

function makeAnthropicToChat(s) {
  return (line) => {
    const p = parseSseLine(line);
    if (!p) return [];
    if (p.done) return ['data: [DONE]\n\n'];
    const { data } = p;
    if (!data) return [];

    if (data.type === 'message_start') {
      s.model = data.message?.model || '';
      return [chatDelta(s.msgId, s.model, { role: 'assistant' })];
    }
    if (data.type === 'content_block_delta' && data.delta?.type === 'text_delta') {
      return [chatDelta(s.msgId, s.model, { content: data.delta.text })];
    }
    if (data.type === 'message_delta' && data.delta?.stop_reason) {
      const fr = data.delta.stop_reason === 'end_turn' ? 'stop' : data.delta.stop_reason;
      return [chatDelta(s.msgId, s.model, {}, fr), 'data: [DONE]\n\n'];
    }
    return [];
  };
}

function makeAnthropicToResponses(s) {
  const itemId = `msg_${randomUUID().slice(0,8)}`;
  return (line) => {
    const p = parseSseLine(line);
    if (!p) return [];
    if (p.done) return [];
    const { data } = p;
    if (!data) return [];

    if (data.type === 'message_start') {
      s.model = data.message?.model || '';
      return [
        sse('response.created', { type: 'response.created', response: { id: s.respId, object: 'response', status: 'in_progress', model: s.model, output: [] } }),
        sse('response.output_item.added', { type: 'response.output_item.added', output_index: 0, item: { type: 'message', id: itemId, role: 'assistant', content: [] } }),
        sse('response.content_part.added', { type: 'response.content_part.added', item_id: itemId, output_index: 0, content_index: 0, part: { type: 'output_text', text: '' } }),
      ];
    }
    if (data.type === 'content_block_delta' && data.delta?.type === 'text_delta') {
      return [sse('response.output_text.delta', { type: 'response.output_text.delta', item_id: itemId, output_index: 0, content_index: 0, delta: data.delta.text })];
    }
    if (data.type === 'message_stop') {
      return [
        sse('response.output_text.done',  { type: 'response.output_text.done', item_id: itemId, output_index: 0, content_index: 0, text: '' }),
        sse('response.output_item.done',  { type: 'response.output_item.done', output_index: 0, item: { type: 'message', id: itemId, role: 'assistant', content: [{ type: 'output_text', text: '' }] } }),
        sse('response.completed',         { type: 'response.completed', response: { id: s.respId, object: 'response', status: 'completed', model: s.model } }),
      ];
    }
    return [];
  };
}

function makeOpenAIToAnthropic(s) {
  let blockStarted = false;
  return (line) => {
    const p = parseSseLine(line);
    if (!p) return [];
    if (p.done) {
      return [
        `event: message_stop\ndata: {"type":"message_stop"}\n\n`,
      ];
    }
    const { data } = p;
    if (!data) return [];
    const delta   = data.choices?.[0]?.delta || {};
    const finish  = data.choices?.[0]?.finish_reason;
    const out     = [];

    if (!s.started) {
      s.model = data.model || '';
      out.push(`event: message_start\ndata: ${JSON.stringify({ type: 'message_start', message: { id: `msg_${randomUUID().slice(0,8)}`, type: 'message', role: 'assistant', model: s.model, content: [], stop_reason: null, usage: { input_tokens: 0, output_tokens: 0 } } })}\n\n`);
      s.started = true;
    }
    if (delta.content !== undefined && !blockStarted) {
      out.push(`event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n`);
      blockStarted = true;
    }
    if (delta.content) {
      out.push(`event: content_block_delta\ndata: ${JSON.stringify({ type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: delta.content } })}\n\n`);
    }
    if (finish) {
      out.push(`event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n`);
      out.push(`event: message_delta\ndata: ${JSON.stringify({ type: 'message_delta', delta: { stop_reason: finish === 'stop' ? 'end_turn' : finish, stop_sequence: null }, usage: { output_tokens: 0 } })}\n\n`);
    }
    return out;
  };
}

function makeOpenAIToResponses(s) {
  const itemId = `msg_${randomUUID().slice(0,8)}`;
  return (line) => {
    const p = parseSseLine(line);
    if (!p) return [];
    if (p.done) {
      return [
        sse('response.output_item.done', { type: 'response.output_item.done', output_index: 0, item: { type: 'message', id: itemId, role: 'assistant', content: [] } }),
        sse('response.completed', { type: 'response.completed', response: { id: s.respId, status: 'completed', model: s.model } }),
      ];
    }
    const { data } = p;
    if (!data) return [];
    const delta  = data.choices?.[0]?.delta || {};
    const finish = data.choices?.[0]?.finish_reason;
    const out    = [];

    if (!s.started) {
      s.model = data.model || '';
      out.push(sse('response.created', { type: 'response.created', response: { id: s.respId, object: 'response', status: 'in_progress', model: s.model, output: [] } }));
      out.push(sse('response.output_item.added', { type: 'response.output_item.added', output_index: 0, item: { type: 'message', id: itemId, role: 'assistant', content: [] } }));
      out.push(sse('response.content_part.added', { type: 'response.content_part.added', item_id: itemId, output_index: 0, content_index: 0, part: { type: 'output_text', text: '' } }));
      s.started = true;
    }
    if (delta.content) {
      out.push(sse('response.output_text.delta', { type: 'response.output_text.delta', item_id: itemId, output_index: 0, content_index: 0, delta: delta.content }));
    }
    if (finish) {
      out.push(sse('response.output_text.done', { type: 'response.output_text.done', item_id: itemId, output_index: 0, content_index: 0, text: '' }));
    }
    return out;
  };
}
