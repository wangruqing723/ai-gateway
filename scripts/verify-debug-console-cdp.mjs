#!/usr/bin/env node
// 使用系统 Chrome、Node 内置 WebSocket 与本地假接口验证调试控制台；不加载 config.yaml/data、不访问上游。
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { basename, dirname, extname, join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';

const repo = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const webRoot = resolve(repo, 'cmd/gateway/web');
const html = readFileSync(join(webRoot, 'index.html'), 'utf8');
assert.ok(html.includes('x-ref="debugScroll"'), '正式 index.html 缺少调试页内容区');
assert.ok(html.includes('debugRawResponse'), '正式 index.html 缺少调试响应视图');
const tempPrefix = join(tmpdir(), 'ai-gateway-debug-console-');
const tempRoot = mkdtempSync(tempPrefix);
const fixtureConfig = {
    revision: 'fixture-revision',
    providers: {
        saved: { baseUrl: 'http://upstream.invalid/v1', format: 'anthropic', enabled: true, apiKey: 'fixture-only-key', maxConcurrent: 2, maxPerSecond: 0, maxQueueWait: 1000 },
        disabled: { baseUrl: 'http://disabled.invalid/v1', format: 'openai', enabled: false, apiKey: 'fixture-disabled-key', maxConcurrent: 1, maxPerSecond: 0, maxQueueWait: 1000 }
    },
    routes: [
        { match: 'claude-*', enabled: true, strategy: 'failover', targets: [{ provider: 'saved', model: 'fixture-target' }, { provider: 'disabled' }] },
        { match: 'gpt-fixture', enabled: true, targets: [{ provider: 'saved' }] }
    ],
    failover: {}, breaker: {}, providerHealth: {}, cache: {}
};
const debugRequests = [];
const logQueries = [];
const modelQueries = [];
let abortedRequests = 0;
const fixtureLogs = [
    { id: 'debug-fixture', time: '2026-11-25T00:00:00Z', model: 'claude-fixture', targetModel: 'fixture-target', provider: 'saved', status: 200, stream: true, durationMs: 25, attempts: 1, debug: true, debugMode: 'provider' },
    { id: 'normal-fixture', time: '2026-11-25T00:00:01Z', model: 'gpt-fixture', provider: 'saved', status: 200, stream: false, durationMs: 18, attempts: 1 }
];

function sendJSON(res, status, value) {
    const bytes = Buffer.from(JSON.stringify(value));
    res.writeHead(status, { 'content-type': 'application/json', 'content-length': String(bytes.length) });
    res.end(bytes);
}

function readBody(req) {
    return new Promise((resolveBody, reject) => {
        const chunks = [];
        req.on('data', chunk => chunks.push(chunk));
        req.on('end', () => resolveBody(Buffer.concat(chunks).toString('utf8')));
        req.on('error', reject);
    });
}

function outerEvent(name, data) {
    return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

function innerStream(format) {
    const unsafeText = '<img src=x onerror="window.__debugHTMLExecuted=true">你好';
    if (format === 'anthropic') {
        return [
            'event: message_start\r\ndata: {"type":"message_start","message":{"usage":{"input_tokens":7}}}\r\n\r\n',
            `event: content_block_delta\r\ndata: ${JSON.stringify({ type: 'content_block_delta', delta: { type: 'text_delta', text: unsafeText } })}\r\n\r\n`,
            'event: message_delta\r\ndata: {"type":"message_delta","usage":{"output_tokens":3}}\r\n\r\n'
        ].join('');
    }
    if (format === 'openai-chat') {
        return [
            `data: ${JSON.stringify({ choices: [{ delta: { content: 'chat fixture' } }] })}\r\n\r\n`,
            'data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}\r\n\r\n'
        ].join('');
    }
    return [
        `event: response.output_text.delta\r\ndata: ${JSON.stringify({ type: 'response.output_text.delta', delta: 'responses fixture' })}\r\n\r\n`,
        'event: response.completed\r\ndata: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}\r\n\r\n'
    ].join('');
}

async function writeFragmented(res, text) {
    const bytes = Buffer.from(text, 'utf8');
    let index = 0;
    let step = 0;
    while (index < bytes.length && !res.destroyed) {
        const size = [1, 2, 5, 3, 7][step++ % 5];
        res.write(bytes.subarray(index, index + size));
        index += size;
        if (step % 16 === 0) await new Promise(resolveNext => setTimeout(resolveNext, 1));
    }
    if (!res.destroyed) res.end();
}

function debugLog(payload) {
    return {
        id: `debug-${debugRequests.length}`,
        time: '2026-11-25T00:00:00Z',
        model: payload.body.model,
        targetModel: 'fixture-target',
        provider: 'saved',
        status: 200,
        stream: payload.body.stream !== false,
        durationMs: 25,
        firstByteMs: 7,
        attempts: 2,
        attemptTrail: 'saved/fixture-target → saved/fixture-target',
        attemptDetails: [{ sequence: 1, provider: 'saved', targetModel: 'fixture-target', providerFormat: 'anthropic', outcome: 'success', upstreamStatus: 200, durationMs: 25 }],
        attemptDetailsOmitted: 1,
        debug: true,
        debugMode: payload.mode
    };
}

const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    if (url.pathname === '/api/config') return sendJSON(res, 200, fixtureConfig);
    if (url.pathname === '/health') return sendJSON(res, 200, { status: 'ok', listenAddress: '127.0.0.1:7789', queues: {}, providerHealth: {}, providerKeys: {} });
    if (url.pathname === '/api/metrics') return sendJSON(res, 200, { summary: {}, providers: {}, events: [], vision: {} });
    if (url.pathname.startsWith('/api/providers/models')) {
        modelQueries.push({ path: url.pathname, provider: url.searchParams.get('provider') || '' });
        return sendJSON(res, 200, { models: ['fixture-model', 'fixture-alt'], source: 'fixture', cached: true, queriedAt: '2026-11-25T00:00:00Z' });
    }
    if (url.pathname === '/api/logs') {
        const params = Object.fromEntries(url.searchParams.entries());
        logQueries.push(params);
        let data = fixtureLogs;
        if (params.debug === 'only') data = data.filter(log => log.debug === true);
        if (params.debug === 'exclude') data = data.filter(log => log.debug !== true);
        if (params.provider) data = data.filter(log => log.provider === params.provider);
        return sendJSON(res, 200, { data, limit: 100 });
    }
    if (url.pathname === '/api/debug/run') {
        let payload;
        try { payload = JSON.parse(await readBody(req)); }
        catch { return sendJSON(res, 400, { type: 'debug_invalid_request', message: 'fixture JSON parse error' }); }
        debugRequests.push(payload);
        if (payload.body?.model === 'preflight-error') {
            return sendJSON(res, 400, { type: 'debug_invalid_request', message: '<img src=x onerror="window.__debugHTMLExecuted=true"> fixture rejection' });
        }
        res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-store', connection: 'keep-alive' });
        res.flushHeaders?.();
        const meta = { requestId: 'debug-fixture', mode: payload.mode, clientFormat: payload.clientFormat, model: payload.body.model, candidates: [{ provider: 'saved', targetModel: 'fixture-target', providerFormat: 'anthropic', enabled: true }] };
        if (payload.body.model === 'abort-model') {
            res.write(outerEvent('meta', meta) + outerEvent('response_start', { status: 200, headers: { 'Content-Type': 'text/event-stream' } }));
            res.on('close', () => { if (!res.writableEnded) abortedRequests++; });
            return;
        }
        if (payload.body.model === 'incomplete-model') {
            res.end(outerEvent('meta', meta) + outerEvent('response_start', { status: 200, headers: { 'Content-Type': 'application/json' } }));
            return;
        }
        const upstream = {
            sequence: 1, provider: 'saved', targetModel: 'fixture-target', providerFormat: 'anthropic',
            method: 'POST', url: 'http://upstream.invalid/v1/messages',
            headers: { Authorization: '<已脱敏>', 'Content-Type': 'application/json' },
            body: JSON.stringify(payload.body), bodyTruncated: false
        };
        const events = [
            outerEvent('meta', meta),
            outerEvent('upstream_request', upstream),
            outerEvent('response_start', { status: 200, headers: { 'Content-Type': 'text/event-stream', 'X-Ai-Gateway-Provider': 'saved' } }),
            outerEvent('response_chunk', { data: innerStream(payload.clientFormat) }),
            outerEvent('done', { log: debugLog(payload), aborted: false })
        ].join('');
        void writeFragmented(res, events);
        return;
    }
    if (url.pathname.startsWith('/api/')) return sendJSON(res, 200, { data: [], accounts: [], events: [], summary: {}, providers: {}, breakers: {}, queues: {}, vision: {} });

    const path = url.pathname === '/' ? '' : decodeURIComponent(url.pathname.slice(1));
    const file = path ? resolve(webRoot, path) : '';
    if (path && !file.startsWith(webRoot + '/') && file !== webRoot) return sendJSON(res, 403, { error: 'forbidden' });
    if (!path) {
        res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
        res.end(html);
        return;
    }
    try {
        const content = readFileSync(file);
        const types = { '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2', '.svg': 'image/svg+xml' };
        res.writeHead(200, { 'content-type': `${types[extname(file)] || 'application/octet-stream'}; charset=utf-8` });
        res.end(content);
    } catch { res.writeHead(404); res.end(); }
});

