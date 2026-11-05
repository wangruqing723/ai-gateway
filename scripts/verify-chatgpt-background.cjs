#!/usr/bin/env node
'use strict';

// 验证第六阶段前端：ChatGPT 后台刷新设置的回读回传、设置页状态行（含旧后端 404 兼容）、
// 账户详情「后台计划」小节的有 / 无字段两种情况，以及 1440 / 390 宽度截图。
// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-background.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19993';
const iso = offsetMs => new Date(Date.now() + offsetMs).toISOString().replace(/\.\d{3}Z$/, 'Z');
const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;

const account = (n, extra) => ({
    id: `fake-${n}`, email: `fake${n}@example.invalid`, plan: 'plus', status: 'ready', enabled: true, expiresAt: iso(10 * 24 * HOUR),
    lastRefresh: iso(-HOUR), tags: [], providers: ['codex'], usageState: { state: 'unknown', freshness: 'none', message: '尚未查询用量' },
    invocation: { state: 'unknown', message: '尚未实测' }, authorization: { state: 'valid', canRefresh: true, message: '访问令牌有效' }, ...extra
});
const scope = () => ({
    requests: 1, successes: 1, failures: 0, localRetries: 0, rateLimited: 0, successRate: 1, latencySamples: 1, p50LatencyMs: 300, p95LatencyMs: 300,
    skips: { accountPaused: 0, authorization: 0, queue: 0, cooldown: 0 },
    diagnostics: { successes: 0, failures: 0 }, refresh: { successes: 0, failures: 0 }, usage: { successes: 0, failures: 0 }
});
const config = {
    revision: 'fake-1', failover: {}, breaker: {}, providerHealth: {}, cache: {},
    chatgptAccountLimits: { sharedCooldown: true },
    chatgptBackground: { tokenRefresh: true, usageIntervalMinutes: 45 },
    providers: {
        codex: { baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', authType: 'chatgptOAuth', accountId: 'fake-1', maxConcurrent: 2, maxPerSecond: 0, maxQueueWait: 30000 }
    },
    routes: [{ match: 'gpt-*', targets: [{ provider: 'codex', model: 'gpt-6-luna' }] }]
};
const backgroundOf = config => ({
    tokenRefresh: config.chatgptBackground?.tokenRefresh === true,
    usageRefresh: config.chatgptBackground?.usageRefresh === true
});

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', error => errors.push(error.message));
        const state = {
            accounts: [
                account(1, { background: {
                    active: true, lastUsedAt: iso(-10 * MINUTE), skipped: '',
                    token: { lastRunAt: iso(-5 * MINUTE), lastResult: 'ok', nextAt: iso(50 * MINUTE), failures: 0 },
                    usage: { lastRunAt: iso(-3 * MINUTE), lastResult: 'error', lastMessage: '用量查询返回 503', nextAt: iso(87 * MINUTE), failures: 2 }
                } }),
                account(2, {}),
                account(3, { background: { active: false, skipped: 'inactive' } })
            ],
            puts: [],
            backgroundGets: 0,
            // 'running' | 'unsupported' | 'disabled' | 'missing'（旧后端 404）
            backgroundMode: 'running',
            config: JSON.parse(JSON.stringify(config))
        };
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            const json = (body, status = 200) => route.fulfill({ status, json: body });
            if (url.pathname === '/api/chatgpt/background') {
                state.backgroundGets++;
                if (state.backgroundMode === 'missing') return route.fulfill({ status: 404, body: '404 page not found' });
                const flags = backgroundOf(state.config);
                if (state.backgroundMode === 'unsupported') return json({ data: { ...flags, running: false, crossProcessLocking: false, reason: 'unsupported_platform', lastTickAt: '' } });
                if (state.backgroundMode === 'disabled') return json({ data: { tokenRefresh: false, usageRefresh: false, running: false, crossProcessLocking: true, reason: '', lastTickAt: '' } });
                return json({ data: { ...flags, running: true, crossProcessLocking: true, reason: '', lastTickAt: iso(-20 * 1000) } });
            }
            if (url.pathname === '/api/chatgpt/accounts') return json({ data: state.accounts });
            if (url.pathname === '/api/chatgpt/accounts/events') return json({ data: [] });
            const metrics = url.pathname.match(/^\/api\/chatgpt\/accounts\/([^/]+)\/metrics$/);
            if (metrics) {
                const id = decodeURIComponent(metrics[1]);
                return json({ data: { accountId: id, windowMinutes: 15, since: iso(-HOUR), window: scope(), total: scope(), events: [] } });
            }
            if (url.pathname.startsWith('/api/chatgpt/')) return json({ data: state.accounts });
            if (url.pathname === '/api/metrics') return json({ events: [], vision: {} });
            if (url.pathname === '/api/config' && req.method() === 'PUT') {
                // PUT 是全量替换：假后端只保留最近一次请求体，GET 原样返回，未下发的块随之消失。
                state.puts.push(JSON.parse(req.postData() || '{}'));
                state.config = { ...JSON.parse(JSON.stringify(state.puts.at(-1))), revision: 'fake-' + (state.puts.length + 1) };
                return json({ message: '配置已保存' });
            }
            if (url.pathname === '/api/config') return json(state.config);
            if (url.pathname === '/health') return json({ status: 'ok', queues: {}, providerHealth: {}, breakers: null, providerKeys: {} });
            if (url.pathname.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const file = path.resolve(root, url.pathname === '/' ? 'index.html' : url.pathname.slice(1));
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        const data = fn => page.evaluate(fn);
        const plain = value => JSON.parse(JSON.stringify(value ?? null));
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 3);
        await page.waitForFunction(() => window.Alpine.$data(document.body).revision === 'fake-1');
        const settle = () => page.waitForFunction(() => !window.Alpine.$data(document.body)._navScrollRestoreTab).then(() => page.waitForTimeout(400));

        // 1. 配置回读与回传：只回传显式开启的布尔与已填写的周期；未改动时不显示未保存。
        assert.deepEqual(plain(await data(() => window.Alpine.$data(document.body).config.chatgptBackground)),
            { tokenRefresh: true, usageRefresh: false, usageIntervalMinutes: 45, activeWindowMinutes: null });
        assert.deepEqual(plain(await data(() => window.Alpine.$data(document.body).configPayload().chatgptBackground)), { tokenRefresh: true, usageIntervalMinutes: 45 });
        assert.equal(await data(() => window.Alpine.$data(document.body).isDirty()), false, '回读后不应显示未保存');
        assert.equal(state.backgroundGets, 0, '不在设置页时不读取后台状态');
        // 旧后端整体缺失 chatgptBackground：回落为全部关闭，payload 整块省略。
        const legacy = await data(() => {
            const app = window.Alpine.$data(document.body);
            const normalized = app.normalizeChatGPTBackground(undefined);
            const saved = app.config.chatgptBackground;
            app.config.chatgptBackground = normalized;
            const payload = app.configPayload();
            app.config.chatgptBackground = saved;
            return { normalized, has: Object.prototype.hasOwnProperty.call(JSON.parse(JSON.stringify(payload)), 'chatgptBackground') };
        });
        assert.deepEqual(plain(legacy.normalized), { tokenRefresh: false, usageRefresh: false, usageIntervalMinutes: null, activeWindowMinutes: null });
        assert.equal(legacy.has, false, '全部默认时不下发 chatgptBackground');
        assert.equal(await data(() => window.Alpine.$data(document.body).isDirty()), false);
        console.log('通过：chatgptBackground 回读回传，缺失时回落为关闭且不下发，未改动不显示未保存');

        // 2. 账户详情：有 background 字段时显示后台计划；没有字段时整块不渲染。
        const bodyOf = email => page.locator('tbody', { has: page.getByText(email, { exact: true }) });
        await bodyOf('fake1@example.invalid').locator('tr').first().click();
        const plan = bodyOf('fake1@example.invalid').getByTestId('chatgpt-account-background');
        await plan.waitFor({ state: 'visible' });
        const keys = await plan.getByTestId('chatgpt-background-row').evaluateAll(rows => rows.map(row => row.dataset.background));
        assert.deepEqual(keys, ['state', 'lastUsed', 'token', 'usage']);
        const planText = await plan.textContent();
        assert.match(planText, /参与后台刷新（活跃账户）/);
        assert.match(planText, /上次 .+ 成功/);
        assert.match(planText, /用量查询返回 503/);
        assert.match(planText, /连续失败 2 次/);
        assert.match(planText, /下次 /);
        await plan.scrollIntoViewIfNeeded();
        await page.screenshot({ path: '/tmp/chatgpt-background-account-1440.png', animations: 'disabled' });
        await bodyOf('fake2@example.invalid').locator('tr').first().click();
        await bodyOf('fake2@example.invalid').getByTestId('chatgpt-account-stats').waitFor({ state: 'visible' });
        assert.equal(await bodyOf('fake2@example.invalid').getByTestId('chatgpt-account-background').count(), 0, '没有 background 字段时不显示后台计划');
        await bodyOf('fake3@example.invalid').locator('tr').first().click();
        const skipped = bodyOf('fake3@example.invalid').getByTestId('chatgpt-account-background');
        await skipped.waitFor({ state: 'visible' });
        const skippedKeys = await skipped.getByTestId('chatgpt-background-row').evaluateAll(rows => rows.map(row => row.dataset.background));
        assert.deepEqual(skippedKeys, ['state', 'lastUsed'], '功能关闭时省略 token / usage 行');
        assert.match(await skipped.textContent(), /跳过：闲置/);
        assert.match(await skipped.textContent(), /本次启动后尚无真实调用/);
        console.log('通过：账户详情后台计划显示参与 / 跳过、最近使用、令牌与用量结果；无字段时不渲染');

        // 3. 设置页：进入时读取状态（运行中），编辑后 payload 留空不下发、不发 0。
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('settings'));
        await settle();
        const section = page.getByTestId('chatgpt-background');
        await section.scrollIntoViewIfNeeded();
        assert(await section.isVisible());
        const status = page.getByTestId('chatgpt-background-status');
        await status.waitFor({ state: 'visible' });
        assert.equal(await status.getAttribute('data-status'), 'running');
        assert.match(await status.textContent(), /运行中.*令牌预刷新.*最近检查/s);
        assert.equal(state.backgroundGets, 1, '进入设置页读取一次后台状态');
        assert.equal(await page.getByTestId('chatgpt-background-token-refresh').isChecked(), true);
        assert.equal(await page.getByTestId('chatgpt-background-usage-interval').inputValue(), '45');
        assert.equal(await page.getByTestId('chatgpt-background-active-window').getAttribute('placeholder'), '留空用默认值 60，范围 5-1440');
        await page.getByTestId('chatgpt-background-usage-refresh').check();
        await page.getByTestId('chatgpt-background-token-refresh').uncheck();
        await page.getByTestId('chatgpt-background-usage-interval').fill('');
        await page.getByTestId('chatgpt-background-active-window').fill('0');
        assert.deepEqual(plain(await data(() => window.Alpine.$data(document.body).configPayload().chatgptBackground)), { usageRefresh: true }, '留空与 0 不下发，false 不下发');
        assert.equal(await data(() => window.Alpine.$data(document.body).isDirty()), true);
        await page.getByTestId('chatgpt-background-active-window').fill('120');
        assert.deepEqual(plain(await data(() => window.Alpine.$data(document.body).configPayload().chatgptBackground)), { usageRefresh: true, activeWindowMinutes: 120 });
        await section.scrollIntoViewIfNeeded();
        await page.screenshot({ path: '/tmp/chatgpt-background-settings-1440.png', animations: 'disabled' });
        console.log('通过：设置页进入时读取运行中状态；编辑后留空、0 与 false 均不下发');

        // 4. 保存：PUT 带上后台设置；保存成功后重新读取状态（切到平台不支持）；回读后不显示未保存。
        state.backgroundMode = 'unsupported';
        await page.evaluate(() => window.Alpine.$data(document.body).saveConfig());
        await page.waitForFunction(() => window.Alpine.$data(document.body).revision === 'fake-2');
        assert.deepEqual(state.puts.at(-1).chatgptBackground, { usageRefresh: true, activeWindowMinutes: 120 });
        assert.deepEqual(state.puts.at(-1).chatgptAccountLimits, { sharedCooldown: true }, '其他块照常回传');
        await page.waitForFunction(() => document.querySelector('[data-testid="chatgpt-background-status"]')?.dataset.status === 'unsupported');
        assert.match(await status.textContent(), /平台不支持跨进程文件锁/);
        assert.deepEqual(plain(await data(() => window.Alpine.$data(document.body).config.chatgptBackground)),
            { tokenRefresh: false, usageRefresh: true, usageIntervalMinutes: null, activeWindowMinutes: 120 });
        assert.equal(await data(() => window.Alpine.$data(document.body).isDirty()), false, '保存回读后不应显示未保存');
        console.log('通过：保存下发后台设置并回读；保存成功后重新读取状态（平台不支持跨进程锁未启动）');

        // 5. 未启用状态；随后旧后端 404 时状态行隐藏且不弹错误。
        state.backgroundMode = 'disabled';
        await page.evaluate(() => window.Alpine.$data(document.body).loadChatGPTBackgroundStatus());
        await page.waitForFunction(() => document.querySelector('[data-testid="chatgpt-background-status"]')?.dataset.status === 'disabled');
        assert.match(await status.textContent(), /未启用/);
        state.backgroundMode = 'missing';
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('monitor'));
        await settle();
        const before = state.backgroundGets;
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('settings'));
        await settle();
        await page.waitForFunction(() => window.Alpine.$data(document.body).chatGPTBackgroundStatus === null);
        assert.equal(state.backgroundGets, before + 1, '重新进入设置页再读一次');
        assert.equal(await status.isVisible(), false, '404 时不显示状态行');
        const errorToasts = await data(() => window.Alpine.$data(document.body).statusToasts.filter(item => item.type === 'error').map(item => item.message));
        assert.deepEqual(errorToasts, [], '404 不报错');
        console.log('通过：未启用状态；旧后端 404 时不显示状态行、不弹错误');

        // 6. 窄屏截图：设置区块与账户后台计划不横向溢出。
        state.backgroundMode = 'running';
        await page.evaluate(() => window.Alpine.$data(document.body).loadChatGPTBackgroundStatus());
        await status.waitFor({ state: 'visible' });
        await page.setViewportSize({ width: 390, height: 900 });
        await section.scrollIntoViewIfNeeded();
        const overflow = el => el.evaluate(node => {
            // 既看自身内容是否超出，也看区块是否超出视口右边界。
            const rect = node.getBoundingClientRect();
            return Math.max(node.scrollWidth - node.clientWidth, Math.ceil(rect.right - document.documentElement.clientWidth));
        });
        assert(await overflow(section) <= 1, '390px 下设置区块不应横向溢出');
        await page.screenshot({ path: '/tmp/chatgpt-background-settings-390.png', animations: 'disabled' });
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('providers'));
        await settle();
        await page.evaluate(() => { const app = window.Alpine.$data(document.body); if (app.expandedChatGPTAccountId !== 'fake-1') app.toggleChatGPTAccount(app.chatgptAccounts[0]); });
        const narrowPlan = bodyOf('fake1@example.invalid').getByTestId('chatgpt-account-background');
        await narrowPlan.waitFor({ state: 'visible' });
        await narrowPlan.scrollIntoViewIfNeeded();
        const planOverflow = await overflow(narrowPlan);
        const planBox = await narrowPlan.evaluate(node => { const r = node.getBoundingClientRect(); return { left: r.left, right: r.right, scrollWidth: node.scrollWidth, clientWidth: node.clientWidth }; });
        assert(planOverflow <= 1, '390px 下后台计划不应横向溢出：' + JSON.stringify(planBox));
        await page.screenshot({ path: '/tmp/chatgpt-background-account-390.png', animations: 'disabled' });
        assert.deepEqual(errors, []);
        console.log('通过：窄屏截图已生成；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
