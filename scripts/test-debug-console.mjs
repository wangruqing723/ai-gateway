import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import test from 'node:test';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const members = name => new Function(`return ({\n${readFileSync(resolve(root, name), 'utf8')}\n})`)();
const debug = members('cmd/gateway/web/src/app/18-debug-console.js.part');
const logs = members('cmd/gateway/web/src/app/05-logs.js.part');

function appFor(overrides = {}) {
    return Object.assign({}, debug, {
        _debugConfigSnapshot: {
            providers: {
                saved: { enabled: true },
                disabled: { enabled: false }
            },
            routes: [{
                match: 'claude-*',
                targets: [{ provider: 'saved', model: 'upstream-model' }, { provider: 'disabled' }]
            }]
        },
        debugMode: 'provider',
        debugClientFormat: 'anthropic',
        debugProvider: 'saved',
        debugRequestView: 'simple',
        debugSimple: { model: 'claude-test', system: 'system text', message: 'hello', stream: true, maxTokens: '' },
        ...overrides
    });
}

test('简易表单按三种客户端格式构造请求体，默认流式且不传 max tokens', () => {
    const app = appFor();
    const anthropic = app.debugBuildRequest();
    assert.equal(anthropic.valid, true);
    assert.deepEqual(anthropic.payload.body, {
        model: 'claude-test', stream: true,
        messages: [{ role: 'user', content: 'hello' }], system: 'system text'
    });
    assert.equal(Object.hasOwn(anthropic.payload.body, 'max_tokens'), false);

    app.debugClientFormat = 'openai-chat';
    const chat = app.debugBuildRequest().payload.body;
    assert.deepEqual(chat.messages, [
        { role: 'system', content: 'system text' },
        { role: 'user', content: 'hello' }
    ]);
    assert.deepEqual(chat.stream_options, { include_usage: true });
    assert.equal(Object.hasOwn(chat, 'max_tokens'), false);

    app.debugClientFormat = 'openai-responses';
    app.debugSimple.maxTokens = '123';
    const responses = app.debugBuildRequest().payload.body;
    assert.deepEqual(responses, {
        model: 'claude-test', stream: true, input: 'hello', instructions: 'system text', max_output_tokens: 123
    });
});

test('路由与指定候选请求只带适用字段，通配路由要求具体模型', () => {
    const app = appFor({ debugMode: 'route', debugRouteIndex: 0, debugSimple: {
        model: 'claude-*', system: '', message: 'hello', stream: true, maxTokens: ''
    } });
    assert.match(app.debugRequestValidation().error, /具体模型名/);
    app.debugSimple.model = 'claude-test';
    assert.deepEqual(app.debugBuildRequest().payload, {
        mode: 'route', clientFormat: 'anthropic', routeIndex: 0, routeMatch: 'claude-*',
        body: { model: 'claude-test', stream: true, messages: [{ role: 'user', content: 'hello' }] }
    });

    app.debugMode = 'target';
    app.debugTargetIndex = 1;
    const target = app.debugBuildRequest();
    assert.equal(target.valid, true);
    assert.equal(target.payload.targetIndex, 1);
    assert.equal(target.payload.targetProvider, 'disabled');
    assert.equal(Object.hasOwn(target.payload, 'provider'), false);
});

test('编辑后的原始 JSON 是实际 body；对象和格式切换保留草稿', () => {
    const raw = '{\n  "model": "raw-model",\n  "messages": [],\n  "custom": "keep exactly"\n}';
    const app = appFor({ debugRequestView: 'raw', debugRawJSON: raw, debugRawEdited: true });
    app.loadDebugProviderModels = () => {};
    app.debugChangeClientFormat('openai-chat');
    app.debugChangeProvider('disabled');
    app.debugChangeRoute('0');
    assert.equal(app.debugRawJSON, raw);
    assert.equal(app.debugBuildRequest().payload.body.custom, 'keep exactly');
    assert.equal(app.debugBuildRequest().payload.body.model, 'raw-model');

    app.debugRawJSON = '{"model":';
    assert.equal(app.debugRequestValidation().valid, false);
    assert.match(app.debugRequestValidation().error, /合法的 JSON/);
});

test('SSE 解析器支持 CRLF 跨片段、分行 data 与没有末尾换行的事件', () => {
    const app = appFor();
    const parser = app.debugCreateSSEParser();
    assert.deepEqual(app.debugFeedSSE(parser, 'event: message\r'), []);
    assert.deepEqual(app.debugFeedSSE(parser, '\ndata: 第一行\r\ndata: 第二行\r'), []);
    assert.deepEqual(app.debugFeedSSE(parser, '\n\r\n'), [{ event: 'message', data: '第一行\n第二行' }]);

    const tail = app.debugCreateSSEParser();
    assert.deepEqual(app.debugFeedSSE(tail, 'event: done\ndata: {"ok":true}', true), [
        { event: 'done', data: '{"ok":true}' }
    ]);
});