const chromePath = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
let httpPort;
let cdpPort;
let chrome;
let chromeSpawnError;
let socket;
let socketSequence = 0;
const pending = new Map();
const exceptions = [];
const externalRequests = [];
const results = [];
let verificationError = null;
const check = (name, ok) => {
    results.push(`${ok ? 'PASS' : 'FAIL'} ${name}`);
    assert.ok(ok, name);
};

async function waitForChromeExit(child, timeoutMs) {
    if (!child || !child.pid || child.exitCode !== null || child.signalCode !== null) return true;
    return new Promise(resolveExit => {
        const finish = exited => {
            clearTimeout(timer);
            child.removeListener('exit', onExit);
            child.removeListener('error', onError);
            resolveExit(exited);
        };
        const onExit = () => finish(true);
        const onError = () => finish(true);
        const timer = setTimeout(() => finish(false), timeoutMs);
        child.once('exit', onExit);
        child.once('error', onError);
    });
}

async function waitForHost(predicate, label, timeout = 6000) {
    const until = Date.now() + timeout;
    while (Date.now() < until) {
        if (predicate()) return;
        await new Promise(resolveDelay => setTimeout(resolveDelay, 25));
    }
    throw new Error(`等待超时：${label}`);
}

function ownsTempRoot() {
    const resolved = resolve(tempRoot);
    return dirname(resolved) === resolve(tmpdir()) && basename(resolved).startsWith('ai-gateway-debug-console-');
}

