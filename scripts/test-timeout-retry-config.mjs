import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { spawn } from 'node:child_process';
import { tmpdir } from 'node:os';
import test from 'node:test';
import { dirname, extname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function createGatewayApp() {
    const appDir = resolve(root, 'cmd/gateway/web/src/app');
    const parts = readdirSync(appDir).filter(name => name.endsWith('.js.part')).sort()
        .map(name => readFileSync(resolve(appDir, name), 'utf8').replace(/\n$/, '')).join('\n');
    return new Function(`return ({\n${parts}\n})`)();
}

test('route timeout and retry modes survive load, edit, copy, save, payload, and YAML preview', async () => {
    const app = createGatewayApp();
    app.openRouteDrawer = () => {};
    app.showStatus = () => {};
    app.snapshotConfig = () => null;
    app.persistConfig = async () => true;
    app.restoreConfig = () => {};
    app.config = app.normalizeConfig({
        providers: {
            primary: {
                baseUrl: 'http://upstream.invalid',
                localRetry: { mode: 'untilDisconnect', maxRetries: 4, intervalMs: 9 }
            }
        },
        routes: [
            {
                match: 'multi-*',
                timeoutMode: 'client',
                localRetry: { mode: 'untilDisconnect' },
                targets: [
                    {
                        provider: 'primary', model: 'first', timeoutMode: 'global',
                        localRetry: { mode: 'limited' }
                    },
                    {
                        provider: 'primary', model: 'second',
                        localRetry: { mode: 'untilDisconnect' }
                    }
                ]
            },
            {
                match: 'single-*',
                timeoutMode: 'client',
                localRetry: { mode: 'untilDisconnect', maxRetries: 6, intervalMs: 8 },
                targets: [{
                    provider: 'primary', model: 'only', timeoutMode: 'global',
                    localRetry: { mode: 'limited', maxRetries: 0, intervalMs: 4 }
                }]
            }
        ]
    });

    assert.equal(app.config.providers.primary.localRetry.mode, undefined, 'Provider 不接收 localRetry.mode');
    assert.equal(app.config.routes[0].timeoutMode, 'client');
    assert.equal(app.config.routes[0].localRetry.mode, 'untilDisconnect');
    assert.equal(app.config.routes[0].targets[0].timeoutMode, 'global');
    assert.equal(app.config.routes[0].targets[0].localRetry.mode, 'limited');
    assert.equal(app.config.routes[0].targets[0].localRetry.maxRetries, null, 'mode-only target retains inherited numeric values');
    assert.equal(app.config.routes[0].targets[1].localRetry.mode, 'untilDisconnect');

    app.editRoute(0);
    assert.equal(app.routeForm.timeoutMode, 'client');
    assert.equal(app.routeForm.localRetry.mode, 'untilDisconnect');
    assert.equal(app.routeForm.targets[0].timeoutMode, 'global');
    assert.equal(app.routeForm.targets[0].localRetry.mode, 'limited');
    assert.equal(app.routeForm.targets[1].localRetry.mode, 'untilDisconnect');
    await app.saveRoute();

    app.duplicateRoute(0);
    app.routeForm.match = 'copy-*';
    assert.equal(app.routeForm.timeoutMode, 'client');
    assert.equal(app.routeForm.localRetry.mode, 'untilDisconnect');
    assert.equal(app.routeForm.targets[0].timeoutMode, 'global');
    assert.equal(app.routeForm.targets[0].localRetry.mode, 'limited');
    await app.saveRoute();
    const copied = app.config.routes[2];
    assert.equal(copied.timeoutMode, 'client');
    assert.equal(copied.localRetry.mode, 'untilDisconnect');
    assert.equal(copied.targets[0].timeoutMode, 'global');
    assert.equal(copied.targets[0].localRetry.mode, 'limited');
    assert.equal(copied.targets[1].localRetry.mode, 'untilDisconnect');

    app.editRoute(1);
    await app.saveRoute();
    const single = app.config.routes[1];
    assert.equal(single.targets, undefined, '单候选保存后沿用扁平 route 写法');
    assert.equal(single.timeoutMode, 'global', '单候选 target timeoutMode 覆盖 route');
    assert.equal(single.localRetry.mode, 'limited', '单候选 target localRetry.mode 覆盖 route');
    assert.equal(single.localRetry.maxRetries, 0, '显式有限重试次数 0 保留');

    app.routeForm.localRetry = { mode: 'untilDisconnect', maxRetries: 4, intervalMs: 0 };
    const inheritedTarget = { provider: 'primary', localRetry: { mode: '', maxRetries: null, intervalMs: null } };
    assert.match(app.validateRouteRetrySettings([inheritedTarget]), /候选 1.*须大于 0/);
    inheritedTarget.localRetry.intervalMs = 900;
    assert.equal(app.validateRouteRetrySettings([inheritedTarget]), '', '候选正间隔覆盖路由 0 后可保存');
    inheritedTarget.localRetry.mode = 'limited';
    inheritedTarget.localRetry.intervalMs = 0;
    assert.equal(app.validateRouteRetrySettings([inheritedTarget]), '', '有限模式允许有效间隔为 0');

    const payload = app.configPayload();
    assert.deepEqual(payload.routes, app.config.routes, 'PUT 配置载荷保留所有 route/target 字段');
    assert.equal(payload.providers.primary.localRetry.mode, undefined, 'Provider 载荷不输出 localRetry.mode');
    const routeYaml = app.routesYaml();
    assert.match(routeYaml, /timeoutMode: client/);
    assert.match(routeYaml, /timeoutMode: global/);
    assert.match(routeYaml, /mode: untilDisconnect/);
    assert.match(routeYaml, /mode: limited/);
    assert.doesNotMatch(app.providersYaml(), /mode: (?:limited|untilDisconnect)/);
});

test('config transfer preview, apply, and export preserve timeout and retry YAML', async () => {
    const app = createGatewayApp();
    const transferYaml = [
        'providers:',
        '  primary:',
        '    baseUrl: "http://fixture.invalid"',
        '    localRetry:',
        '      maxRetries: 4',
        '      intervalMs: 800',
        'routes:',
        '  - match: "fixture-*"',
        '    timeoutMode: client',
        '    localRetry:',
        '      mode: untilDisconnect',
        '      maxRetries: 6',
        '      intervalMs: 500',
        '    targets:',
        '      - provider: primary',
        '        model: fixture-model',
        '        timeoutMode: global',
        '        localRetry:',
        '          mode: limited',
        '          maxRetries: 2',
        '          intervalMs: 300'
    ].join('\n');
    const importedConfig = {
        revision: 'fixture-r2',
        providers: { primary: { baseUrl: 'http://fixture.invalid', localRetry: { maxRetries: 4, intervalMs: 800 } } },
        routes: [{
            match: 'fixture-*', timeoutMode: 'client',
            localRetry: { mode: 'untilDisconnect', maxRetries: 6, intervalMs: 500 },
            targets: [{
                provider: 'primary', model: 'fixture-model', timeoutMode: 'global',
                localRetry: { mode: 'limited', maxRetries: 2, intervalMs: 300 }
            }]
        }]
    };
    const calls = [];
    let exportedBlob;
    const previous = {
        fetch: globalThis.fetch,
        document: globalThis.document,
        window: globalThis.window,
        objectURL: Object.getOwnPropertyDescriptor(URL, 'createObjectURL'),
        revokeURL: Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL')
    };

    try {
        app.config = app.normalizeConfig({ providers: {}, routes: [] });
        app.revision = 'fixture-r1';
        app.configTransferImportContent = transferYaml;
        app.guardConfigTransferDraft = async () => true;
        app.askConfirm = async () => true;
        app.showStatus = () => {};
        app.$nextTick = (callback) => callback?.();
        app.loadConfig = async () => {
            app.config = app.normalizeConfig(importedConfig);
            return true;
        };
        app.loadChatGPTAccounts = async () => [];

        globalThis.window = {
            setInterval: () => 1,
            clearInterval: () => {},
            setTimeout: () => 1
        };
        globalThis.document = {
            createElement: () => ({ click() {}, remove() {} }),
            body: { appendChild() {} }
        };
        Object.defineProperty(URL, 'createObjectURL', { configurable: true, writable: true, value: (blob) => { exportedBlob = blob; return 'blob:fixture'; } });
        Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: () => {} });
        globalThis.fetch = async (url, init = {}) => {
            calls.push({ url, init });
            if (url === '/api/config/import/preview') {
                return new Response(JSON.stringify({ data: {
                    catalog: { providers: [{ name: 'primary' }], routes: [{ index: 0, match: 'fixture-*' }], sourceAccountIds: [] },
                    canApply: true, previewToken: 'fixture-preview', expiresAt: '2099-01-01T00:00:00Z'
                } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
            }
            if (url === '/api/config/import/apply') {
                return new Response(JSON.stringify({ status: 'success', message: 'fixture import applied' }), {
                    status: 200, headers: { 'Content-Type': 'application/json' }
                });
            }
            if (url === '/api/config/export') {
                return new Response(transferYaml, {
                    status: 200,
                    headers: { 'Content-Type': 'application/yaml', 'Content-Disposition': 'attachment; filename=fixture.yaml' }
                });
            }
            throw new Error(`未预期的配置传输请求：${url}`);
        };

        await app.previewConfigTransfer();
        await app.applyConfigTransfer();
        assert.equal(app.config.routes[0].timeoutMode, 'client', '导入后的配置加载保留 route timeoutMode');
        assert.equal(app.config.routes[0].localRetry.mode, 'untilDisconnect', '导入后的配置加载保留持续模式');
        assert.equal(app.config.routes[0].targets[0].timeoutMode, 'global', '导入后的配置加载保留候选显式 global');
        assert.equal(app.config.routes[0].targets[0].localRetry.mode, 'limited', '导入后的配置加载保留候选显式 limited');
        assert.equal(app.config.providers.primary.localRetry.mode, undefined, 'Provider 导入数据不产生重试模式');

        const previewCall = calls.find((call) => call.url === '/api/config/import/preview');
        const applyCall = calls.find((call) => call.url === '/api/config/import/apply');
        assert.equal(JSON.parse(previewCall.init.body).content, transferYaml, '预览原样发送 timeout/retry YAML');
        assert.equal(JSON.parse(applyCall.init.body).content, transferYaml, '应用原样发送同一份 timeout/retry YAML');

        app.configTransferExportScope = 'all';
        app.configTransferExporting = false;
        app.configTransferExportError = '';
        await app.exportConfigTransfer();
        assert.deepEqual(JSON.parse(calls.find((call) => call.url === '/api/config/export').init.body), { scope: 'all' });
        assert.match(await exportedBlob.text(), /timeoutMode: client/);
        assert.match(await exportedBlob.text(), /mode: untilDisconnect/);
        assert.match(await exportedBlob.text(), /timeoutMode: global/);
        assert.match(await exportedBlob.text(), /mode: limited/);
    } finally {
        globalThis.fetch = previous.fetch;
        if (previous.document === undefined) delete globalThis.document;
        else globalThis.document = previous.document;
        if (previous.window === undefined) delete globalThis.window;
        else globalThis.window = previous.window;
        if (previous.objectURL) Object.defineProperty(URL, 'createObjectURL', previous.objectURL);
        else delete URL.createObjectURL;
        if (previous.revokeURL) Object.defineProperty(URL, 'revokeObjectURL', previous.revokeURL);
        else delete URL.revokeObjectURL;
    }
});

function sleep(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
}

async function listen(server, host = '127.0.0.1') {
    await new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(0, host, resolve);
    });
    return server.address().port;
}

