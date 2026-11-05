#!/usr/bin/env node
'use strict';

// 验证 ChatGPT 账户导入预检流程：预检、逐项改选自动重新预检、应用携带凭证与 If-Match、失败与关闭清理。
// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-import-preview.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19996';
const accounts = [{ id: 'acc-1', email: 'one@example.invalid', plan: 'plus', status: 'ready', providers: ['codex'] }];
const raw = '[{"access_token":"FAKE-ONLY-1","account_id":"remote-1"},{"access_token":"FAKE-ONLY-2","account_id":"remote-2"}]';

// planFor 按请求中的逐项动作模拟后端计划：第 1 条是已有账户，第 2 条是新账户。
function planFor(actions) {
    const chosen = Object.fromEntries((actions || []).map(a => [a.index, a]));
    const first = chosen[1]?.action || 'skip';
    const second = chosen[2]?.action || 'add';
    const update = first === 'update' ? 1 : 0, add = second === 'add' ? 1 : 0;
    return {
        detectedFormat: 'cockpit_tools_or_cliproxyapi', baseVersion: 'v1', canApply: update + add > 0, noChanges: update + add === 0,
        summary: { total: 2, add, update, skip: 2 - add - update, invalid: 0, blocked: 0 },
        entries: [
            { index: 1, email: 'one@example.invalid', status: 'existing', matchedAccountId: 'acc-1', affectedProviders: ['codex'], suggestedAction: 'skip', action: first, allowedActions: ['skip', 'update'], hasRefreshToken: true },
            { index: 2, email: 'two@example.invalid', status: 'new', suggestedAction: 'add', action: second, allowedActions: ['add', 'skip'], hasRefreshToken: true }
        ],
        previewToken: update + add > 0 ? 'fake-token-' + update + add : null, expiresAt: update + add > 0 ? '2099-01-01T00:00:00Z' : null
    };
}

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', e => errors.push(e.message));
        // Alpine 表达式错误只打到控制台；一并收集，失败时便于定位是哪条表达式。
        page.on('console', m => { if (/Alpine Expression Error/.test(m.text())) errors.push(m.text()); });
        const previews = [], applies = [];
        let applyFails = false;
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            const json = (value, headers) => route.fulfill({ json: value, headers });
            if (url.pathname === '/api/chatgpt/accounts/import/preview') {
                const body = req.postDataJSON();
                previews.push(body);
                return json({ data: planFor(body.actions) }, { ETag: '"v1"' });
            }
            if (url.pathname === '/api/chatgpt/accounts/import/apply') {
                applies.push({ body: req.postDataJSON(), ifMatch: req.headers()['if-match'] });
                if (applyFails) return route.fulfill({ status: 409, json: { error: { type: 'account_conflict', message: '账户或目标凭据在预检后已变化，请重新预检' } } });
                return json({ data: { detectedFormat: 'cockpit_tools_or_cliproxyapi', imported: 1, updated: 1, skippedDuplicates: 0, accounts: [] } }, { ETag: '"v2"' });
            }
            if (url.pathname === '/api/chatgpt/accounts' || url.pathname === '/api/chatgpt/accounts/usage/refresh') return json({ data: accounts }, { ETag: '"v1"' });
            if (url.pathname === '/api/config') return json({ revision: 'fake', providers: {}, routes: [], failover: {}, breaker: {}, providerHealth: {}, cache: {} });
            if (url.pathname === '/health') return json({ status: 'ok', queues: {}, providerHealth: {}, breakers: null });
            if (url.pathname.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const filename = url.pathname === '/' ? 'index.html' : url.pathname.replace(/^\//, '');
            const file = path.resolve(root, filename);
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 1);
        const app = fn => page.evaluate(fn);
        const dialog = page.locator('[data-testid="chatgpt-transfer-dialog"]');
        const text = page.locator('[data-testid="chatgpt-import-text"]');
        const previewButton = page.getByTestId('chatgpt-import-preview');
        const applyButton = page.getByTestId('chatgpt-import-apply');
        const idle = () => page.waitForFunction(() => !window.Alpine.$data(document.body).importingChatGPTAccounts);
        const open = async () => {
            await page.locator('[data-testid="chatgpt-transfer-open"]').click();
            await dialog.waitFor({ state: 'visible' });
            await dialog.getByRole('button', { name: '导入', exact: true }).click();
        };

        await open();
        await text.fill(raw);
        await previewButton.click();
        await idle();
        assert.deepEqual(previews[0], { source: { mediaType: 'application/json', encoding: 'utf8', content: raw }, actions: [] });
        assert.equal(await page.getByTestId('chatgpt-import-plan-entry').count(), 2);
        assert.match(await page.getByTestId('chatgpt-import-plan').textContent(), /绑定 Provider：codex/);
        assert(!(await page.getByRole('button', { name: '导入账户', exact: true }).isVisible()), '有计划时不应再显示单步导入');
        assert.match(await applyButton.textContent(), /新增 1，更新 0/);
        console.log('通过：预检原文提交、逐条显示匹配与绑定影响、单步导入按钮隐藏');

        await page.getByTestId('chatgpt-import-action-1').selectOption('update');
        await idle();
        assert.deepEqual(previews[1].actions, [{ index: 1, action: 'update', targetAccountId: 'acc-1' }]);
        assert.match(await applyButton.textContent(), /新增 1，更新 1/);
        await page.getByTestId('chatgpt-import-action-2').selectOption('skip');
        await idle();
        assert.deepEqual(previews[2].actions, [{ index: 1, action: 'update', targetAccountId: 'acc-1' }, { index: 2, action: 'skip' }]);
        await page.getByTestId('chatgpt-import-action-1').selectOption('skip');
        await idle();
        assert(await applyButton.isDisabled(), '没有变更时不能应用');
        await page.getByTestId('chatgpt-import-action-1').selectOption('update');
        await idle();
        console.log('通过：改选后自动重新预检，动作按序号规范化，无变更时禁止应用');

        await applyButton.click();
        await idle();
        assert.equal(applies[0].ifMatch, '"v1"');
        assert.equal(applies[0].body.previewToken, 'fake-token-10');
        assert.deepEqual(applies[0].body.source.content, raw);
        assert.equal(await text.inputValue(), '');
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportPlan), null);
        assert(await dialog.getByText('新增 1 个，更新 1 个，重复跳过 0 个').isVisible());
        console.log('通过：应用携带预检凭证与 If-Match，成功后清空源和计划');

        await text.fill(raw);
        await previewButton.click();
        await idle();
        await page.getByTestId('chatgpt-import-action-1').selectOption('update');
        await idle();
        applyFails = true;
        await applyButton.click();
        await idle();
        assert.match(await dialog.getByRole('alert').textContent(), /请重新预检/);
        assert.equal(await text.inputValue(), '');
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportSource), null);
        console.log('通过：应用冲突时提示重新预检并清空源');

        await text.fill(raw);
        await previewButton.click();
        await idle();
        await text.fill(raw + ' ');
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportPlan), null, '改动输入应作废计划');
        await previewButton.click();
        await idle();
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportSource), null, '关闭弹窗应清空源');
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            await open();
            await text.fill(raw);
            await previewButton.click();
            await idle();
            assert(await dialog.locator('[role=dialog]').evaluate(el => el.scrollWidth <= el.clientWidth), `${width}px 下弹窗不应横向溢出`);
            await page.screenshot({ path: `/tmp/chatgpt-import-preview-${width}.png` });
            await page.keyboard.press('Escape');
            await dialog.waitFor({ state: 'hidden' });
        }
        assert.deepEqual(errors, []);
        console.log('通过：改输入与关闭作废计划，桌面与窄屏无横向溢出；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