async function cleanup() {
    const errors = [];
    try { socket?.close(); } catch (error) { errors.push(`关闭 CDP WebSocket：${error.message}`); }
    for (const waiter of pending.values()) clearTimeout(waiter.timer);
    pending.clear();

    try {
        if (chrome?.pid && chrome.exitCode === null && chrome.signalCode === null) {
            const exited = waitForChromeExit(chrome, 5000);
            chrome.kill();
            if (!await exited) {
                const forceExited = waitForChromeExit(chrome, 3000);
                chrome.kill('SIGKILL');
                if (!await forceExited) throw new Error('Chrome 在 SIGKILL 后仍未退出');
            }
        }
    } catch (error) {
        errors.push(`等待 Chrome 退出：${error.message}`);
    }

    try {
        if (server.listening) {
            await new Promise((resolveClose, rejectClose) => server.close(error => error ? rejectClose(error) : resolveClose()));
        }
    } catch (error) {
        errors.push(`关闭假接口：${error.message}`);
    }

    try {
        if (!ownsTempRoot()) throw new Error('临时目录不属于本脚本创建的 mkdtemp 目录');
        rmSync(tempRoot, { recursive: true, force: true });
    } catch (error) {
        errors.push(`清理临时目录：${error.message}`);
    }

    if (errors.length) console.error(`资源清理告警（不覆盖浏览器验证结论）：\n${errors.join('\n')}`);
}

