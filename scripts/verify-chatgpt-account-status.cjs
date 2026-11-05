#!/usr/bin/env node
'use strict';

// 验证 ChatGPT 账户的授权 / 实际调用 / 用量新鲜度三个状态维度，以及 OAuth Provider 的「未实测」健康档位。
// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-account-status.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19998';
const iso = (offsetMs) => new Date(Date.now() + offsetMs).toISOString().replace(/\.\d{3}Z$/, 'Z');
const HOUR = 3600 * 1000;
const unknownInvocation = { state: 'unknown', message: '尚无经网关发出的真实调用记录' };
const freshUsage = (windows = []) => ({ windows, lastAttemptAt: iso(-60000), lastSuccessAt: iso(-60000), lastCheckedAt: iso(-60000) });
const okUsageState = { state: 'ok', freshness: 'fresh', message: '用量数据来自最近一次成功查询' };
const account = (n, extra) => ({
    id: `fake-${n}`, email: `fake${n}@example.invalid`, plan: 'plus', status: 'ready', expiresAt: iso(10 * 24 * HOUR),
    usage: freshUsage(), usageState: okUsageState, invocation: unknownInvocation,
    authorization: { state: 'valid', canRefresh: true, message: '访问令牌有效，到期前会在使用时自动刷新' }, ...extra
});
const accounts = [
    account(1, {}),
    account(2, { expiresAt: iso(-HOUR), authorization: { state: 'refreshable', canRefresh: true, message: '访问令牌已过期，下次使用时会尝试用刷新凭据续期' } }),
    account(3, { status: 'reauthorization_required', authorization: { state: 'reauthorization_required', canRefresh: false, message: '刷新凭据已失效，需要重新授权' } }),
    account(4, {
        invocation: { state: 'success', message: '最近一次真实调用成功', provider: 'codex', model: 'gpt-6-luna', observedAt: iso(-60000) },
        authorization: { state: 'refreshable', canRefresh: true, message: 'x', lastRefreshFailedAt: iso(-60000), lastRefreshError: '最近一次自动刷新超时；下次使用时会重试' }
    }),
    account(5, {
        invocation: { state: 'failure', message: '最近一次真实调用被上游拒绝（HTTP 401），请检查账户授权或权限', provider: 'codex', model: 'gpt-6-luna', observedAt: iso(-60000), httpStatus: 401, reason: '401/error' },
        usage: { windows: [{ name: 'primary', usedPercent: 40, remainingPercent: 60, resetAt: iso(-10 * 60000) }], lastAttemptAt: iso(-2 * HOUR), lastSuccessAt: iso(-2 * HOUR), lastCheckedAt: iso(-2 * HOUR) },
        usageState: { state: 'ok', freshness: 'stale', message: '用量数据已超过缓存周期，可能已过时', resetPendingWindows: ['primary'] }
    }),
    // 旧后端：没有 authorization / invocation / usageState 字段。
    { id: 'fake-6', email: 'fake6@example.invalid', plan: 'plus', status: 'ready', expiresAt: iso(10 * 24 * HOUR), usage: freshUsage() }
];
const providerHealth = {
    codex: { name: 'codex', status: 'unverified', message: '访问令牌有效；尚未经网关实际调用验证', checkedAt: iso(0) },
    'codex-ok': { name: 'codex-ok', status: 'ok', message: '最近一次真实调用成功（模型 gpt-6-luna）', checkedAt: iso(0) },
    plain: { name: 'plain', status: 'unchecked', message: '未检测' }
};
const providerConfig = (extra) => ({ baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', maxConcurrent: 2, maxPerSecond: 0, maxQueueWait: 30000, ...extra });
const config = {
    revision: 'fake', failover: {}, breaker: {}, providerHealth: {}, cache: {},
    providers: {
        codex: providerConfig({ authType: 'chatgptOAuth', accountId: 'fake-1' }),
        'codex-ok': providerConfig({ authType: 'chatgptOAuth', accountId: 'fake-4' }),
        plain: providerConfig({ baseUrl: 'https://api.example.invalid', format: 'openai', apiKey: '***' })
    },
    routes: [{ match: 'gpt-*', provider: 'codex', model: 'gpt-6-luna' }]
};

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', e => errors.push(e.message));
        let batchHealth = null;
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            const json = value => route.fulfill({ json: value });
            if (url.pathname === '/api/chatgpt/accounts') return json({ data: accounts });
            if (url.pathname.startsWith('/api/chatgpt/')) return json({ data: accounts });
            if (url.pathname === '/api/config') return json(config);
            if (url.pathname === '/health') return json({ status: 'ok', queues: {}, providerHealth, breakers: null, providerKeys: {} });
            if (url.pathname === '/api/providers/health') {
                const name = url.searchParams.get('provider');
                return json({ providers: name ? { [name]: providerHealth[name] } : batchHealth });
            }
            if (url.pathname.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const filename = url.pathname === '/' ? 'index.html' : url.pathname.replace(/^\//, '');
            const file = path.resolve(root, filename);
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 6);
        const app = fn => page.evaluate(fn);

        const rowOf = email => page.locator('tbody', { has: page.getByText(email, { exact: true }) }).locator('tr').first();
        const expectRow = async (email, statusLabel, invocationLabel) => {
            const row = rowOf(email);
            // 第一列是批量选择框，账户与授权徽章在第二列。
            const tags = await row.locator('td').nth(1).locator('.tag').allTextContents();
            assert(tags.some(text => text.startsWith(statusLabel)), `${email} 授权徽章 ${JSON.stringify(tags)}，期望 ${statusLabel}`);
            assert.equal(await row.getByTestId('chatgpt-invocation-badge').textContent(), invocationLabel, `${email} 调用徽章`);
        };
        await expectRow('fake1@example.invalid', '访问令牌到期：', '尚未实测');
        await expectRow('fake2@example.invalid', '可自动刷新', '尚未实测');
        await expectRow('fake3@example.invalid', '需重新授权', '尚未实测');
        await expectRow('fake4@example.invalid', '自动刷新失败', '调用成功');
        await expectRow('fake5@example.invalid', '访问令牌到期：', '调用失败');
        await expectRow('fake6@example.invalid', '访问令牌到期：', '尚未实测');
        assert.match(await rowOf('fake5@example.invalid').getByTestId('chatgpt-invocation-badge').getAttribute('class'), /bg-danger\/10/);
        assert.match(await rowOf('fake4@example.invalid').getByTestId('chatgpt-invocation-badge').getAttribute('title'), /codex · gpt-6-luna/);
        console.log('通过：授权与实际调用分两枚徽章，旧后端字段缺失时回退旧语义');
        const staleRow = rowOf('fake5@example.invalid');
        assert(await staleRow.getByTestId('chatgpt-usage-stale').isVisible(), '过期快照应提示数据已过时');
        assert(await staleRow.getByTestId('chatgpt-usage-reset-pending').isVisible(), '重置时间已到应提示待复查');
        assert(await staleRow.getByText('已到重置 待复查').isVisible(), '窗口行不应把已过的重置时间当成普通重置时间');
        assert(!(await rowOf('fake1@example.invalid').getByTestId('chatgpt-usage-stale').isVisible()), '新鲜快照不应提示过时');
        await staleRow.click();
        const detail = page.locator('tbody', { has: page.getByText('fake5@example.invalid', { exact: true }) }).getByTestId('chatgpt-invocation-detail');
        await detail.waitFor({ state: 'visible' });
        assert.match(await detail.textContent(), /被上游拒绝（HTTP 401）.*codex · gpt-6-luna/s);
        await rowOf('fake4@example.invalid').click();
        const authDetail = page.locator('tbody', { has: page.getByText('fake4@example.invalid', { exact: true }) }).getByTestId('chatgpt-authorization-detail');
        await authDetail.waitFor({ state: 'visible' });
        assert.match(await authDetail.textContent(), /自动刷新失败.*最近一次自动刷新超时/s);
        console.log('通过：过时快照与重置待复查提示，展开详情显示调用和刷新失败原因');

        await app(() => window.Alpine.$data(document.body).selectNav('monitor'));
        const monitorRow = name => page.locator('tr', { has: page.locator('td', { hasText: new RegExp(`^${name}$`) }) }).first();
        await monitorRow('codex').waitFor({ state: 'visible' });
        assert.equal((await monitorRow('codex').locator('.tag-dot').first().textContent()).trim(), '未实测');
        assert.equal((await monitorRow('codex-ok').locator('.tag-dot').first().textContent()).trim(), '健康');
        assert.equal((await monitorRow('plain').locator('.tag-dot').first().textContent()).trim(), '未检测');
        const counts = await app(() => window.Alpine.$data(document.body).providerStateCounts());
        assert.deepEqual({ ok: counts.ok, error: counts.error, unchecked: counts.unchecked }, { ok: 1, error: 0, unchecked: 2 });
        const routeStatus = await app(() => window.Alpine.$data(document.body).routeTargetStatus('codex'));
        assert.match(routeStatus.healthTitle, /凭据可用，尚未实测/);
        console.log('通过：OAuth Provider 未实测不计为健康，路由提示同步');

        const toastTexts = () => page.locator('[role=status]').allTextContents();
        await app(() => window.Alpine.$data(document.body).checkOneProvider('codex'));
        assert((await toastTexts()).some(text => text.includes('「codex」检测完成')));
        assert.equal(await app(() => window.Alpine.$data(document.body).statusToasts.at(-1).type), 'success');
        batchHealth = { codex: providerHealth.codex, 'codex-ok': providerHealth['codex-ok'], plain: { name: 'plain', status: 'ok', message: 'ok' } };
        await app(() => window.Alpine.$data(document.body).checkProviderHealth());
        const last = await app(() => window.Alpine.$data(document.body).statusToasts.at(-1));
        assert.equal(last.type, 'success');
        assert.match(last.message, /2\/3 可用，1 个凭据可用但尚未实测/);
        batchHealth = { ...batchHealth, plain: { name: 'plain', status: 'error', message: 'down' } };
        await app(() => window.Alpine.$data(document.body).checkProviderHealth());
        assert.equal(await app(() => window.Alpine.$data(document.body).statusToasts.at(-1).type), 'error');
        console.log('通过：未实测不按失败提示，真实异常仍按失败提示');

        await app(() => window.Alpine.$data(document.body).selectNav('providers'));
        // 切页时主区域在滚动位置恢复前会暂时 visibility: hidden，等恢复完成再断言可见性。
        await page.waitForFunction(() => !window.Alpine.$data(document.body)._navScrollRestoreTab);
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            const badge = rowOf('fake5@example.invalid').getByTestId('chatgpt-invocation-badge');
            await badge.scrollIntoViewIfNeeded();
            assert(await badge.isVisible(), `${width}px 下调用徽章应可见`);
            await page.screenshot({ path: `/tmp/chatgpt-account-status-${width}.png` });
        }
        assert.deepEqual(errors, []);
        console.log('通过：桌面与窄屏截图已生成；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
