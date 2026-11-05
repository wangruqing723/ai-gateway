#!/usr/bin/env node
'use strict';

// 验证第四阶段的账户调用统计、账户事件与「需要关注」提醒的前端交互。
// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据，不发系统通知。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-observe.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19993';
const iso = offsetMs => new Date(Date.now() + offsetMs).toISOString().replace(/\.\d{3}Z$/, 'Z');
const HOUR = 3600 * 1000;

const fresh = { state: 'ok', freshness: 'fresh', message: 'ok' };
const usage = remaining => ({
    windows: [{ name: 'primary', limitWindowSeconds: 18000, remainingPercent: remaining, usedPercent: 100 - remaining, resetAt: iso(2 * HOUR) }],
    lastAttemptAt: iso(-60000), lastSuccessAt: iso(-60000), lastCheckedAt: iso(-60000)
});
const account = (n, extra) => ({
    id: `fake-${n}`, email: `fake${n}@example.invalid`, plan: 'plus', status: 'ready', enabled: true, expiresAt: iso(10 * 24 * HOUR),
    lastRefresh: iso(-HOUR), tags: [], providers: [], usage: usage(60), usageState: fresh,
    invocation: { state: 'unknown', message: '尚未实测' },
    authorization: { state: 'valid', canRefresh: true, message: '访问令牌有效' }, ...extra
});
const initialAccounts = () => [
    // 额度 8%：新鲜快照，提醒「额度偏低」；最后成功较早。
    account(1, { usage: usage(8), invocation: { state: 'failure', message: '失败', httpStatus: 429, lastSuccessAt: iso(-2 * HOUR) } }),
    account(2, {
        status: 'reauthorization_required', invocation: { state: 'success', message: 'ok', observedAt: iso(-60000), lastSuccessAt: iso(-60000) },
        authorization: { state: 'reauthorization_required', canRefresh: false, message: '刷新凭据已失效，需要重新授权', lastRefreshFailedAt: iso(-5 * 60000) }
    }),
    // 最近调用是诊断成功（observedAt 很新），但没有统计口径的最后成功时间：排序应视为未知。
    account(3, {
        invocation: { state: 'success', message: 'ok', observedAt: iso(-1000) },
        authorization: { state: 'refreshable', canRefresh: true, message: 'x', lastRefreshFailedAt: iso(-10 * 60000), lastRefreshError: '最近一次自动刷新超时；下次使用时会重试' }
    }),
    // 快照过时：即使剩余 5% 也不提醒额度偏低。
    account(4, { usage: usage(5), usageState: { ...fresh, freshness: 'stale' } }),
    account(5, { authorization: { state: 'valid', canRefresh: true, message: '访问令牌有效', credentialsUnsaved: true } }),
    account(6, { usage: usage(3) }),
    // 窗口重置已到待复查：不提醒额度偏低。
    account(7, { usage: usage(2), usageState: { ...fresh, resetPendingWindows: ['primary'] } })
];

const scope = extra => ({
    requests: 0, successes: 0, failures: 0, localRetries: 0, rateLimited: 0, successRate: null, latencySamples: 0,
    p50LatencyMs: null, p95LatencyMs: null, skips: { accountPaused: 0, authorization: 0, queue: 0 },
    diagnostics: { successes: 0, failures: 0 }, refresh: { successes: 0, failures: 0 }, usage: { successes: 0, failures: 0 }, ...extra
});
const accountStats = {
    'fake-1': {
        accountId: 'fake-1', windowMinutes: 15, since: iso(-3 * HOUR),
        window: scope({ requests: 12, successes: 10, failures: 2, localRetries: 1, rateLimited: 1, successRate: 10 / 12, latencySamples: 11,
            p50LatencyMs: 500, p95LatencyMs: 2000, skips: { accountPaused: 1, authorization: 0, queue: 2 }, diagnostics: { successes: 1, failures: 0 } }),
        total: scope({ requests: 40, successes: 40, successRate: 1, latencySamples: 40, p50LatencyMs: 500, p95LatencyMs: 1000 }),
        lastSuccessAt: iso(-2 * HOUR), lastFailureAt: iso(-60000),
        events: [{ id: 'e2', time: '2026-11-04 08:10:00', kind: 'account_paused', provider: '', accountIds: ['fake-1'], account: 'fake1@example.invalid', detail: '账户已暂停，新请求不再准入', recovered: false }]
    },
    'fake-2': { accountId: 'fake-2', windowMinutes: 15, since: iso(-3 * HOUR), window: scope({}), total: scope({}), events: [] }
};
const recoveredEvent = { id: 'e9', time: '2026-11-04 08:20:00', kind: 'account_recovered', provider: 'codex', accountIds: ['fake-6'], account: 'fake6@example.invalid', detail: '凭据替换后的新调用已成功', recovered: true };
const runEvents = [
    { id: 'e3', time: '2026-11-04 08:30:00', kind: 'account_paused', provider: '', accountIds: ['fake-1'], account: 'fake1@example.invalid', detail: '账户已暂停，新请求不再准入', recovered: false },
    { id: 'e4', time: '2026-11-04 08:31:00', kind: 'account_reauth_required', provider: 'codex', accountIds: ['fake-2'], account: 'fake2@example.invalid', detail: '刷新凭据被拒，需要重新授权', recovered: false }
];
const config = {
    revision: 'fake', failover: {}, breaker: {}, providerHealth: {}, cache: {},
    providers: { codex: { baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', authType: 'chatgptOAuth', accountId: 'fake-1', maxConcurrent: 2, maxPerSecond: 0, maxQueueWait: 30000 } },
    routes: [{ match: 'gpt-*', provider: 'codex', model: 'gpt-6-luna' }]
};

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', error => errors.push(error.message));
        const state = { accounts: initialAccounts(), events: [recoveredEvent, { ...runEvents[1] }], failMetrics: false, requests: [] };
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            state.requests.push(url.pathname);
            const json = (body, status = 200) => route.fulfill({ status, json: body });
            if (url.pathname === '/api/chatgpt/accounts') return json({ data: state.accounts });
            if (url.pathname === '/api/chatgpt/accounts/events') return json({ data: state.events });
            const metricsMatch = url.pathname.match(/^\/api\/chatgpt\/accounts\/([^/]+)\/metrics$/);
            if (metricsMatch) {
                if (state.failMetrics) return json({ error: { type: 'internal_error', message: '假后端故障' } }, 500);
                const data = accountStats[decodeURIComponent(metricsMatch[1])];
                return data ? json({ data }) : json({ error: { type: 'account_not_found', message: 'ChatGPT OAuth 账户不存在' } }, 404);
            }
            if (url.pathname.startsWith('/api/chatgpt/')) return json({ data: state.accounts });
            if (url.pathname === '/api/metrics') return json({ events: runEvents, vision: {} });
            if (url.pathname === '/api/config') return json(config);
            if (url.pathname === '/health') return json({ status: 'ok', queues: {}, providerHealth: {}, breakers: null, providerKeys: {} });
            if (url.pathname.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const file = path.resolve(root, url.pathname === '/' ? 'index.html' : url.pathname.slice(1));
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        const app = fn => page.evaluate(fn);
        const ready = async () => {
            await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 7);
            await page.waitForFunction(() => window.Alpine.$data(document.body).chatGPTAccountEvents.length > 0);
        };
        await page.goto(origin + '/#providers');
        await ready();
        const panel = page.getByTestId('chatgpt-reminders');
        const reminders = () => panel.getByTestId('chatgpt-reminder').evaluateAll(items => items.map(item =>
            item.querySelector('.tag').textContent.trim() + ' ' + item.querySelector('button').textContent.trim()));
        await panel.waitFor({ state: 'visible' });
        assert.deepEqual(await reminders(), [
            '需要重新授权 fake2@example.invalid', '额度偏低 fake1@example.invalid', '自动刷新失败 fake3@example.invalid',
            '新凭据未保存 fake5@example.invalid', '额度偏低 fake6@example.invalid', '账户已恢复 fake6@example.invalid'
        ]);
        assert.equal(await page.getByTestId('chatgpt-reminder-count').textContent(), '6');
        console.log('通过：提醒按严重程度排序，过时快照与重置待复查不提醒额度偏低，未解决的异常事件不当作恢复');

        // 单条已读写入本机并跨刷新保留；证据时间变化后重新提醒。
        await panel.getByTestId('chatgpt-reminder').nth(0).getByTestId('chatgpt-reminder-read').click();
        assert.equal(await page.getByTestId('chatgpt-reminder-count').textContent(), '5');
        assert.equal(JSON.parse(await app(() => localStorage.getItem('gw.chatgptReminderRead'))).length, 1);
        await page.reload();
        await ready();
        assert.equal(await page.getByTestId('chatgpt-reminder-count').textContent(), '5');
        await panel.getByTestId('chatgpt-reminder').filter({ hasText: '自动刷新失败' }).getByTestId('chatgpt-reminder-read').click();
        assert.equal(await page.getByTestId('chatgpt-reminder-count').textContent(), '4');
        state.accounts.find(item => item.id === 'fake-3').authorization.lastRefreshFailedAt = iso(-1000);
        await app(() => window.Alpine.$data(document.body).loadChatGPTAccounts());
        await page.waitForFunction(() => window.Alpine.$data(document.body).unreadChatGPTReminders().length === 5);
        assert((await reminders()).includes('自动刷新失败 fake3@example.invalid'));
        assert(!(await reminders()).includes('需要重新授权 fake2@example.invalid'), '凭据未变化时重新授权提醒保持已读');
        console.log('通过：已读按「条件 + 证据时间」跨刷新保留，新的失败时间重新提醒');

        // 展开账户：读取统计并标注窗口、样本量；没有样本不以 0 冒充。
        const rowOf = email => page.locator('tbody', { has: page.getByText(email, { exact: true }) });
        await rowOf('fake1@example.invalid').locator('tr').first().click();
        const stats = rowOf('fake1@example.invalid').getByTestId('chatgpt-account-stats');
        await stats.getByTestId('chatgpt-stat-row').first().waitFor({ state: 'visible' });
        const cells = async (root, key) => (await root.locator(`[data-stat="${key}"] td`).allTextContents()).map(text => text.trim());
        assert.match(await stats.locator('thead').textContent(), /最近 15 分钟.*本次启动以来/s);
        assert.deepEqual(await cells(stats, 'requests'), ['12', '40']);
        assert.deepEqual(await cells(stats, 'successRate'), ['83.3%（10/12）', '100%（40/40）']);
        assert.deepEqual(await cells(stats, 'latency'), ['P50 500ms（11 个样本）', 'P50 500ms · P95 1.0s（40 个样本）']);
        assert.deepEqual(await cells(stats, 'skips'), ['3（暂停 1 · 取令牌 0 · 排队 2）', '0']);
        assert.notEqual(await stats.getByTestId('chatgpt-stats-last-success').textContent(), '本次启动后尚无');
        assert.match(await stats.getByTestId('chatgpt-account-event').first().textContent(), /账户暂停.*账户已暂停/s);
        await rowOf('fake2@example.invalid').locator('tr').first().click();
        const empty = rowOf('fake2@example.invalid').getByTestId('chatgpt-account-stats');
        await empty.getByTestId('chatgpt-stat-row').first().waitFor({ state: 'visible' });
        assert.deepEqual(await cells(empty, 'requests'), ['0', '0']);
        assert.deepEqual(await cells(empty, 'successRate'), ['暂无样本', '暂无样本']);
        assert.deepEqual(await cells(empty, 'latency'), ['暂无样本', '暂无样本']);
        assert.equal(await empty.getByTestId('chatgpt-stats-last-success').textContent(), '本次启动后尚无');
        // 刷新失败保留上次结果，只追加错误说明。
        state.failMetrics = true;
        await empty.getByTestId('chatgpt-stats-refresh').click();
        await empty.getByTestId('chatgpt-stats-error').waitFor({ state: 'visible' });
        assert.match(await empty.getByTestId('chatgpt-stats-error').textContent(), /假后端故障.*上次读取的结果/);
        assert.equal(await empty.getByTestId('chatgpt-stat-row').count(), 9);
        state.failMetrics = false;
        console.log('通过：账户详情显示窗口与累计统计、样本量、最后成功和最近事件；无样本显示暂无样本；失败保留旧结果');

        // 定位：被筛选隐藏时只提示，不改动筛选；可见时展开并读取统计。
        await app(() => { window.Alpine.$data(document.body).chatGPTAccountFilter = 'fake6'; });
        await panel.getByTestId('chatgpt-reminder').filter({ hasText: '额度偏低' }).filter({ hasText: 'fake1@' }).locator('button').first().click();
        assert.match(await app(() => window.Alpine.$data(document.body).statusToasts.at(-1).message), /被当前筛选隐藏/);
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTAccountFilter), 'fake6');
        await app(() => { window.Alpine.$data(document.body).chatGPTAccountFilter = ''; });
        const metricsBefore = state.requests.filter(p => p.endsWith('/fake-6/metrics')).length;
        await panel.getByTestId('chatgpt-reminder').filter({ hasText: '额度偏低' }).filter({ hasText: 'fake6@' }).locator('button').first().click();
        await page.waitForFunction(() => window.Alpine.$data(document.body).expandedChatGPTAccountId === 'fake-6');
        await page.waitForFunction(() => document.activeElement?.dataset?.chatgptAccountRow === 'fake-6');
        assert(state.requests.filter(p => p.endsWith('/fake-6/metrics')).length > metricsBefore, '定位展开后应读取统计');
        await rowOf('fake6@example.invalid').getByTestId('chatgpt-stats-error').waitFor({ state: 'visible' });
        console.log('通过：提醒定位遵守当前筛选，展开后读取统计并把焦点移到账户行');

        // 「最近调用成功时间」排序取统计口径的最后成功时间，诊断写回的最近调用不参与。
        await app(() => { const a = window.Alpine.$data(document.body); a.chatGPTAccountFiltersOpen = true; a.chatGPTAccountSort = 'lastSuccess'; a.chatGPTAccountSortDirection = 'desc'; });
        const order = await app(() => window.Alpine.$data(document.body).filteredChatGPTAccounts().map(a => a.id));
        assert.deepEqual(order.slice(0, 2), ['fake-2', 'fake-1']);
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTAccountSortUnknownCount()), 5);
        await page.getByText('5 个账户本次启动后尚无成功的真实调用，统一排在最后。').waitFor({ state: 'visible' });
        await app(() => { const a = window.Alpine.$data(document.body); a.chatGPTAccountSort = 'identity'; a.chatGPTAccountFiltersOpen = false; });
        console.log('通过：最近成功排序改用统计口径，未知统一置后并说明原因');

        // 全部已读后面板隐藏；新的恢复事件重新提醒。上限 200 时优先保留仍有效的已读项。
        await page.getByTestId('chatgpt-reminders-read-all').click();
        await panel.waitFor({ state: 'hidden' });
        await page.reload();
        await ready();
        assert.equal(await panel.isVisible(), false);
        await app(() => {
            const a = window.Alpine.$data(document.body);
            a.chatGPTReminderReadKeys = [...Array.from({ length: 250 }, (_, i) => 'stale|' + i), ...a.chatGPTReminderReadKeys];
            a.markChatGPTRemindersRead(a.chatGPTReminderItems()[0]);
        });
        const kept = await app(() => {
            const a = window.Alpine.$data(document.body);
            return { length: a.chatGPTReminderReadKeys.length, unread: a.unreadChatGPTReminders().length };
        });
        assert.deepEqual(kept, { length: 200, unread: 0 });
        state.events = [{ ...recoveredEvent, id: 'e10', time: '2026-11-04 09:00:00' }, ...state.events];
        await app(() => window.Alpine.$data(document.body).loadChatGPTAccounts());
        await panel.waitFor({ state: 'visible' });
        assert.deepEqual(await reminders(), ['账户已恢复 fake6@example.invalid']);
        console.log('通过：全部已读跨刷新保留，已读上限保留有效项，新的恢复事件重新提醒');

        // 监控页事件行显示账户维度与中文标签；凭据异常与管理动作用不同颜色。
        await app(() => window.Alpine.$data(document.body).selectNav('monitor'));
        // 只取状态事件行本身，避免匹配到包含它的外层网格。
        const eventRowOf = time => page.locator('div.grid.min-w-\\[620px\\]', { hasText: time }).first();
        const eventRow = eventRowOf('2026-11-04 08:30:00');
        await eventRow.waitFor({ state: 'visible' });
        assert.match(await eventRow.textContent(), /账户暂停.*fake1@example\.invalid · 账户已暂停/s);
        const reauthTag = eventRowOf('2026-11-04 08:31:00').locator('.tag');
        assert.equal(await reauthTag.textContent(), '需重新授权');
        assert.match(await reauthTag.getAttribute('class'), /bg-danger\/10/);
        assert.match(await eventRow.locator('.tag').getAttribute('class'), /bg-white\/\[\.04\]/);
        console.log('通过：运行事件显示账户维度，异常与管理动作区分颜色');

        await app(() => window.Alpine.$data(document.body).selectNav('providers'));
        await page.waitForFunction(() => !window.Alpine.$data(document.body)._navScrollRestoreTab);
        // 截图前等切页淡入与滚动结束，并关闭动画，避免截到只绘制了一部分的过渡帧。
        const settle = async () => {
            await page.waitForFunction(() => !window.Alpine.$data(document.body)._navScrollRestoreTab);
            await page.waitForTimeout(400);
        };
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            await panel.evaluate(el => el.scrollIntoView({ block: 'start' }));
            await settle();
            assert(await panel.isVisible(), `${width}px 下提醒面板应可见`);
            await page.screenshot({ path: `/tmp/chatgpt-observe-${width}.png`, animations: 'disabled' });
        }
        // 展开带样本的账户，单独截取统计区，检查表格与事件列表的实际排版。
        await page.setViewportSize({ width: 1440, height: 900 });
        await app(() => { const a = window.Alpine.$data(document.body); a.expandedChatGPTAccountId = ''; a.toggleChatGPTAccount(a.chatgptAccounts[0]); });
        const statsShot = rowOf('fake1@example.invalid').getByTestId('chatgpt-account-stats');
        await statsShot.getByTestId('chatgpt-stat-row').first().waitFor({ state: 'visible' });
        await statsShot.evaluate(el => el.scrollIntoView({ block: 'center' }));
        await settle();
        await statsShot.screenshot({ path: '/tmp/chatgpt-observe-stats-1440.png', animations: 'disabled' });
        assert.deepEqual(errors, []);
        console.log('通过：桌面与窄屏截图已生成；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