test('调试日志筛选会写入查询参数，重置会清空新旧过滤条件', () => {
    const app = Object.assign({}, logs, {
        logFilters: {
            status: '5xx', provider: 'saved', stream: 'true', model: 'claude', q: 'id',
            attemptProvider: 'saved', attemptStatus: '500', attemptOutcome: 'failure', debug: 'only'
        },
        logAdvancedFiltersOpen: true,
        loadCount: 0,
        loadRequestLogs() { this.loadCount++; }
    });
    assert.deepEqual(app.logFilterQuery('debug', app.logFilters.debug), ['debug', 'only']);
    app.resetLogFilters();
    assert.deepEqual(app.logFilters, {
        status: '', provider: '', stream: '', model: '', q: '',
        attemptProvider: '', attemptStatus: '', attemptOutcome: '', debug: ''
    });
    assert.equal(app.logAdvancedFiltersOpen, false);
    assert.equal(app.loadCount, 1);
});

test('配置选项来自保存快照，仅正文通过安全 Markdown 入口渲染', () => {
    const app = appFor({ config: { providers: { saved: {}, unsavedDraft: {} }, routes: [{ match: 'draft-*' }] } });
    assert.deepEqual(app.debugSavedProviders().map(entry => entry.name), ['saved', 'disabled']);
    assert.deepEqual(app.debugSavedRoutes().map(entry => entry.route.match), ['claude-*']);

    const template = readFileSync(resolve(root, 'cmd/gateway/web/src/index.template.html'), 'utf8');
    const debugSection = template.slice(template.indexOf('x-ref="debugScroll"'), template.indexOf('x-ref="monitorScroll"'));
    assert.ok(debugSection.length > 0);
    assert.equal([...debugSection.matchAll(/x-html\s*=/g)].length, 1);
    assert.match(debugSection, /x-html="debugMarkdownHTML\(debugRenderedText/);
    assert.match(debugSection, /x-text="debugRawResponse/);
    assert.match(debugSection, /x-text="debugErrorMessage/);
    assert.doesNotMatch(readFileSync(resolve(root, 'cmd/gateway/web/src/app/18-debug-console.js.part'), 'utf8'), /localStorage|sessionStorage/);
});

function createGatewayApp() {
    const appDir = resolve(root, 'cmd/gateway/web/src/app');
    const parts = readdirSync(appDir).filter(name => name.endsWith('.js.part')).sort()
        .map(name => readFileSync(resolve(appDir, name), 'utf8').replace(/\n$/, '')).join('\n');
    return new Function(`return ({\n${parts}\n})`)();
}

test('模型下拉不区分大小写筛选、去重，支持键盘和手动输入', () => {
    const picker = createGatewayApp().debugModelPicker();
    picker.debugSimple = { model: 'GPT' };
    picker.debugProviderModels = ['gpt-one', 'Claude', 'GPT-two', 'gpt-one'];
    picker.$nextTick = () => {};
    picker.$refs = { modelInput: { focus() {} } };
    assert.deepEqual(picker.filtered(), ['gpt-one', 'GPT-two']);
    picker.move(1);
    assert.equal(picker.active, 0);
    picker.move(-1);
    assert.equal(picker.active, 1);
    picker.choose(picker.filtered()[picker.active]);
    assert.equal(picker.debugSimple.model, 'GPT-two');
    assert.equal(picker.open, false);
    picker.debugSimple.model = 'custom-model';
    picker.move(1);
    assert.equal(picker.active, -1);
    picker.close();
    assert.equal(picker.debugSimple.model, 'custom-model');
    picker.openList();
    picker.sync('another', 'provider', []);
    assert.equal(picker.open, false);
});

test('全局 User-Agent 开关在配置加载前后都有完整默认值', () => {
    const app = createGatewayApp();
    const defaults = { anthropic: true, openai: true, 'openai-responses': true };
    assert.deepEqual(app.config.globalUserAgentEnabled, defaults, '首次渲染前即有三种格式的默认开关');

    app._serverConfigData = { providers: {}, routes: [] };
    app.applyServerConfig();
    assert.deepEqual(app.config.globalUserAgentEnabled, defaults, '旧配置缺少开关时沿用默认启用语义');

    app._serverConfigData = { providers: {}, routes: [], globalUserAgentEnabled: { anthropic: false } };
    app.applyServerConfig();
    assert.deepEqual(app.config.globalUserAgentEnabled, {
        anthropic: false, openai: true, 'openai-responses': true
    }, '加载后只保留显式关闭，其余格式仍默认启用');
});

test('所有前端片段按 webbuild 顺序合并后可解析', () => {
    assert.equal(typeof createGatewayApp().debugRun, 'function');
});

function sseEvent(name, data) {
    return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

function responseFromByteChunks(text, headers = { 'content-type': 'text/event-stream' }) {
    const bytes = new TextEncoder().encode(text);
    let offset = 0;
    const stream = new ReadableStream({
        pull(controller) {
            if (offset >= bytes.length) return controller.close();
            const size = [1, 2, 4, 3][offset % 4];
            controller.enqueue(bytes.slice(offset, offset + size));
            offset += size;
        }
    });
    return new Response(stream, { status: 200, headers });
}

function readyRunApp(overrides = {}) {
    return appFor({
        debugMode: 'provider',
        debugProvider: 'saved',
        debugClientFormat: 'anthropic',
        debugRequestView: 'simple',
        debugSimple: { model: 'claude-test', system: '', message: 'hello', stream: true, maxTokens: '' },
        ...overrides
    });
}

test('debugRun 解码任意字节分片、解析内层 CRLF SSE 和真实用量', async () => {
    const originalFetch = globalThis.fetch;
    const inner = [
        'event: message_start\r\ndata: {"message":{"usage":{"input_tokens":4}}}\r\n\r\n',
        `event: content_block_delta\r\ndata: ${JSON.stringify({ delta: { type: 'text_delta', text: '多字节响应你好' } })}\r\n\r\n`,
        'event: message_delta\r\ndata: {"usage":{"output_tokens":2}}\r\n\r\n'
    ].join('');
    const envelope = [
        sseEvent('meta', { clientFormat: 'anthropic' }),
        sseEvent('response_start', { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
        sseEvent('response_chunk', { data: inner }),
        sseEvent('done', { log: { status: 200, durationMs: 10, attempts: 1 }, aborted: false })
    ].join('');
    globalThis.fetch = async () => responseFromByteChunks(envelope);
    try {
        const app = readyRunApp();
        await app.debugRun();
        assert.equal(app.debugStatus, 'success');
        assert.equal(app.debugRenderedText, '多字节响应你好');
        assert.deepEqual(app.debugUsage, { inputTokens: 4, outputTokens: 2 });
        assert.equal(app.debugUsage.totalTokens, undefined);
    } finally {
        globalThis.fetch = originalFetch;
    }
});

test('debugRun 处理事件流前 JSON 错误和缺少 done 的异常结束', async () => {
    const originalFetch = globalThis.fetch;
    try {
        globalThis.fetch = async () => new Response(JSON.stringify({ type: 'debug_invalid_request', message: 'fixture error' }), {
            status: 400, headers: { 'content-type': 'application/json' }
        });
        const rejected = readyRunApp();
        await rejected.debugRun();
        assert.equal(rejected.debugStatus, 'failure');
        assert.match(rejected.debugErrorMessage, /fixture error/);
        assert.equal(rejected.debugResponseStart.status, 400);

        globalThis.fetch = async () => responseFromByteChunks(
            sseEvent('meta', { clientFormat: 'anthropic' }) + sseEvent('response_start', { status: 200, headers: { 'Content-Type': 'application/json' } })
        );
        const incomplete = readyRunApp();
        await incomplete.debugRun();
        assert.equal(incomplete.debugStatus, 'incomplete');
        assert.match(incomplete.debugStatusMessage, /未收到 done/);
    } finally {
        globalThis.fetch = originalFetch;
    }
});

test('debugRun 中止会关闭 reader，同一时刻第二次调用不会发出请求', async () => {
    const originalFetch = globalThis.fetch;
    let fetchCount = 0;
    let streamCancelled = false;
    globalThis.fetch = async () => {
        fetchCount++;
        const stream = new ReadableStream({
            start(controller) {
                controller.enqueue(new TextEncoder().encode(
                    sseEvent('meta', { clientFormat: 'anthropic' }) +
                    sseEvent('response_start', { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
                ));
            },
            cancel() { streamCancelled = true; }
        });
        return new Response(stream, { status: 200, headers: { 'content-type': 'text/event-stream' } });
    };
    try {
        const app = readyRunApp();
        const running = app.debugRun();
        for (let attempt = 0; attempt < 100 && !app._debugReader; attempt++) await new Promise(resolveNext => setImmediate(resolveNext));
        assert.ok(app._debugReader, 'reader 已启动');
        await app.debugRun();
        assert.equal(fetchCount, 1);
        app.stopDebugRun();
        await running;
        assert.equal(app.debugStatus, 'aborted');
        assert.equal(streamCancelled, true);
        assert.equal(app._debugRunActive, false);
    } finally {
        globalThis.fetch = originalFetch;
    }
});