try {
    await new Promise((resolveListen, rejectListen) => {
        const onError = error => {
            server.removeListener('listening', onListening);
            rejectListen(error);
        };
        const onListening = () => {
            server.removeListener('error', onError);
            resolveListen();
        };
        server.once('error', onError);
        server.once('listening', onListening);
        server.listen(0, '127.0.0.1');
    });
    httpPort = server.address().port;
    cdpPort = 19000 + (process.pid % 10000);
    chrome = spawn(chromePath, [
        '--headless=new', `--remote-debugging-port=${cdpPort}`, `--user-data-dir=${join(tempRoot, 'profile')}`,
        '--no-first-run', '--no-default-browser-check', '--disable-background-networking', '--window-size=1440,1000', 'about:blank'
    ], { stdio: 'ignore' });
    chrome.on('error', error => { chromeSpawnError = error; });

    let target;
    for (let attempt = 0; attempt < 80 && !target; attempt++) {
        await new Promise(resolveDelay => setTimeout(resolveDelay, 125));
        if (chromeSpawnError) throw chromeSpawnError;
        if (chrome.exitCode !== null) throw new Error(`系统 Chrome 已退出，exitCode=${chrome.exitCode}`);
        try { target = (await (await fetch(`http://127.0.0.1:${cdpPort}/json/list`)).json()).find(item => item.type === 'page'); }
        catch { /* Chrome 尚未启动 */ }
    }
    assert.ok(target, '系统 Chrome 未启动');
    socket = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((resolveOpen, rejectOpen) => {
        const timer = setTimeout(() => rejectOpen(new Error('CDP WebSocket 连接超时')), 6000);
        socket.addEventListener('open', () => { clearTimeout(timer); resolveOpen(); }, { once: true });
        socket.addEventListener('error', () => { clearTimeout(timer); rejectOpen(new Error('CDP WebSocket 连接失败')); }, { once: true });
    });
    socket.addEventListener('message', event => {
        const message = JSON.parse(event.data);
        if (message.id && pending.has(message.id)) {
            const waiter = pending.get(message.id);
            clearTimeout(waiter.timer);
            pending.delete(message.id);
            if (message.error) waiter.reject(new Error(`CDP ${message.error.message || '命令失败'}`));
            else waiter.resolve(message);
        }
        if (message.method === 'Runtime.exceptionThrown') {
            const details = message.params.exceptionDetails;
            exceptions.push({
                source: 'Runtime.exceptionThrown',
                description: details.exception?.description ?? details.text,
                text: details.text
            });
        }
        if (message.method === 'Runtime.consoleAPICalled' && message.params.type === 'error') {
            exceptions.push({
                source: 'Runtime.consoleAPICalled',
                description: message.params.args.map(argument => argument.value ?? argument.description ?? '').join(' ')
            });
        }
        if (message.method === 'Network.requestWillBeSent') {
            const url = message.params.request.url;
            if (/^https?:/.test(url) && new URL(url).origin !== `http://127.0.0.1:${httpPort}`) externalRequests.push(url);
        }
    });
    const send = (method, params = {}) => new Promise((resolveMessage, rejectMessage) => {
        const id = ++socketSequence;
        const timer = setTimeout(() => {
            pending.delete(id);
            rejectMessage(new Error(`CDP 命令超时：${method}`));
        }, 8000);
        pending.set(id, { resolve: resolveMessage, reject: rejectMessage, timer });
        try { socket.send(JSON.stringify({ id, method, params })); }
        catch (error) {
            clearTimeout(timer);
            pending.delete(id);
            rejectMessage(error);
        }
    });
    const evaluate = async expression => {
        const response = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
        if (response.result?.exceptionDetails) throw new Error(response.result.exceptionDetails.text);
        return response.result?.result?.value;
    };
    const waitFor = async (expression, label, timeout = 6000) => {
        const until = Date.now() + timeout;
        while (Date.now() < until) {
            if (await evaluate(expression)) return;
            await new Promise(resolveDelay => setTimeout(resolveDelay, 40));
        }
        throw new Error(`等待超时：${label}`);
    };
    await send('Runtime.enable');
    await send('Page.enable');
    await send('Network.enable');
    await send('Page.navigate', { url: `http://127.0.0.1:${httpPort}/#debug` });
    await waitFor(`Boolean(window.Alpine?.$data(document.body)?._debugConfigSnapshot)`, '配置快照加载');
    await waitFor(`window.Alpine.$data(document.body).activeTab === 'debug'`, 'debug hash 页签激活');
    await evaluate('document.fonts.ready.then(() => true)');
    const debugScrollSelector = '[x-ref="debugScroll"]';
    check('debug hash 导航与标题', await evaluate(`window.location.hash === '#debug' && window.Alpine.$data(document.body).pageTitle() === '调试控制台'`));
    check('debug 页签 bolt 使用本地 Material Symbols 字体', await evaluate(`(() => {
        const tab = Array.from(document.querySelectorAll('nav [role="tab"]')).find(element => element.innerText.includes('调试'));
        const icon = tab?.querySelector('.material-symbols-outlined');
        return Boolean(icon) && icon.textContent.trim() === 'bolt'
            && getComputedStyle(icon).fontFamily.includes('Material Symbols Outlined')
            && document.fonts.check('20px "Material Symbols Outlined"', 'bolt')
            && icon.getBoundingClientRect().width <= 24;
    })()`));
    check('正式调试页内容区节点存在', await evaluate(`Boolean(document.querySelector(${JSON.stringify(debugScrollSelector)}))`));

    const responsePanelSelector = view => `[x-show="debugResponseView === '${view}'"]`;
    const selectResponseView = async (view, expectedText) => {
        const panelSelector = responsePanelSelector(view);
        const expectedContent = expectedText === undefined ? '' : ` && panel.textContent.includes(${JSON.stringify(expectedText)})`;
        await evaluate(`window.Alpine.$data(document.body).debugResponseView = ${JSON.stringify(view)}; true`);
        await waitFor(`(() => {
            const app = window.Alpine.$data(document.body);
            const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
            const panel = root?.querySelector(${JSON.stringify(panelSelector)});
            return Boolean(root && panel && app.debugResponseView === ${JSON.stringify(view)} && getComputedStyle(panel).display !== 'none'${expectedContent});
        })()`, `响应 ${view} 视图 Alpine DOM 更新`);
    };
    const responsePanelText = async view => {
        const panelSelector = responsePanelSelector(view);
        return evaluate(`(() => {
            const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
            const panel = root?.querySelector(${JSON.stringify(panelSelector)});
            return root && panel && getComputedStyle(panel).display !== 'none' ? panel.textContent : null;
        })()`);
    };
    const responsePreText = async view => {
        const panelSelector = responsePanelSelector(view);
        return evaluate(`(() => {
            const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
            const panel = root?.querySelector(${JSON.stringify(panelSelector)});
            const pre = panel?.querySelector('.debug-markdown, pre');
            return root && panel && pre && getComputedStyle(panel).display !== 'none' ? pre.textContent : null;
        })()`);
    };

    await evaluate(`window.Alpine.$data(document.body).openDebugForRoute(0)`);
    check('路由快捷入口预填已保存路由与具体模型提示', await evaluate(`(() => { const a = window.Alpine.$data(document.body); return a.debugMode === 'route' && a.debugRouteIndex === 0 && a.debugRouteHasWildcard() && a.debugSimple.model === ''; })()`));

    await evaluate(`window.Alpine.$data(document.body).openDebugForProvider('saved')`);
    await waitFor(`window.Alpine.$data(document.body).debugProviderModels.includes('fixture-model')`, 'Provider 模型目录');
    check('Provider 模型列表复用已保存模型查询接口', modelQueries.some(query => query.path === '/api/providers/models' && query.provider === 'saved') && await evaluate(`window.Alpine.$data(document.body).debugProviderModels.includes('fixture-alt')`));
    check('调试模型目录只向本地假接口查询', modelQueries.length > 0 && modelQueries.every(query => query.path === '/api/providers/models' && query.provider === 'saved'));

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.config.providers.unsavedDraft = { baseUrl: 'http://draft.invalid', format: 'anthropic' }; return true; })()`);
    const savedNames = await evaluate(`window.Alpine.$data(document.body).debugSavedProviders().map(entry => entry.name)`);
    check('配置快照不包含未保存 Provider 草稿', savedNames.includes('saved') && !savedNames.includes('unsavedDraft'));
    check('未保存新 Provider 的快捷入口不可用', await evaluate(`document.querySelector('[aria-label="调试 Provider unsavedDraft"]')?.disabled === true`));

    const setupSimple = (format, model = 'fixture-model') => evaluate(`(() => {
        const a = window.Alpine.$data(document.body);
        a.debugMode = 'provider'; a.debugProvider = 'saved'; a.debugRequestView = 'simple';
        a.debugClientFormat = ${JSON.stringify(format)};
        a.debugSimple = { model: ${JSON.stringify(model)}, system: 'fixture system', message: 'fixture user', stream: true, maxTokens: '' };
        return a.debugBuildRequest();
    })()`);

    const anthropicBuild = await setupSimple('anthropic');
    check('Anthropic body 使用 system/messages 且省略 max_tokens', anthropicBuild.valid && anthropicBuild.payload.body.system === 'fixture system' && anthropicBuild.payload.body.messages[0].content === 'fixture user' && !Object.hasOwn(anthropicBuild.payload.body, 'max_tokens'));
    const chatBuild = await setupSimple('openai-chat');
    check('OpenAI Chat body 注入流式 usage 选项', chatBuild.valid && chatBuild.payload.body.messages[0].role === 'system' && chatBuild.payload.body.stream_options.include_usage === true);
    const responsesBuild = await setupSimple('openai-responses');
    check('OpenAI Responses body 使用 instructions/input', responsesBuild.valid && responsesBuild.payload.body.instructions === 'fixture system' && responsesBuild.payload.body.input === 'fixture user' && !Object.hasOwn(responsesBuild.payload.body, 'max_output_tokens'));

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugMode = 'route'; a.debugRouteIndex = 0; a.debugRequestView = 'simple'; a.debugSimple.model = 'claude-*'; return true; })()`);
    check('通配路由拒绝把模式本身作为模型名', await evaluate(`!window.Alpine.$data(document.body).debugCanRun()`));
    await evaluate(`window.Alpine.$data(document.body).debugSimple.model = 'claude-fixture'`);
    check('路由请求引用已保存路由下标和 match', await evaluate(`(() => { const p = window.Alpine.$data(document.body).debugBuildRequest().payload; return p.mode === 'route' && p.routeIndex === 0 && p.routeMatch === 'claude-*'; })()`));
    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugMode = 'target'; a.debugTargetIndex = 1; return true; })()`);
    check('指定候选请求带有已保存的 target identity', await evaluate(`(() => { const p = window.Alpine.$data(document.body).debugBuildRequest().payload; return p.mode === 'target' && p.targetIndex === 1 && p.targetProvider === 'disabled'; })()`));

    for (const format of ['anthropic', 'openai-chat', 'openai-responses']) {
        await setupSimple(format);
        await evaluate('window.Alpine.$data(document.body).debugRun(); true');
        await waitFor(`window.Alpine.$data(document.body).debugStatus === 'success' && !window.Alpine.$data(document.body)._debugRunActive`, `${format} 假 SSE 请求`);
        const sent = debugRequests.at(-1);
        check(`${format} 请求实际发送该协议 body`, sent.clientFormat === format && sent.body.model === 'fixture-model');
    }

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugRequestView = 'raw'; a.debugRawJSON = '{"model":"raw-fixture","messages":[],"stream":false,"custom":"preserve me"}'; a.debugRawEdited = true; a.debugClientFormat = 'openai-chat'; a.debugChangeClientFormat('openai-responses'); return true; })()`);
    check('格式切换保留原始 JSON 草稿', await evaluate(`window.Alpine.$data(document.body).debugRawJSON.includes('preserve me')`));
    await evaluate('window.Alpine.$data(document.body).debugRun(); true');
    await waitFor(`window.Alpine.$data(document.body).debugStatus === 'success' && !window.Alpine.$data(document.body)._debugRunActive`, '原始 JSON 请求');
    const rawSent = debugRequests.at(-1);
    check('原始 JSON 模式 body 未被格式或表单覆盖', rawSent.clientFormat === 'openai-responses' && rawSent.body.model === 'raw-fixture' && rawSent.body.custom === 'preserve me' && rawSent.body.stream === false);

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugClientFormat = 'anthropic'; a.debugRequestView = 'simple'; a.debugSimple = { model: 'fixture-model', system: '', message: 'x', stream: true, maxTokens: '' }; a.debugMode = 'provider'; a.debugProvider = 'saved'; return true; })()`);
    await evaluate('window.Alpine.$data(document.body).debugRun(); true');
    await waitFor(`window.Alpine.$data(document.body).debugStatus === 'success' && !window.Alpine.$data(document.body)._debugRunActive`, '分片与中文响应');
    check('SSE 多字节分片、CRLF、尾部 usage 均解析', await evaluate(`(() => { const a = window.Alpine.$data(document.body); return a.debugRenderedText.includes('你好') && a.debugUsage.inputTokens === 7 && a.debugUsage.outputTokens === 3; })()`));
    const expectedRenderedText = '<img src=x onerror="window.__debugHTMLExecuted=true">你好';
    await selectResponseView('text', expectedRenderedText);
    check('响应内容按纯文本渲染，HTML 事件未执行', await evaluate(`(() => {
        const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
        const panel = root?.querySelector(${JSON.stringify(responsePanelSelector('text'))});
        const pre = panel?.querySelector('.debug-markdown, pre');
        const app = window.Alpine.$data(document.body);
        return Boolean(root && panel && pre && getComputedStyle(panel).display !== 'none')
            && app.debugRenderedText === ${JSON.stringify(expectedRenderedText)} && pre.textContent.trim() === ${JSON.stringify(expectedRenderedText)}
            && !pre.querySelector('img') && !window.__debugHTMLExecuted;
    })()`));
    check('未提供的 usage 项不会被伪造为零', await evaluate(`window.Alpine.$data(document.body).debugUsage.totalTokens === undefined`));
    await selectResponseView('raw', innerStream('anthropic'));
    const rawResponseText = await responsePreText('raw');
    check('原始响应视图保留完整内层 SSE 原文', rawResponseText === innerStream('anthropic') && await evaluate(`(() => {
        const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
        const panel = root?.querySelector(${JSON.stringify(responsePanelSelector('raw'))});
        const pre = panel?.querySelector('.debug-markdown, pre');
        return Boolean(root && panel && pre && getComputedStyle(panel).display !== 'none')
            && !pre.querySelector('img') && !window.__debugHTMLExecuted;
    })()`));
    await selectResponseView('text', expectedRenderedText);
    const textView = await responsePreText('text');
    check('渲染文本视图只保留文本增量', textView.trim() === expectedRenderedText && !textView.includes('content_block_delta'));
    const markdownFixture = '# 标题\n\n**加粗**与 `代码`\n\n- 项目\n\n> 引用\n\n| 名称 | 值 |\n| --- | --- |\n| 测试 | 1 |\n\n```js\nconst value = 1;\n```\n\n[安全链接](https://example.com)\n\n![图片](https://example.com/tracker.png)\n\n[危险](javascript:alert(1))\n\n<img src=x onerror="window.__debugHTMLExecuted=true">\n<div x-init="window.__debugHTMLExecuted=true">内容</div>';
    await evaluate(`window.Alpine.$data(document.body).debugRenderedText = ${JSON.stringify(markdownFixture)}`);
    await waitFor(`document.querySelector('.debug-markdown h1')?.textContent === '标题'`, 'Markdown 正文更新');
    check('Markdown 标题、列表、引用、表格与代码块渲染', await evaluate(`(() => { const r = document.querySelector('.debug-markdown'); return ['h1','strong','ul li','blockquote','table td','pre code'].every(s => r.querySelector(s)); })()`));
    check('Markdown 不产生图片、危险链接或可执行属性', await evaluate(`(() => { const r = document.querySelector('.debug-markdown'); return !r.querySelector('img,script,[x-init],[onerror]') && r.querySelectorAll('a').length === 1 && r.querySelector('a').getAttribute('href') === 'https://example.com' && !window.__debugHTMLExecuted; })()`));
    await evaluate('window.Alpine.$data(document.body).debugRenderedText = "```js\\nconst partial ="');
    await waitFor(`document.querySelector('.debug-markdown pre code')?.textContent.includes('const partial =')`, '未闭合流式代码块');
    await evaluate('window.Alpine.$data(document.body).debugRenderedText += " 1;\\n```"');
    await waitFor(`document.querySelector('.debug-markdown pre code')?.textContent.includes('const partial = 1;')`, '流式代码块闭合');
    check('流式未闭合代码块更新后正确闭合', true);
    await evaluate(`window.Alpine.$data(document.body).debugRenderedText = ${JSON.stringify(expectedRenderedText)}`);
    await selectResponseView('attempts', '已省略 1 条尝试明细');
    check('尝试视图复用 attemptDetailRows 的省略行', (await responsePanelText('attempts'))?.includes('已省略 1 条尝试明细') === true);
    await selectResponseView('upstream', '<已脱敏>');
    const upstreamViewText = await responsePanelText('upstream');
    check('上游请求视图逐次展示脱敏请求', upstreamViewText !== null && !upstreamViewText.includes('fixture-only-key') && upstreamViewText.includes('<已脱敏>'));
    await selectResponseView('headers', 'X-Ai-Gateway-Provider');
    check('响应头视图显示真实响应头', (await responsePanelText('headers'))?.includes('X-Ai-Gateway-Provider') === true);

    await evaluate(`window.Alpine.$data(document.body).selectNav('logs')`);
    await waitFor(`window.Alpine.$data(document.body).activeTab === 'logs'`, '请求日志页签激活');
    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.logFilters.provider = 'saved'; a.logFilters.debug = 'only'; a.loadRequestLogs(); return true; })()`);
    await waitFor(`window.Alpine.$data(document.body).requestLogs.length === 1 && window.Alpine.$data(document.body).requestLogs[0]?.id === 'debug-fixture'`, '仅调试日志筛选');
    await waitFor(`document.querySelector('tbody tr[role="button"]')?.innerText.includes('调试 · Provider')`, '日志调试徽标 DOM 更新');
    check('日志筛选 only 保留 Provider 筛选并发送 debug=only', Boolean(logQueries.at(-1)) && logQueries.at(-1).provider === 'saved' && logQueries.at(-1).debug === 'only');
    check('日志行显示调试徽标', await evaluate(`document.querySelector('tbody tr[role="button"]')?.innerText.includes('调试 · Provider')`));
    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.logFilters.debug = 'exclude'; a.loadRequestLogs(); return true; })()`);
    await waitFor(`window.Alpine.$data(document.body).requestLogs[0]?.id === 'normal-fixture'`, '排除调试日志筛选');
    check('日志筛选 exclude 保留 Provider 筛选', Boolean(logQueries.at(-1)) && logQueries.at(-1).provider === 'saved' && logQueries.at(-1).debug === 'exclude');
    await evaluate(`window.Alpine.$data(document.body).resetLogFilters()`);
    await waitFor(`window.Alpine.$data(document.body).logFilters.debug === '' && window.Alpine.$data(document.body).requestLogs.length === 2`, '重置日志筛选');
    check('重置清除 debug 与既有筛选项', Boolean(logQueries.at(-1)) && logQueries.at(-1).debug === undefined && logQueries.at(-1).provider === undefined && await evaluate(`Object.values(window.Alpine.$data(document.body).logFilters).every(value => value === '')`));
    check('全部日志筛选恢复普通与调试记录', await evaluate(`window.Alpine.$data(document.body).requestLogs.length === 2`));

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.selectNav('debug'); a.debugRequestView = 'simple'; a.debugMode = 'provider'; a.debugProvider = 'saved'; a.debugClientFormat = 'anthropic'; a.debugSimple = { model: 'preflight-error', system: '', message: 'x', stream: true, maxTokens: '' }; a.debugRun(); return true; })()`);
    await waitFor(`window.Alpine.$data(document.body).debugStatus === 'failure' && !window.Alpine.$data(document.body)._debugRunActive`, '事件流前 JSON 错误');
    await waitFor(`(() => {
        const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
        const alert = root?.querySelector('[role="alert"][x-text="debugErrorMessage"]');
        return Boolean(root && alert && getComputedStyle(alert).display !== 'none' && alert.textContent.includes('fixture rejection'));
    })()`, 'JSON 错误文本更新到 DOM');
    check('HTTP JSON 错误可读且纯文本显示', await evaluate(`(() => {
        const root = document.querySelector(${JSON.stringify(debugScrollSelector)});
        const alert = root?.querySelector('[role="alert"][x-text="debugErrorMessage"]');
        const app = window.Alpine.$data(document.body);
        return Boolean(root && alert) && alert.getAttribute('x-text') === 'debugErrorMessage' && app.debugErrorMessage.includes('fixture rejection')
            && alert.textContent.includes('fixture rejection') && !alert.querySelector('img') && !window.__debugHTMLExecuted;
    })()`));

    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugSimple.model = 'incomplete-model'; a.debugRun(); return true; })()`);
    await waitFor(`window.Alpine.$data(document.body).debugStatus === 'incomplete' && !window.Alpine.$data(document.body)._debugRunActive`, '未收到 done 的异常终止');
    check('未收到 done 显示异常终止状态', await evaluate(`window.Alpine.$data(document.body).debugStatusLabel() === '异常终止'`));

    const beforeAbort = debugRequests.length;
    await evaluate(`(() => { const a = window.Alpine.$data(document.body); a.debugSimple.model = 'abort-model'; a.debugRun(); return true; })()`);
    await waitFor(`window.Alpine.$data(document.body)._debugRunActive`, '中止测试请求启动');
    await evaluate('window.Alpine.$data(document.body).debugRun(); true');
    check('同一时刻第二次运行不会发出请求', debugRequests.length === beforeAbort + 1);
    await evaluate('window.Alpine.$data(document.body).stopDebugRun()');
    await waitFor(`window.Alpine.$data(document.body).debugStatus === 'aborted' && !window.Alpine.$data(document.body)._debugRunActive`, 'AbortController 中止');
    await waitForHost(() => abortedRequests > 0, 'AbortController 关闭假 SSE 连接');
    check('AbortController 关闭假 SSE 连接', abortedRequests > 0);

    for (const [width, height, name] of [[1440, 1000, '桌面'], [390, 844, '移动端']]) {
        await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
        await evaluate(`window.Alpine.$data(document.body).selectNav('debug')`);
        await waitFor(`window.innerWidth === ${width} && window.Alpine.$data(document.body).activeTab === 'debug'`, `${name}调试页布局更新`);
        await evaluate('new Promise(resolveFrame => requestAnimationFrame(() => requestAnimationFrame(resolveFrame)))');
        check(`${name}调试页无横向溢出`, await evaluate('document.documentElement.scrollWidth <= window.innerWidth'));
    }
    check('页面没有 JavaScript 异常或外部请求', exceptions.length === 0 && externalRequests.length === 0);
} catch (error) {
    verificationError = error;
    throw error;
} finally {
    console.log(results.join('\n'));
    console.log(`假接口请求数：${debugRequests.length}；日志查询数：${logQueries.length}`);
    if (verificationError) console.error(`浏览器异常明细：\n${JSON.stringify(exceptions, null, 2)}`);
    await cleanup();
}
