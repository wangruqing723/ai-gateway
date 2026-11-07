#!/usr/bin/env node
'use strict';
// 只读最终构建产物；全部网络均由假 origin 拦截，不读取配置、环境文件或账户库。
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19994';
const sentinel = '__AI_GATEWAY_KEEP_BALANCE_TOKEN__';
const secret = 'fake-private-balance-token-never-persist';
const now = '2030-01-01T00:00:00Z';
const base = { baseUrl: 'https://upstream.example.invalid/v1', format: 'openai', apiKeyConfigured: true, apiKey: '__AI_GATEWAY_KEEP_API_KEY__', enabled: true };
const config = { revision: 'fake-balance', providers: {
    wallet: { ...base, balance: { type: 'newapi', source: 'account', accessToken: sentinel, accessTokenConfigured: true, quotaPerUnit: '0500000', refreshIntervalMinutes: 0 } },
    limited: { ...base, balance: { type: 'sub2api', source: 'apiKey', refreshIntervalMinutes: 0 } },
    oauth: { baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', authType: 'chatgptOAuth', accountId: 'fake-account', modelCatalog: { clientVersion: '0.167.0' }, enabled: true }
}, routes: [{ match: '*', provider: 'oauth', model: 'manual-model' }], cache: {}, failover: {}, breaker: {}, providerHealth: {} };
const wallet = { type: 'newapi', source: 'account', state: 'ok', freshness: 'fresh', lastSuccessAt: now, lastAttemptAt: now, snapshot: { queriedAt: now, items: [
    { kind: 'account_balance', source: 'newapi.user_self', remaining: '0', used: '2', limit: null, unit: 'USD', rawRemaining: '0', rawUsed: '1000000', rawUnit: 'quota', quotaPerUnit: '500000', conversionSource: 'configuration', windows: [] }
], warnings: [] } };
const quota = { type: 'sub2api', source: 'apiKey', state: 'ok', freshness: 'fresh', snapshot: { items: [
    { kind: 'key_quota', source: 'sub2api.usage', remaining: null, used: '3', limit: null, unlimited: true, unit: 'USD', windows: [] },
    { kind: 'subscription_quota', source: 'sub2api.usage', remaining: '7', used: '3', limit: '10', unit: 'USD', windows: [{ name: 'daily', remaining: '7', used: '3', limit: '10', unit: 'USD', resetAt: now }] },
    { kind: 'usage_only', remaining: null, used: '3', unit: 'USD', windows: [] }
], warnings: [] } };
(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [], requests = [], consoleMessages = [], external = [];
        let oldBackend = false, fail = false;
        page.on('pageerror', e => errors.push(e.message));
        page.on('console', m => consoleMessages.push(m.text()));
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) { external.push(req.url()); return route.abort(); }
            assert(!req.url().includes(secret), '秘密不得进入 URL');
            if (url.pathname.startsWith('/api/')) requests.push({ path: url.pathname, method: req.method(), body: req.postData() });
            if (url.pathname === '/api/config') {
                if (req.method() === 'PUT') {
                    const body = req.postDataJSON();
                    assert(body.providers.wallet.balance, '保存不得丢失余额配置');
                    assert(body.providers.oauth.modelCatalog, '保存不得丢失目录配置');
                    return route.fulfill({ json: { message: '已保存' } });
                }
                return route.fulfill({ json: config });
            }
            if (url.pathname === '/api/providers/balances') return route.fulfill(oldBackend ? { status: 404, body: '' } : { json: { revision: config.revision, providers: { wallet, limited: quota } } });
            if (url.pathname === '/api/providers/balances/refresh') {
                const input = req.postDataJSON();
                assert.equal(input.revision, config.revision);
                assert(input.providers.length <= 50);
                return route.fulfill({ json: { revision: config.revision, results: input.providers.map(provider => ({ provider, status: fail ? 'failed' : provider === 'limited' ? 'skipped' : 'cached', balance: provider === 'wallet' ? { ...wallet, state: fail ? 'failed' : 'ok', freshness: fail ? 'stale' : 'fresh', error: fail ? { code: 'upstream_timeout', message: '查询超时' } : null } : quota })) } });
            }
            if (url.pathname === '/api/providers/models' || url.pathname === '/api/providers/models/refresh') return route.fulfill({ json: { models: ['fake-oauth-model', 'another-model'], source: 'chatgpt-codex', queriedAt: now, cached: false, stale: false, message: '目录不代表模型调用成功' } });
            if (url.pathname.startsWith('/api/chatgpt/accounts')) return route.fulfill({ json: { data: [] } });
            if (url.pathname.startsWith('/api/')) return route.fulfill({ json: { data: [], providers: {}, summary: {} } });
            if (url.pathname === '/health') return route.fulfill({ json: { status: 'ok', queues: {}, providerHealth: {}, breakers: {} } });
            const file = path.resolve(root, url.pathname === '/' ? 'index.html' : url.pathname.slice(1));
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            return route.fulfill({ contentType: ({ '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' })[path.extname(file)] || 'application/octet-stream', body: fs.readFileSync(file) });
        });
        const app = fn => page.evaluate(fn);
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).providerBalances.wallet);
        assert.equal(requests.filter(r => r.path.endsWith('/balances/refresh')).length, 0, '进入页面只读本地缓存');
        const row = page.locator('[data-testid="provider-balance"][data-provider="wallet"]');
        assert.match(await row.textContent(), /0 USD/);
        assert.match(await page.locator('[data-provider="limited"]').textContent(), /不限额（仅此项）.*订阅额度（非钱包）.*仅用量/s);
        const refreshCount = requests.filter(r => r.path.endsWith('/balances/refresh')).length;
        await app(async () => { await window.Alpine.$data(document.body).refreshProviderBalances(Array.from({ length: 51 }, (_, i) => 'fake-' + i)); });
        assert.equal(requests.filter(r => r.path.endsWith('/balances/refresh')).length, refreshCount, '超过50项必须提示并阻止，不静默截断');
        assert.match(await page.locator('body').textContent(), /超过 50/);
        await app(async () => { const a = window.Alpine.$data(document.body); a.providerFilter = 'wallet'; await a.refreshProviderBalances(); });
        assert.deepEqual(JSON.parse(requests.filter(r => r.path.endsWith('/balances/refresh')).at(-1).body).providers, ['wallet']);
        fail = true;
        await app(async () => { await window.Alpine.$data(document.body).refreshProviderBalances(['wallet']); });
        assert.match(await row.textContent(), /0 USD/); assert.match(await row.textContent(), /旧值保留.*旧数据/s);
        await app(async () => { await window.Alpine.$data(document.body).editProvider('wallet'); });
        await page.getByLabel('独立余额查询令牌', { exact: true }).fill(secret);
        const checks = await app(() => {
            const a = window.Alpine.$data(document.body), b = a.providerForm.balance;
            const token = a.providerBalancePayload(b).accessToken;
            b.accessToken = ''; const keep = a.providerBalancePayload(b).accessToken;
            b.clearAccessToken = true; const clear = a.providerBalancePayload(b).accessToken;
            b.clearAccessToken = false; b.apiKeyId = 'unused'; const noKey = !('apiKeyId' in a.providerBalancePayload(b));
            return { token, keep, clear, noKey, payload: a.configPayload(), yaml: a.providersYaml() };
        });
        assert.equal(checks.token, secret); assert.equal(checks.keep, sentinel); assert.equal(checks.clear, ''); assert(checks.noKey);
        assert(checks.payload.providers.oauth.modelCatalog); assert.match(checks.yaml, /balance:/); assert.match(checks.yaml, /modelCatalog:/); assert(!checks.yaml.includes(secret));
        await app(() => { const a = window.Alpine.$data(document.body); a.showProviderModal = false; });
        await app(async () => { const a = window.Alpine.$data(document.body); await a.persistConfig(); });
        await app(async () => { const a = window.Alpine.$data(document.body); a.selectNav('routes'); a.editRoute(0); });
        await page.waitForFunction(() => window.Alpine.$data(document.body).showRouteModal);
        await app(async () => { const a = window.Alpine.$data(document.body); const t = a.routeForm.targets[0]; await a.openModelPicker('t:' + t._id, 'oauth', t.model); });
        await page.locator('button:visible').filter({ hasText: /^刷新远端模型$/ }).first().click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).modelPicker.loading);
        assert.equal(await app(() => window.Alpine.$data(document.body).routeForm.targets[0].model), 'manual-model', '刷新不能覆盖手填模型');
        await app(() => { const a = window.Alpine.$data(document.body); a.showRouteModal = false; a.beginInlineEdit(0, 0, 'manual-model', 'oauth'); });
        await page.waitForFunction(() => !window.Alpine.$data(document.body).inlineModel.modelsLoading);
        await page.locator('#inline-model-input').press('ArrowDown');
        await app(async () => { await window.Alpine.$data(document.body).loadInlineModels(true); });
        assert(requests.filter(r => r.path.endsWith('/models/refresh')).length >= 2, '两个入口都明确调用刷新接口');
        const storage = await app(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage)]));
        assert(!storage.includes(secret)); assert(!storage.includes('fake-oauth-model'), 'OAuth目录不得持久化');
        assert(!consoleMessages.join('\n').includes(secret)); assert.deepEqual(external, []);
        await app(() => window.Alpine.$data(document.body).cancelInlineModel());
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            await app(async () => { const a = window.Alpine.$data(document.body); a.selectNav('providers'); await a.editProvider('wallet'); });
            await page.getByLabel('独立余额查询令牌', { exact: true }).focus(); await page.keyboard.press('Tab');
            assert(await page.evaluate(() => document.activeElement?.tagName !== 'BODY'), '键盘焦点可达');
            assert(await page.locator('[x-ref="providerDrawer"]').evaluate(el => el.scrollWidth <= el.clientWidth), `${width}px 抽屉不得横向溢出`);
            await app(() => { window.Alpine.$data(document.body).statusToasts = []; });
            await page.getByLabel('独立余额查询令牌', { exact: true }).scrollIntoViewIfNeeded();
            await page.waitForFunction(() => {
                const drawer = document.querySelector('[x-ref="providerDrawer"]');
                return drawer && getComputedStyle(drawer).opacity === '1' && !drawer.getAnimations({ subtree: true }).some(animation => animation.playState === 'running');
            });
            await page.screenshot({ path: `/tmp/provider-models-balance-${width}.png` });
            await app(() => { window.Alpine.$data(document.body).showProviderModal = false; });
        }
        oldBackend = true;
        await app(async () => { await window.Alpine.$data(document.body).loadProviderBalances(); });
        assert.equal(await app(() => window.Alpine.$data(document.body).providerBalancesSupported), false);
        assert.deepEqual(errors, []); assert(!consoleMessages.some(m => /Alpine Expression Error/.test(m)));
        console.log('通过：只读缓存、筛选批量、真实零与口径、失败保留、令牌保留清除、完整保存、OAuth双入口刷新、键盘、1440/390、旧后端与秘密隔离');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
