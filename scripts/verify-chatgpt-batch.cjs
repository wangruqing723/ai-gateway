#!/usr/bin/env node
'use strict';

// 验证 ChatGPT 账户筛选排序、选择范围、暂停恢复和批量操作的前端交互。
// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-batch.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19994';
const iso = offsetMs => new Date(Date.now() + offsetMs).toISOString().replace(/\.\d{3}Z$/, 'Z');
const HOUR = 3600 * 1000;

const fresh = { state: 'ok', freshness: 'fresh', message: 'ok' };
const usage = (primary, secondary) => ({
    windows: [
        ...(primary === null ? [] : [{ name: 'primary', remainingPercent: primary, usedPercent: 100 - primary, resetAt: iso(2 * HOUR) }]),
        ...(secondary === undefined ? [] : [{ name: 'secondary', remainingPercent: secondary, usedPercent: 100 - secondary, resetAt: iso(48 * HOUR) }])
    ],
    lastAttemptAt: iso(-60000), lastSuccessAt: iso(-60000), lastCheckedAt: iso(-60000)
});
const account = (n, extra) => ({
    id: `fake-${n}`, email: `fake${n}@example.invalid`, plan: 'plus', status: 'ready', enabled: true, expiresAt: iso(10 * 24 * HOUR),
    tags: [], providers: [], usage: usage(50), usageState: fresh, invocation: { state: 'unknown', message: '尚未实测' },
    authorization: { state: 'valid', canRefresh: true, message: '访问令牌有效' }, ...extra
});
const initialAccounts = () => [
    account(1, { tags: ['工作'], providers: ['codex-a'], usage: usage(80), invocation: { state: 'success', message: 'ok', observedAt: iso(-60000) } }),
    account(2, { tags: ['工作', '备用'], plan: 'pro', usage: usage(20) }),
    account(3, { tags: ['备用'], usage: usage(55), usageState: { ...fresh, freshness: 'stale' } }),
    account(4, { enabled: false, pausedAt: iso(-HOUR), plan: 'pro', usage: usage(null), providers: ['codex-b'] }),
    account(5, { usage: usage(20), invocation: { state: 'failure', message: '失败', httpStatus: 429 } }),
    account(6, { tags: ['实验'], status: 'reauthorization_required', authorization: { state: 'reauthorization_required', canRefresh: false, message: '需要重新授权' } })
];
const providerConfig = extra => ({ baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', authType: 'chatgptOAuth', maxConcurrent: 2, maxPerSecond: 0, maxQueueWait: 30000, ...extra });
const config = {
    revision: 'fake', failover: {}, breaker: {}, providerHealth: {}, cache: {},
    providers: {
        'codex-a': providerConfig({ accountId: 'fake-1' }),
        'codex-b': providerConfig({ accountId: 'fake-4' }),
        'codex-off': providerConfig({ accountId: 'fake-1', enabled: false })
    },
    routes: [{ match: 'gpt-*', provider: 'codex-a', model: 'gpt-6-luna' }]
};
config.providers['codex-a'].enabled = true;

// 假后端：只实现前端会调用的契约，版本号随写入推进，便于检查 If-Match。
function createBackend() {
    const state = { accounts: initialAccounts(), seq: 1, requests: [], tokens: new Map(), conflictNextState: false, expireNextApply: false, usageDelayMs: 300 };
    state.accounts[0].providers = ['codex-a', 'codex-off'];
    const etag = () => `"epoch.${state.seq}"`;
    const find = id => state.accounts.find(item => item.id === id);
    const planFor = input => {
        const missing = input.accountIds.filter(id => !find(id));
        if (missing.length) return { status: 404, body: { error: { type: 'account_not_found', message: '部分账户不存在', accountIds: missing } } };
        const entries = input.accountIds.map(id => {
            const item = find(id);
            const before = { exists: true, enabled: item.enabled !== false, tags: [...item.tags] };
            const after = { ...before, tags: [...before.tags] };
            const errors = [];
            if (input.operation === 'pause') after.enabled = false;
            if (input.operation === 'resume') after.enabled = true;
            if (input.operation === 'tags') {
                for (const tag of input.addTags || []) if (!after.tags.includes(tag)) after.tags.push(tag);
                after.tags = after.tags.filter(tag => !(input.removeTags || []).includes(tag));
            }
            if (input.operation === 'delete') {
                after.exists = false;
                if (item.providers.length) errors.push('账户被 Provider 引用：' + item.providers.join(', '));
            }
            const changed = JSON.stringify(before) !== JSON.stringify(after);
            return { accountId: id, email: item.email, action: errors.length ? 'blocked' : changed ? 'change' : 'unchanged', before, after, providers: item.providers, errors };
        });
        const count = action => entries.filter(entry => entry.action === action).length;
        const summary = { total: entries.length, change: count('change'), unchanged: count('unchanged'), blocked: count('blocked') };
        const canApply = summary.blocked === 0 && summary.change > 0;
        return { status: 200, plan: { operation: input.operation, baseVersion: etag().slice(1, -1), canApply, noChanges: summary.change === 0, summary, entries, blockers: summary.blocked ? ['存在被绑定的账户，整批不可删除'] : undefined } };
    };
    const handle = (req, url) => {
        const body = req.postData() ? JSON.parse(req.postData()) : null;
        state.requests.push({ method: req.method(), path: url.pathname, body, ifMatch: req.headers()['if-match'] || '' });
        if (url.pathname === '/api/chatgpt/accounts' && req.method() === 'GET') return { status: 200, headers: { ETag: etag() }, body: { data: state.accounts } };
        const stateMatch = url.pathname.match(/^\/api\/chatgpt\/accounts\/([^/]+)\/state$/);
        if (stateMatch) {
            if (state.conflictNextState) { state.conflictNextState = false; state.seq++; return { status: 409, body: { error: { type: 'account_conflict', message: '账户已被其他操作修改，请刷新后重试' } } }; }
            if (req.headers()['if-match'] !== etag()) return { status: 409, body: { error: { type: 'account_conflict', message: '版本不一致' } } };
            const item = find(decodeURIComponent(stateMatch[1]));
            const paused = body.state === 'paused';
            const changed = (item.enabled === false) !== paused;
            if (changed) { item.enabled = !paused; item.pausedAt = paused ? iso(0) : undefined; state.seq++; }
            return { status: 200, headers: { ETag: etag() }, body: { data: { account: item, changed, providers: [] } } };
        }
        if (url.pathname === '/api/chatgpt/accounts/batch/preview') {
            const result = planFor(body);
            if (result.status !== 200) return result;
            const token = 'token-' + state.tokens.size;
            state.tokens.set(token, { input: JSON.stringify(body), version: etag() });
            return { status: 200, headers: { ETag: etag() }, body: { data: { ...result.plan, previewToken: result.plan.canApply ? token : null, expiresAt: result.plan.canApply ? iso(600000) : null } } };
        }
        if (url.pathname === '/api/chatgpt/accounts/batch/apply') {
            if (state.expireNextApply) { state.expireNextApply = false; return { status: 410, body: { error: { type: 'preview_expired', message: '预检已过期，请重新预检' } } }; }
            const { previewToken, ...input } = body;
            const saved = state.tokens.get(previewToken);
            if (!saved || saved.input !== JSON.stringify(input)) return { status: 409, body: { error: { type: 'preview_mismatch', message: '预检不匹配' } } };
            if (req.headers()['if-match'] !== saved.version || saved.version !== etag()) return { status: 409, body: { error: { type: 'account_conflict', message: '版本不一致' } } };
            const { plan } = planFor(input);
            for (const entry of plan.entries.filter(item => item.action === 'change')) {
                const item = find(entry.accountId);
                if (input.operation === 'delete') state.accounts = state.accounts.filter(acc => acc.id !== item.id);
                else { item.enabled = entry.after.enabled; item.tags = entry.after.tags; }
            }
            state.seq++;
            return { status: 200, headers: { ETag: etag() }, body: { data: { operation: input.operation, summary: plan.summary, entries: plan.entries, accounts: state.accounts.filter(acc => input.accountIds.includes(acc.id)) } } };
        }
        if (url.pathname === '/api/chatgpt/accounts/usage/batch-refresh') {
            const statuses = ['success', 'cached', 'failure', 'cancelled'];
            const results = body.accountIds.map((id, index) => ({ accountId: id, status: statuses[index % statuses.length], category: index % 4 === 2 ? 'network' : index % 4 === 3 ? 'cancelled' : undefined, message: `结果 ${index}`, durationMs: 120 }));
            return new Promise(resolve => setTimeout(() => resolve({ status: 200, body: { data: { results, accounts: state.accounts.filter(acc => body.accountIds.includes(acc.id)), durationMs: 300, budgetMs: 60000 } } }), state.usageDelayMs));
        }
        if (url.pathname.startsWith('/api/chatgpt/')) return { status: 200, body: { data: state.accounts } };
        if (url.pathname === '/api/config') return { status: 200, body: config };
        if (url.pathname === '/health') return { status: 200, body: { status: 'ok', queues: {}, providerHealth: {}, breakers: null, providerKeys: {} } };
        if (url.pathname.startsWith('/api/')) return { status: 200, body: { data: [], summary: {}, providers: {} } };
        return null;
    };
    return { state, handle };
}

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', error => errors.push(error.message));
        const backend = createBackend();
        await page.route('**/*', async route => {
            const url = new URL(route.request().url());
            if (url.origin !== origin) return route.abort();
            const result = await backend.handle(route.request(), url);
            if (result) return route.fulfill({ status: result.status, headers: result.headers, json: result.body });
            const filename = path.resolve(root, url.pathname === '/' ? 'index.html' : url.pathname.slice(1));
            if (!filename.startsWith(root + path.sep) || !fs.existsSync(filename)) return route.fulfill({ status: 404 });
            const contentType = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(filename)];
            return route.fulfill({ contentType, body: fs.readFileSync(filename) });
        });
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 6);
        const evaluate = fn => page.evaluate(fn);
        const ids = () => evaluate(() => Alpine.$data(document.body).filteredChatGPTAccounts().map(a => a.id));
        await page.getByTestId('chatgpt-filter-toggle').click();
        await page.getByTestId('chatgpt-filter-enabled').selectOption('paused');
        assert.deepEqual(await ids(), ['fake-4']);
        await page.getByTestId('chatgpt-filter-enabled').selectOption('');
        await page.getByTestId('chatgpt-sort').selectOption('primaryRemaining');
        assert.deepEqual(await ids(), ['fake-2', 'fake-5', 'fake-6', 'fake-1', 'fake-3', 'fake-4']);
        await page.getByTestId('chatgpt-sort-direction').click();
        assert.deepEqual(await ids(), ['fake-1', 'fake-6', 'fake-2', 'fake-5', 'fake-3', 'fake-4']);
        await evaluate(() => {
            const a = Alpine.$data(document.body);
            a.toggleChatGPTAccountTagFilter('工作'); a.toggleChatGPTAccountTagFilter('备用');
            a.chatGPTAccountFilters.plan = 'pro';
        });
        assert.deepEqual(await ids(), ['fake-2']);
        await page.getByTestId('chatgpt-select-visible').check();
        await evaluate(() => { const a = Alpine.$data(document.body); a.chatGPTAccountFilters.plan = 'plus'; });
        assert.equal(await evaluate(() => Alpine.$data(document.body).chatGPTSelectionSummary().hidden), 1);
        await page.getByTestId('chatgpt-batch-pause').click();
        assert.match(await page.getByTestId('chatgpt-batch-hidden').textContent(), /1/);
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'visible' });
        await page.getByTestId('chatgpt-batch-apply').click();
        await page.getByTestId('chatgpt-batch-result').waitFor({ state: 'visible' });
        assert.equal(backend.state.accounts.find(a => a.id === 'fake-2').enabled, false);
        await page.getByRole('button', { name: '关闭批量操作', exact: true }).click();
        await evaluate(() => { const a = Alpine.$data(document.body); a.resetChatGPTAccountFilters(); a.chatGPTSelectedAccountIds = ['fake-1', 'fake-3']; });
        await page.getByTestId('chatgpt-batch-delete').click();
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-plan').waitFor({ state: 'visible' });
        assert.equal(await page.getByTestId('chatgpt-batch-apply').isVisible(), false);
        await page.getByRole('button', { name: '关闭批量操作', exact: true }).click();
        await evaluate(() => { Alpine.$data(document.body).chatGPTSelectedAccountIds = ['fake-1', 'fake-2', 'fake-3', 'fake-4']; });
        await page.getByTestId('chatgpt-batch-usage').click();
        await page.getByTestId('chatgpt-batch-usage-start').click();
        await page.getByTestId('chatgpt-batch-result').waitFor({ state: 'visible' });
        assert.equal(await page.getByTestId('chatgpt-batch-result-item').count(), 4);
        assert.equal(backend.state.requests.filter(r => r.path.endsWith('/usage/batch-refresh')).length, 1);
        await page.getByRole('button', { name: '关闭批量操作', exact: true }).click();
        await page.setViewportSize({ width: 1440, height: 1000 });
        // 单账户暂停：确认文案列出受影响和已停用的 Provider；写入携带列表 ETag。
        const row = email => page.locator('tbody', { has: page.getByText(email, { exact: true }) }).locator('tr').first();
        await row('fake1@example.invalid').getByTestId('chatgpt-account-state').click();
        const confirmText = await page.getByRole('alertdialog').textContent();
        assert.match(confirmText, /codex-a/);
        assert.match(confirmText, /codex-off.*保持停用/);
        const before = backend.state.seq;
        await page.getByRole('alertdialog').getByRole('button', { name: '确认暂停' }).click();
        await page.waitForFunction(() => Alpine.$data(document.body).chatgptAccounts.find(a => a.id === 'fake-1')?.enabled === false);
        const stateRequest = backend.state.requests.filter(r => r.path.endsWith('/fake-1/state')).at(-1);
        assert.equal(stateRequest.ifMatch, `"epoch.${before}"`);
        assert.deepEqual(stateRequest.body, { state: 'paused' });
        assert(await row('fake1@example.invalid').getByTestId('chatgpt-paused-badge').isVisible());
        assert.match(await row('fake1@example.invalid').getByTestId('chatgpt-provider-effective').first().textContent(), /账户暂停|已停用/);
        await page.waitForFunction(() => [...document.querySelectorAll('[data-testid=provider-oauth-badge]')].some(el => el.textContent.includes('账户暂停')));
        // 版本冲突：返回 409 后重新读取列表，状态不被页面本地改写。
        backend.state.conflictNextState = true;
        const reloads = backend.state.requests.filter(r => r.path === '/api/chatgpt/accounts').length;
        await row('fake1@example.invalid').getByTestId('chatgpt-account-state').click();
        await page.getByRole('alertdialog').getByRole('button', { name: '确认恢复' }).click();
        await page.waitForFunction(() => !Alpine.$data(document.body).changingChatGPTAccountState);
        assert(backend.state.requests.filter(r => r.path === '/api/chatgpt/accounts').length > reloads, '409 后应重新读取账户列表');
        assert.equal(backend.state.accounts.find(a => a.id === 'fake-1').enabled, false);
        // Esc 关闭批量浮层并把焦点还给打开它的按钮。
        await evaluate(() => { Alpine.$data(document.body).chatGPTSelectedAccountIds = ['fake-5']; });
        await page.getByTestId('chatgpt-batch-tags').click();
        assert.equal(await page.getByTestId('chatgpt-batch-preview').isDisabled(), true, '未填写标签时不能预检');
        await page.keyboard.press('Escape');
        await page.getByTestId('chatgpt-batch-dialog').waitFor({ state: 'hidden' });
        assert.equal(await evaluate(() => document.activeElement?.dataset?.testid), 'chatgpt-batch-tags');
        console.log('通过：单账户暂停确认列出 Provider、ETag 写入、冲突重读、Provider 暂停标记与 Esc 焦点恢复');
        // 改标签作废已有预检；预检过期（410）后清空计划、焦点交给重新预检，账户不被改写。
        const applyCount = () => backend.state.requests.filter(r => r.path.endsWith('/batch/apply')).length;
        await page.getByTestId('chatgpt-batch-tags').click();
        await page.getByTestId('chatgpt-batch-add-tags').fill('新标签');
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'visible' });
        await page.getByTestId('chatgpt-batch-add-tags').fill('新标签, 第二个');
        assert.equal(await evaluate(() => Alpine.$data(document.body).chatGPTBatchPlan), null, '改输入后预检应作废');
        // Alpine 的 x-show 隐藏在下一帧生效，等待隐藏而不是立即读可见性。
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'hidden' });
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'visible' });
        backend.state.expireNextApply = true;
        await page.getByTestId('chatgpt-batch-apply').click();
        await page.getByTestId('chatgpt-batch-error').waitFor({ state: 'visible' });
        assert.match(await page.getByTestId('chatgpt-batch-error').textContent(), /预检已过期.*请重新预检/);
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'hidden' });
        await page.waitForFunction(() => document.activeElement?.dataset?.testid === 'chatgpt-batch-preview');
        assert.deepEqual(backend.state.accounts.find(a => a.id === 'fake-5').tags, []);
        // 重复提交只发出一次写入。
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'visible' });
        const appliesBefore = applyCount();
        await evaluate(() => { const a = Alpine.$data(document.body); a.runChatGPTBatch('apply'); a.runChatGPTBatch('apply'); });
        await page.getByTestId('chatgpt-batch-result').waitFor({ state: 'visible' });
        assert.equal(applyCount() - appliesBefore, 1, '连续两次提交只应发出一次写入');
        assert.deepEqual(backend.state.accounts.find(a => a.id === 'fake-5').tags, ['新标签', '第二个']);
        await page.getByRole('button', { name: '关闭批量操作', exact: true }).click();
        // 预检后账户被别处删除：提交冲突，重读列表，把不存在的账户移出批次并说明数量。
        await evaluate(() => { Alpine.$data(document.body).chatGPTSelectedAccountIds = ['fake-5', 'fake-6']; });
        await page.getByTestId('chatgpt-batch-tags').click();
        await page.getByTestId('chatgpt-batch-add-tags').fill('迁移');
        await page.getByTestId('chatgpt-batch-preview').click();
        await page.getByTestId('chatgpt-batch-apply').waitFor({ state: 'visible' });
        backend.state.accounts = backend.state.accounts.filter(a => a.id !== 'fake-6');
        backend.state.seq++;
        await page.getByTestId('chatgpt-batch-apply').click();
        await page.getByTestId('chatgpt-batch-error').waitFor({ state: 'visible' });
        assert.match(await page.getByTestId('chatgpt-batch-error').textContent(), /已从本次批次移除 1 个不存在的账户/);
        assert.deepEqual(await evaluate(() => Alpine.$data(document.body).chatGPTBatchAccountIds), ['fake-5']);
        assert.deepEqual(await evaluate(() => Alpine.$data(document.body).chatGPTSelectedAccountIds), ['fake-5']);
        assert.deepEqual(backend.state.accounts.find(a => a.id === 'fake-5').tags, ['新标签', '第二个']);
        await page.getByRole('button', { name: '关闭批量操作', exact: true }).click();
        console.log('通过：改输入作废预检、过期重预检与焦点、重复提交单次写入、删除项移出批次');
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            await page.screenshot({ path: `/tmp/chatgpt-batch-${width}.png` });
        }
        assert.deepEqual(errors, []);
        console.log('通过：筛选组合、未知置后、隐藏选择、批量暂停、引用阻断删除、逐项用量结果与桌面窄屏截图');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });

