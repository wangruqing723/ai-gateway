#!/usr/bin/env node
'use strict';

// 只读取正式前端产物，全部请求在本机拦截；不读取真实配置或账户凭据。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright node scripts/verify-chatgpt-account-transfer.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19999';
const accounts = [1, 2].map(n => ({ id: `fake-${n}`, email: `fake${n}@example.invalid`, plan: 'plus', usage: { lastCheckedAt: new Date().toISOString() } }));
const config = { revision: 'fake', providers: {}, routes: [], failover: {}, breaker: {}, providerHealth: {}, cache: {} };

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [];
        page.on('pageerror', e => errors.push(e.message));
        let imports = [], exports = [], fail = false, release = null, hold = false;
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            const json = value => route.fulfill({ json: value });
            if (url.pathname === '/api/chatgpt/accounts/import') {
                imports.push({ body: req.postData(), type: req.headers()['content-type'] });
                if (hold) await new Promise(resolve => { release = resolve; });
                if (fail) return route.fulfill({ status: 400, json: { error: { message: 'JSON 格式无效' } } });
                return json({ data: { imported: 1, skippedDuplicates: 1, detectedFormat: 'cockpit_tools' } });
            }
            if (url.pathname === '/api/chatgpt/accounts/export') {
                exports.push(req.postDataJSON());
                return route.fulfill({ contentType: 'application/json', headers: { 'Content-Disposition': 'attachment; filename="fake.json"' }, body: '[]' });
            }
            if (url.pathname === '/api/chatgpt/accounts') return json({ data: accounts });
            if (url.pathname === '/api/config') return json(config);
            if (url.pathname === '/health') return json({ status: 'ok', providers: {} });
            if (url.pathname.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const filename = url.pathname === '/' ? 'index.html' : url.pathname.replace(/^\//, '');
            const file = path.resolve(root, filename);
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 2);
        await page.evaluate(() => document.fonts.ready);
        const entryIcon = page.locator('[data-testid="chatgpt-transfer-open"] .material-symbols-outlined');
        assert.equal(await entryIcon.textContent(), 'sync');
        assert(await entryIcon.evaluate(el => el.getBoundingClientRect().width <= 24), '入口图标不应退化为英文名称');
        await page.locator('[data-testid="chatgpt-transfer-open"]').screenshot({ path: '/tmp/chatgpt-transfer-entry.png' });
        console.log('通过：入口图标使用本地字体并正常渲染');
        const app = fn => page.evaluate(fn);
        const dialog = page.locator('[data-testid="chatgpt-transfer-dialog"]');
        const text = page.locator('[data-testid="chatgpt-import-text"]');
        const file = dialog.locator('input[type=file]');
        const submit = dialog.getByRole('button', { name: '导入账户', exact: true });
        const open = async () => { await page.locator('[data-testid="chatgpt-transfer-open"]').click(); await dialog.waitFor({ state: 'visible' }); };
        const importTab = async () => dialog.getByRole('button', { name: '导入', exact: true }).click();
        await open();
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTTransferTab), 'export');
        await importTab();
        await text.fill('   ');
        assert(await submit.isDisabled());
        const raw = '[{"access_token":"FAKE-ONLY","account_id":"fake"}]';
        await text.fill(raw);
        hold = true;
        await submit.click();
        await page.waitForFunction(() => window.Alpine.$data(document.body).importingChatGPTAccounts);
        await page.keyboard.press('Escape');
        assert(await dialog.isVisible());
        assert(await dialog.getByRole('button', { name: '导出', exact: true }).isDisabled());
        assert(await text.isDisabled());
        while (!release) await new Promise(resolve => setTimeout(resolve, 10));
        release(); hold = false;
        await page.waitForFunction(() => !window.Alpine.$data(document.body).importingChatGPTAccounts);
        assert.deepEqual(imports[0], { body: raw, type: 'application/json' });
        assert.equal(await text.inputValue(), '');
        assert(await dialog.getByText('新增 1 个，重复跳过 1 个').isVisible());
        console.log('通过：文本原文提交、空白拦截、忙碌保护、成功统计与清理');

        await text.fill(raw);
        await file.setInputFiles({ name: 'fake.json', mimeType: 'application/json', buffer: Buffer.from(raw) });
        assert.equal(await text.inputValue(), '');
        await submit.click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).importingChatGPTAccounts);
        assert.deepEqual(imports[1], { body: raw, type: 'application/json' });
        await file.setInputFiles({ name: 'fake.zip', mimeType: 'application/zip', buffer: Buffer.from('FAKE-ZIP') });
        await submit.click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).importingChatGPTAccounts);
        assert.equal(imports[2].type, 'application/zip');
        await file.setInputFiles({ name: 'fake.json', mimeType: 'application/json', buffer: Buffer.from(raw) });
        await text.fill(raw);
        assert.equal(await file.evaluate(el => el.files.length), 0);
        fail = true;
        await submit.click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).importingChatGPTAccounts);
        assert.equal(await text.inputValue(), '');
        assert(await dialog.getByRole('alert').isVisible());
        fail = false;
        await app(() => { const a = window.Alpine.$data(document.body); a.chatGPTImportText = '中'.repeat(6 * 1024 * 1024); return a.importChatGPTAccounts(); });
        assert.equal(imports.length, 4);
        assert((await dialog.getByRole('alert').textContent()).includes('16 MiB'));
        await text.fill(raw);
        await dialog.getByRole('button', { name: '导出', exact: true }).click();
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportText), '');
        console.log('通过：JSON/ZIP、输入互斥、错误清理、UTF-8 超限与切页清理');

        for (const format of ['cliproxyapi', 'cockpit_tools', 'sub2api', 'auth_json']) {
            if (!(await dialog.isVisible())) await open();
            await dialog.locator('select').selectOption(format);
            await dialog.getByRole('button', { name: '清空', exact: true }).click();
            assert(await dialog.getByRole('button', { name: '导出所选账户' }).isDisabled());
            await dialog.getByRole('button', { name: '全选', exact: true }).click();
            await dialog.getByRole('button', { name: '导出所选账户' }).click();
            await dialog.waitFor({ state: 'hidden' });
            assert.deepEqual(exports.at(-1), { format, accountIds: ['fake-1', 'fake-2'] });
        }
        await app(() => window.Alpine.$data(document.body).exportChatGPTAccount({ id: 'fake-2' }));
        assert.deepEqual(exports.at(-1).accountIds, ['fake-2']);
        await open(); await importTab(); await text.fill(raw);
        assert(await app(() => { let prevented = false; window.Alpine.$data(document.body).onSaveShortcut({ ctrlKey: true, key: 's', preventDefault() { prevented = true; } }); return prevented; }));
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportText), '');
        console.log('通过：四种格式、多选、行内快捷导出、Esc 与保存快捷键');

        await app(() => { window.Alpine.$data(document.body).chatgptAccounts = []; });
        await open();
        assert(await dialog.getByText('暂无可导出的账户，请切换到导入或先连接账户。').isVisible());
        await importTab();
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 850 });
            await text.fill(raw);
            assert(await dialog.locator('[role=dialog]').evaluate(el => el.scrollWidth <= el.clientWidth));
            await page.screenshot({ path: `/tmp/chatgpt-transfer-${width}.png` });
        }
        await dialog.getByRole('button', { name: '关闭账户导入导出弹窗' }).click();
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTImportText), '');
        assert.deepEqual(errors, []);
        console.log('通过：无账户导入、桌面/窄屏布局、关闭清理；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