async function freePort() {
    const server = createNetServer();
    const port = await listen(server);
    await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    return port;
}

test('Chrome CDP route editor applies timeout/retry modes and validates the saved payload', async (t) => {
    assert.equal(typeof WebSocket, 'function', 'Node 20 须使用 --experimental-websocket 启用 WebSocket');
    const webRoot = resolve(root, 'cmd/gateway/web');
    const fixtureDir = mkdtempSync(join(tmpdir(), 'ai-gateway-timeout-retry-cdp-'));
    const initialConfig = {
        revision: 'fixture-r1', restartRequired: [], path: 'fixture memory',
        host: '127.0.0.1', port: 7789, timeout: 120000, streamActivityTimeout: 60000,
        providers: {
            primary: {
                baseUrl: 'http://fixture.invalid', apiKey: 'fixture-only', format: 'anthropic',
                maxConcurrent: 5, maxQueueWait: 30000,
                localRetry: { maxRetries: 4, intervalMs: 1200 }
            }
        },
        routes: [{ match: 'fixture-*', provider: 'primary', model: 'fixture-model' }]
    };
    let savedConfig = structuredClone(initialConfig);
    const savedPayloads = [];
    const fixtureAPIRequests = [];
    const server = createHttpServer(async (req, res) => {
        const requestURL = new URL(req.url, 'http://127.0.0.1');
        if (requestURL.pathname.startsWith('/api/')) {
            fixtureAPIRequests.push({ method: req.method, path: requestURL.pathname });
            if (requestURL.pathname === '/api/config' && req.method === 'GET') {
                res.writeHead(200, { 'Content-Type': 'application/json', ETag: `"${savedConfig.revision}"` });
                res.end(JSON.stringify(savedConfig));
                return;
            }
            if (requestURL.pathname === '/api/config' && req.method === 'PUT') {
                let body = '';
                for await (const chunk of req) body += chunk;
                const payload = JSON.parse(body);
                savedPayloads.push(payload);
                savedConfig = { ...payload, revision: `fixture-r${savedPayloads.length + 1}`, restartRequired: [] };
                res.writeHead(200, { 'Content-Type': 'application/json' });
                res.end(JSON.stringify({ status: 'success', message: 'fixture config saved', restartRequired: [] }));
                return;
            }
            if (requestURL.pathname === '/api/chatgpt/accounts') {
                res.writeHead(200, { 'Content-Type': 'application/json' });
                res.end(JSON.stringify({ data: [] }));
                return;
            }
            if (requestURL.pathname === '/health' || requestURL.pathname === '/api/health') {
                res.writeHead(200, { 'Content-Type': 'application/json' });
                res.end(JSON.stringify({ status: 'ok', providers: {} }));
                return;
            }
            res.writeHead(200, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ data: [], providers: {}, queues: {}, providerHealth: {}, providerKeys: {} }));
            return;
        }
        if (requestURL.pathname === '/health') {
            res.writeHead(200, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ status: 'ok', providers: {} }));
            return;
        }

        const relative = requestURL.pathname === '/' ? 'index.html' : requestURL.pathname.slice(1);
        const file = resolve(webRoot, relative);
        if (!file.startsWith(webRoot + sep)) {
            res.writeHead(404).end();
            return;
        }
        let contents;
        try {
            contents = readFileSync(file);
        } catch {
            res.writeHead(404).end();
            return;
        }
        const type = extname(file) === '.html' ? 'text/html; charset=utf-8'
            : extname(file) === '.css' ? 'text/css; charset=utf-8'
                : extname(file) === '.js' ? 'application/javascript; charset=utf-8' : 'application/octet-stream';
        res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' });
        res.end(contents);
    });

    let httpPort;
    try {
        httpPort = await listen(server);
    } catch (error) {
        rmSync(fixtureDir, { recursive: true, force: true });
        throw error;
    }
    let cdpPort;
    try {
        cdpPort = await freePort();
    } catch (error) {
        await new Promise((resolve) => server.close(() => resolve()));
        rmSync(fixtureDir, { recursive: true, force: true });
        throw error;
    }
    const chromePath = process.env.CHROME_BIN || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
    const chrome = spawn(chromePath, [
        '--headless=new', `--remote-debugging-port=${cdpPort}`, `--user-data-dir=${join(fixtureDir, 'profile')}`,
        '--no-first-run', '--no-default-browser-check', '--remote-allow-origins=*', '--window-size=1440,1000', 'about:blank'
    ], { stdio: 'ignore' });
    let chromeError;
    chrome.once('error', (error) => { chromeError = error; });
    let target;
    for (let attempt = 0; attempt < 100 && !target; attempt++) {
        if (chromeError) break;
        await sleep(100);
        try {
            const response = await fetch(`http://127.0.0.1:${cdpPort}/json/list`);
            target = (await response.json()).find((entry) => entry.type === 'page');
        } catch {
            // Chrome 启动时调试端点尚未监听，继续等待后再读。
        }
    }
    if (chromeError || !target) {
        chrome.kill('SIGTERM');
        await new Promise((resolve) => server.close(() => resolve()));
        rmSync(fixtureDir, { recursive: true, force: true });
        if (chromeError) throw chromeError;
        assert.ok(target, `系统 Chrome 未在 10 秒内启动 CDP；路径：${chromePath}`);
    }

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    const pending = new Map();
    const exceptions = [];
    let sequence = 0;
    ws.addEventListener('message', (event) => {
        const message = JSON.parse(event.data);
        if (message.id && pending.has(message.id)) {
            const entry = pending.get(message.id);
            pending.delete(message.id);
            clearTimeout(entry.timer);
            if (message.error) entry.reject(new Error(`CDP ${entry.method}: ${JSON.stringify(message.error)}`));
            else entry.resolve(message);
        } else if (message.method === 'Runtime.exceptionThrown') {
            exceptions.push(message.params.exceptionDetails);
        }
    });
    const send = (method, params = {}) => new Promise((resolve, reject) => {
        const id = ++sequence;
        const timer = setTimeout(() => {
            pending.delete(id);
            reject(new Error(`CDP ${method} 10 秒无响应`));
        }, 10000);
        pending.set(id, { method, resolve, reject, timer });
        ws.send(JSON.stringify({ id, method, params }));
    });
    const evaluate = async (expression) => {
        const response = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true });
        const result = response.result?.result;
        if (response.result?.exceptionDetails) {
            throw new Error(`浏览器执行异常：${JSON.stringify(response.result.exceptionDetails)}`);
        }
        return result?.value;
    };
    const waitFor = async (expression, label, timeoutMs = 10000) => {
        const deadline = Date.now() + timeoutMs;
        while (Date.now() < deadline) {
            if (await evaluate(expression)) return;
            await sleep(50);
        }
        throw new Error(`等待页面条件超时：${label}\n条件：${expression}`);
    };
    const click = async (selector) => evaluate(`(() => { const element = document.querySelector(${JSON.stringify(selector)}); if (!element) throw new Error('找不到控件：' + ${JSON.stringify(selector)}); element.click(); return true; })()`);
    const setValue = async (selector, value) => evaluate(`(() => {
        const element = document.querySelector(${JSON.stringify(selector)});
        if (!element) throw new Error('找不到控件：' + ${JSON.stringify(selector)});
        const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(element), 'value');
        if (!descriptor?.set) throw new Error('控件没有 value setter：' + ${JSON.stringify(selector)});
        descriptor.set.call(element, ${JSON.stringify(String(value))});
        element.dispatchEvent(new Event('input', { bubbles: true }));
        element.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
    })()`);
    const controlValue = (selector) => evaluate(`document.querySelector(${JSON.stringify(selector)})?.value ?? null`);
    const exists = (selector) => evaluate(`Boolean(document.querySelector(${JSON.stringify(selector)}))`);
    const renderedExpression = (elementExpression) => `(() => {
        const element = ${elementExpression};
        if (!element || typeof element.getClientRects !== 'function' || element.getClientRects().length === 0) return false;
        if (typeof element.checkVisibility === 'function' && !element.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })) return false;
        for (let ancestor = element; ancestor; ancestor = ancestor.parentElement) {
            const style = getComputedStyle(ancestor);
            if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse' || Number(style.opacity) === 0) return false;
        }
        return true;
    })()`;
    const elementExpression = (selector) => `document.querySelector(${JSON.stringify(selector)})`;
    const visible = (selector) => evaluate(renderedExpression(elementExpression(selector)));
    const flushAlpine = () => evaluate('new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))');
    const waitForVisibility = (selector, expected, label) => waitFor(
        `${expected ? '' : '!'}(${renderedExpression(elementExpression(selector))})`, label);
    const waitForVisibleAndTransition = (selector, transitionRootSelector, label) => {
        const visibleExpression = renderedExpression(elementExpression(selector));
        const transitionExpression = `(() => {
            const root = ${elementExpression(transitionRootSelector)};
            if (!root || typeof root.getAnimations !== 'function') return Boolean(root);
            const finiteAnimations = Array.from(root.getAnimations()).filter(animation => {
                const timing = animation.effect?.getComputedTiming?.();
                return !timing || (timing.iterations !== Infinity && Number.isFinite(timing.endTime));
            });
            return finiteAnimations.every(animation => animation.playState === 'finished' || animation.playState === 'idle');
        })()`;
        return waitFor(`(${visibleExpression}) && (${transitionExpression})`, label);
    };
    const ensureExpanded = async (toggleSelector, panelSelector, label) => {
        await waitForVisibility(toggleSelector, true, `${label}按钮可见`);
        const expanded = await evaluate(`${elementExpression(toggleSelector)}?.getAttribute('aria-expanded') === 'true'`);
        if (!expanded) await click(toggleSelector);
        await flushAlpine();
        await waitForVisibleAndTransition(panelSelector, panelSelector, `${label}已展开且过渡结束`);
        assert.equal(await evaluate(`${elementExpression(toggleSelector)}?.getAttribute('aria-expanded')`), 'true', `${label}处于展开状态`);
    };
    const clickLabelText = async (forId, transitionRootSelector, label) => {
        const textSelector = `label[for="${forId}"] > span:first-child`;
        await evaluate(`(() => {
            const element = ${elementExpression(textSelector)};
            if (!element) throw new Error('找不到标签文字：' + ${JSON.stringify(textSelector)});
            element.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'instant' });
            return true;
        })()`);
        await flushAlpine();
        await waitForVisibleAndTransition(textSelector, transitionRootSelector, `${label}可见且展开过渡结束`);
        const point = await evaluate(`(() => {
            const element = ${elementExpression(textSelector)};
            const rect = element?.getBoundingClientRect();
            if (!rect || rect.width <= 0 || rect.height <= 0) throw new Error('标签文字没有可点击区域：' + ${JSON.stringify(textSelector)});
            const x = rect.left + rect.width / 2;
            const y = rect.top + rect.height / 2;
            if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) throw new Error('标签文字未滚动到视口内：' + JSON.stringify({ x, y, innerWidth, innerHeight }));
            return { x, y };
        })()`);
        await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: point.x, y: point.y });
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: point.x, y: point.y, button: 'left', clickCount: 1 });
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: point.x, y: point.y, button: 'left', clickCount: 1 });
    };

    try {
        await new Promise((resolve, reject) => {
            ws.addEventListener('open', resolve, { once: true });
            ws.addEventListener('error', reject, { once: true });
        });
        await send('Runtime.enable');
        await send('Page.enable');
        await send('Page.navigate', { url: `http://127.0.0.1:${httpPort}/#routes` });
        await waitFor(`Boolean(document.querySelector('[data-testid="route-edit-0"]'))`, 'fixture route 已加载');
        await click('[data-testid="route-edit-0"]');
        await flushAlpine();
        await waitForVisibleAndTransition('[role="dialog"][aria-labelledby="route-drawer-title"]', '[x-show="showRouteModal"]', '路由抽屉已展开');
        await waitForVisibility('[data-testid="route-advanced-toggle"]', true, '路由高级设置按钮可见');
        await ensureExpanded('[data-testid="route-advanced-toggle"]', '#route-advanced', '路由高级设置');
        await waitForVisibility('[data-testid="route-timeout-mode"]', true, '路由高级区超时控件可见');

        await setValue('[data-testid="route-timeout-mode"]', 'client');
        await setValue('[data-testid="route-local-retry-mode"]', 'limited');
        await setValue('[data-testid="route-local-retries"]', '4');
        await setValue('[data-testid="route-local-interval"]', '0');
        await setValue('[data-testid="route-local-retry-mode"]', 'untilDisconnect');
        await flushAlpine();
        await waitForVisibility('[data-testid="route-local-retries"]', false, '路由持续模式隐藏有限次数控件');
        assert.equal(await exists('[data-testid="route-local-retries"]'), true, '持续模式下有限次数控件仍在 DOM 中');
        assert.equal(await visible('[data-testid="route-local-retries"]'), false, '持续模式隐藏有限次数控件');
        assert.equal(await controlValue('[data-testid="route-local-retries"]'), '4', '隐藏次数控件仍保留 maxRetries');
        await setValue('[data-testid="route-local-retry-mode"]', 'limited');
        await flushAlpine();
        await waitForVisibility('[data-testid="route-local-retries"]', true, '路由有限模式显示次数控件');
        assert.equal(await exists('[data-testid="route-local-retries"]'), true, '有限模式次数控件仍在 DOM 中');
        assert.equal(await visible('[data-testid="route-local-retries"]'), true, '切回有限模式恢复次数控件');
        assert.equal(await controlValue('[data-testid="route-local-retries"]'), '4', '切回有限模式恢复原次数');
        await setValue('[data-testid="route-local-retry-mode"]', 'untilDisconnect');
        await flushAlpine();

        await ensureExpanded('[data-testid="route-target-settings-0"]', '#route-target-limits-0', '候选设置');
        await waitForVisibility('[data-testid="route-target-timeout-mode-0"]', true, '候选高级区超时控件可见');
        await clickLabelText('route-target-timeout-mode-0', '#route-target-limits-0', '候选超时模式');
        await waitFor(`document.activeElement?.id === 'route-target-timeout-mode-0'`, '真实鼠标点击候选标签后焦点进入对应 select', 1000);
        assert.equal(await evaluate(`document.activeElement?.id === 'route-target-timeout-mode-0'`), true, '真实鼠标点击候选标签将键盘焦点交给对应 select');
        await setValue('[data-testid="route-target-timeout-mode-0"]', 'global');
        await setValue('[data-testid="route-target-local-mode-0"]', 'limited');
        await setValue('[data-testid="route-target-local-retries-0"]', '2');
        await setValue('[data-testid="route-target-local-interval-0"]', '300');
        await flushAlpine();
        await waitFor(`document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText.includes('全局超时（候选）')`, '候选显式模式摘要更新');
        assert.match(await evaluate(`document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText || ''`), /全局超时（候选）/);

        await setValue('[data-testid="route-target-timeout-mode-0"]', '');
        await setValue('[data-testid="route-target-local-mode-0"]', '');
        await flushAlpine();
        await waitFor(`document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText.includes('客户端控制（路由）') && document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText.includes('持续重试（路由）')`, '候选继承模式摘要更新');
        assert.match(await evaluate(`document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText || ''`), /客户端控制（路由）/);
        assert.match(await evaluate(`document.querySelector('[data-testid="route-target-settings-0"]')?.closest('[data-drag-row]')?.innerText || ''`), /持续重试（路由）/);

        await setValue('[data-testid="route-target-timeout-mode-0"]', 'global');
        await setValue('[data-testid="route-target-local-mode-0"]', 'untilDisconnect');
        await flushAlpine();
        await waitForVisibility('[data-testid="route-target-local-retries-0"]', false, '候选持续模式隐藏有限次数控件');
        assert.equal(await exists('[data-testid="route-target-local-retries-0"]'), true, '候选持续模式下有限次数控件仍在 DOM 中');
        assert.equal(await visible('[data-testid="route-target-local-retries-0"]'), false, '候选持续模式隐藏次数控件');
        assert.equal(await controlValue('[data-testid="route-target-local-retries-0"]'), '2', '候选持续模式保留 maxRetries');
        await setValue('[data-testid="route-target-local-mode-0"]', 'limited');
        await flushAlpine();
        await waitForVisibility('[data-testid="route-target-local-retries-0"]', true, '候选有限模式显示次数控件');
        assert.equal(await exists('[data-testid="route-target-local-retries-0"]'), true, '候选有限模式次数控件仍在 DOM 中');
        assert.equal(await visible('[data-testid="route-target-local-retries-0"]'), true, '候选切回有限模式恢复次数控件');
        assert.equal(await controlValue('[data-testid="route-target-local-retries-0"]'), '2', '候选切回有限模式恢复原次数');

        await click('[data-testid="route-target-add"]');
        await waitFor(`Boolean(document.querySelector('[data-testid="route-target-model-1"]'))`, '第二个候选 DOM 已挂载');
        await setValue('[data-testid="route-target-model-1"]', 'fixture-second-model');
        await ensureExpanded('[data-testid="route-target-settings-1"]', '#route-target-limits-1', '第二个候选设置');
        await waitForVisibility('[data-testid="route-target-local-interval-1"]', true, '第二个候选高级区间隔控件可见');
        await setValue('[data-testid="route-target-timeout-mode-1"]', '');
        await setValue('[data-testid="route-target-local-mode-1"]', '');
        await setValue('[data-testid="route-target-local-interval-1"]', '');
        await flushAlpine();

        await click('[data-testid="route-save"]');
        await waitFor(`document.querySelector('[x-ref="statusToastStack"]')?.innerText.includes('候选 2 的持续重试有效间隔须大于 0')`, '路由 0 间隔未被候选覆盖时给出校验提示');
        assert.equal(savedPayloads.length, 0, '无效持续间隔不会提交配置');

        await setValue('[data-testid="route-target-local-interval-1"]', '900');
        await flushAlpine();
        await click('[data-testid="route-save"]');
        const saveDeadline = Date.now() + 10000;
        while (savedPayloads.length === 0 && Date.now() < saveDeadline) await sleep(50);
        assert.equal(savedPayloads.length, 1, '保存请求到达本地假接口');
        await waitFor(`getComputedStyle(document.querySelector('[x-show="showRouteModal"]')).display === 'none'`, '保存成功后路由抽屉关闭');
        assert.equal(savedPayloads.length, 1, '有效配置恰好提交一次');
        const savedRoute = savedPayloads[0].routes[0];
        assert.equal(savedRoute.timeoutMode, 'client', '保存 payload 保留 route client');
        assert.equal(savedRoute.localRetry.mode, 'untilDisconnect', '保存 payload 保留 route 持续模式');
        assert.equal(savedRoute.localRetry.maxRetries, 4, '持续模式仍保留 route maxRetries');
        assert.equal(savedRoute.localRetry.intervalMs, 0, '路由显式 0 间隔保留，允许候选覆盖');
        assert.equal(savedRoute.targets.length, 2);
        assert.equal(savedRoute.targets[0].timeoutMode, 'global', '候选显式恢复 global');
        assert.equal(savedRoute.targets[0].localRetry.mode, 'limited', '候选显式恢复 limited');
        assert.equal(savedRoute.targets[0].localRetry.maxRetries, 2, '候选有限次数保留');
        assert.equal(savedRoute.targets[1].timeoutMode, undefined, '候选超时模式继承路由');
        assert.equal(savedRoute.targets[1].localRetry.mode, undefined, '候选重试模式继承路由');
        assert.equal(savedRoute.targets[1].localRetry.intervalMs, 900, '候选正间隔覆盖路由 0');
        assert.equal(savedPayloads[0].providers.primary.localRetry.mode, undefined, 'Provider payload 不配置 mode');

        await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
        await click('[data-testid="route-edit-0"]');
        await flushAlpine();
        await waitForVisibleAndTransition('[role="dialog"][aria-labelledby="route-drawer-title"]', '[x-show="showRouteModal"]', '移动端路由抽屉已展开');
        await waitForVisibility('[data-testid="route-advanced-toggle"]', true, '移动端路由高级设置按钮可见');
        await ensureExpanded('[data-testid="route-advanced-toggle"]', '#route-advanced', '移动端路由高级设置');
        await waitForVisibility('[data-testid="route-timeout-mode"]', true, '移动端路由高级区超时控件可见');
        await clickLabelText('route-timeout-mode', '#route-advanced', '移动端路由超时模式');
        await waitFor(`document.activeElement?.id === 'route-timeout-mode'`, '移动端真实鼠标点击标签后焦点进入对应 select', 1000);
        const mobile = await evaluate(`document.querySelector('[role="dialog"][aria-labelledby="route-drawer-title"]')?.getBoundingClientRect().width`);
        assert.ok(mobile <= 390, `移动端抽屉宽度超出视口：${mobile}`);
        assert.equal(await evaluate(`document.activeElement?.id === 'route-timeout-mode'`), true, '移动端真实鼠标点击标签将键盘焦点交给对应 select');
        assert.deepEqual(exceptions, [], `浏览器出现未处理异常：${JSON.stringify(exceptions)}`);
        assert.ok(fixtureAPIRequests.every((request) => request.path.startsWith('/api/')), '所有 API 请求只落到本地假接口');
        assert.ok(fixtureAPIRequests.some((request) => request.path === '/api/config' && request.method === 'PUT'));
        t.diagnostic(`Chrome CDP 使用正式产物 ${join(webRoot, 'index.html')}；本地假 API 请求 ${fixtureAPIRequests.length} 次，无真实 config.yaml/data/上游访问。`);
    } finally {
        try { ws.close(); } catch {}
        chrome.kill('SIGTERM');
        await new Promise((resolve) => {
            if (chrome.exitCode !== null || chrome.signalCode !== null) return resolve();
            chrome.once('exit', resolve);
            setTimeout(resolve, 1000).unref?.();
        });
        await new Promise((resolve) => server.close(() => resolve()));
        rmSync(fixtureDir, { recursive: true, force: true });
    }
});
