#!/usr/bin/env node
'use strict';

// 验证 ChatGPT 账户加密备份与备份恢复界面：页签说明、导出密码规则与 If-Match、下载后清空密码、
// 恢复预检请求体、计划渲染与动作切换、错误文案、应用凭证与结果映射、交给配置迁移后的映射预填，
// 以及关闭弹窗后的敏感状态清理。
// 只读取正式前端产物 cmd/gateway/web/index.html，全部请求由 page.route 在本机假 origin 上拦截；
// 账户、密码和备份容器都是假值，不读取真实配置或凭据，不访问网络。
// 用法：PLAYWRIGHT_MODULE=/path/to/playwright [PLAYWRIGHT_EXECUTABLE_PATH=...] node scripts/verify-chatgpt-backup.cjs
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');

const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19995';
const shotDir = path.join(os.homedir(), '.cache', 'aigw-verify');
const EXPORT_PASSWORD = 'FAKE-export-password-0001';
const WRONG_PASSWORD = 'FAKE-wrong-password';
const RESTORE_PASSWORD = 'FAKE-restore-password-0002';
const CONTAINER = JSON.stringify({ format: 'ai-gateway-chatgpt-backup', version: 1, kdf: { name: 'pbkdf2-hmac-sha256', iterations: 600000, salt: 'RkFLRS1TQUxULTE2Qnl0ZQ==' }, cipher: { name: 'aes-256-gcm', nonce: 'RkFLRS1OT05DRQ==' }, ciphertext: 'RkFLRS1DSVBIRVJURVhULU9OTFk=' });
const CONFIG_YAML = 'kind: ai-gateway.provider-route-transfer\nversion: 1\nproviders: {}\nroutes: []\n';

// planFor 按请求中的选项与逐项动作模拟后端恢复计划。
// 第 1 条匹配本地 acc-1（默认跳过，可更新）；第 2 条源 ID 被占用需重映射；第 3 条新增并保留源 ID。
function planFor(body) {
    const chosen = Object.fromEntries((body.actions || []).map(a => [a.index, a]));
    const first = chosen[1]?.action || 'skip';
    const second = chosen[2]?.action || 'add';
    const third = chosen[3]?.action || 'add';
    const add = (second === 'add') + (third === 'add'), update = first === 'update' ? 1 : 0;
    const canApply = add + update > 0;
    return {
        baseVersion: 'v1', createdAt: '2026-11-05T08:00:00Z', metadata: body.metadata, restorePaused: body.restorePaused,
        canApply, noChanges: !canApply,
        summary: { total: 3, add, update, skip: 3 - add - update, invalid: 0, blocked: 0 },
        entries: [
            { index: 1, sourceAccountId: 'acc-1', email: 'one@example.invalid', plan: 'plus', hasRefreshToken: true, tags: ['主力'], hasNote: true, paused: true,
                status: 'existing', matchedAccountId: 'acc-1', matchedEmail: 'one@example.invalid', affectedProviders: ['codex-main'],
                suggestedAction: 'skip', action: first, allowedActions: ['skip', 'update'], targetAccountId: first === 'update' ? 'acc-1' : '',
                remapped: false, metadataChanges: first === 'update' && body.metadata === 'replace', pausedChange: first === 'update' && body.restorePaused,
                errors: [], warnings: ['备份凭据的刷新时间早于本地，可能是旧备份'] },
            { index: 2, sourceAccountId: 'acc-9', email: 'nine@example.invalid', plan: 'pro', hasRefreshToken: true, tags: [], hasNote: false, paused: false,
                status: 'new', affectedProviders: [], suggestedAction: 'add', action: second, allowedActions: ['add', 'skip'], targetAccountId: '',
                remapped: true, metadataChanges: false, pausedChange: false, errors: [], warnings: [] },
            { index: 3, sourceAccountId: 'acc-src-3', email: 'three@example.invalid', plan: 'team', hasRefreshToken: true, tags: ['备用'], hasNote: false, paused: false,
                status: 'new', affectedProviders: [], suggestedAction: 'add', action: third, allowedActions: ['add', 'skip'], targetAccountId: 'acc-src-3',
                remapped: false, metadataChanges: false, pausedChange: false, errors: [], warnings: [] }
        ],
        blockers: [],
        previewToken: canApply ? `fake-restore-token-${first}-${second}-${third}-${body.metadata}-${body.restorePaused}` : null,
        expiresAt: canApply ? '2099-01-01T00:00:00Z' : null
    };
}

// configPreviewFor 模拟配置迁移导入预检：源文件引用三个 OAuth 源账户。
function configPreviewFor(body) {
    return {
        baseRevision: 'fake', sourceFormat: 'transfer-v1', ignoredGlobalFields: [], canReplace: true, canApply: true,
        catalog: {
            providers: ['acc-9', 'acc-src-3', 'acc-other'].map((id, i) => ({ name: 'oauth-' + i, selected: true, keyCount: 0, apiKeyConfigured: false, sourceAccountId: id, conflict: false })),
            routes: [],
            sourceAccountIds: ['acc-9', 'acc-src-3', 'acc-other']
        },
        summary: { providers: { add: 3, overwrite: 0, delete: 0, keep: 0, unchanged: 0 }, routes: { add: 0, overwrite: 0, delete: 0, keep: 0, unchanged: 0 } },
        changes: { providers: [], routes: [] }, finalRouteOrder: [], warnings: [], blockers: [],
        previewToken: 'fake-config-token-' + JSON.stringify(body.options?.accountMappings || {}).length,
        expiresAt: '2099-01-01T00:00:00Z'
    };
}
(async () => {
    for (const key of ['http_proxy', 'https_proxy', 'all_proxy', 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY']) delete process.env[key];
    fs.mkdirSync(shotDir, { recursive: true });
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const context = await browser.newContext({ acceptDownloads: true, viewport: { width: 1440, height: 1000 } });
        const page = await context.newPage();
        const errors = [], consoleTexts = [];
        page.on('pageerror', e => errors.push(e.message));
        page.on('console', m => {
            consoleTexts.push(m.text());
            if (/Alpine Expression Error/.test(m.text())) errors.push(m.text());
        });
        const state = {
            accounts: [
                { id: 'acc-1', email: 'one@example.invalid', plan: 'plus', status: 'ready', providers: ['codex-main'] },
                { id: 'acc-2', email: 'two@example.invalid', plan: 'pro', status: 'ready', providers: [] }
            ],
            etag: 'v1', accountGets: 0, exports: [], previews: [], applies: [], configPreviews: [], external: [],
            exportConflict: true, applyExpire: true
        };
        let lastPreviewETag = '';
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.protocol === 'blob:' || url.protocol === 'data:') return route.continue().catch(() => {});
            if (url.origin !== origin) { state.external.push(req.url()); return route.abort(); }
            const json = (value, status = 200, headers = {}) => route.fulfill({ status, json: value, headers: { 'Cache-Control': 'no-store', ...headers } });
            const fail = (status, type, message) => json({ error: { type, message } }, status);
            const p = url.pathname;
            if (p === '/api/chatgpt/accounts/backup/export') {
                state.exports.push({ body: req.postDataJSON(), ifMatch: req.headers()['if-match'] || '' });
                if (state.exportConflict) {
                    state.exportConflict = false;
                    state.etag = 'v1b';
                    return fail(409, 'account_conflict', '账户版本已变化');
                }
                return route.fulfill({ status: 200, body: CONTAINER, headers: { 'Content-Type': 'application/octet-stream', 'Content-Disposition': 'attachment; filename="chatgpt-accounts-20261105-080000.aigw-backup"', 'Cache-Control': 'no-store' } });
            }
            if (p === '/api/chatgpt/accounts/backup/preview') {
                const body = req.postDataJSON();
                state.previews.push(body);
                if (body.password !== RESTORE_PASSWORD) return fail(400, 'backup_decrypt_failed', '密码错误或备份已损坏');
                lastPreviewETag = `"${state.etag}"`;
                return json({ data: planFor(body) }, 200, { ETag: lastPreviewETag });
            }
            if (p === '/api/chatgpt/accounts/backup/apply') {
                const body = req.postDataJSON();
                state.applies.push({ body, ifMatch: req.headers()['if-match'] || '' });
                if (state.applyExpire) { state.applyExpire = false; return fail(410, 'preview_expired', '预检已过期'); }
                state.accounts = [...state.accounts,
                    { id: 'acc-new-9', email: 'nine@example.invalid', plan: 'pro', status: 'ready', providers: [] },
                    { id: 'acc-src-3', email: 'three@example.invalid', plan: 'team', status: 'ready', providers: [] }];
                state.etag = 'v2';
                return json({ data: { imported: 2, updated: 1, skipped: 0, accounts: state.accounts, mappings: [
                    { sourceAccountId: 'acc-1', targetAccountId: 'acc-1', action: 'update', remapped: false },
                    { sourceAccountId: 'acc-9', targetAccountId: 'acc-new-9', action: 'add', remapped: true },
                    { sourceAccountId: 'acc-src-3', targetAccountId: 'acc-src-3', action: 'add', remapped: false }
                ] } }, 200, { ETag: '"v2"' });
            }
            if (p === '/api/chatgpt/accounts' || p === '/api/chatgpt/accounts/usage/refresh') {
                if (p === '/api/chatgpt/accounts') state.accountGets += 1;
                return json({ data: state.accounts }, 200, { ETag: `"${state.etag}"` });
            }
            if (p === '/api/config/import/preview') {
                const body = req.postDataJSON();
                state.configPreviews.push(body);
                return json({ data: configPreviewFor(body) }, 200, { ETag: '"fake"' });
            }
            if (p === '/api/config') return json({ revision: 'fake', providers: {}, routes: [], failover: {}, breaker: {}, providerHealth: {}, cache: {} }, 200, { ETag: '"fake"' });
            if (p === '/health') return json({ status: 'ok', queues: {}, providerHealth: {}, breakers: null });
            if (p.startsWith('/api/')) return json({ data: [], summary: {}, providers: {} });
            const filename = p === '/' ? 'index.html' : p.replace(/^\//, '');
            const file = path.resolve(root, filename);
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });

        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 2);
        const app = (fn, arg) => page.evaluate(fn, arg);
        const data = key => app(k => { const v = window.Alpine.$data(document.body)[k]; return v === undefined ? undefined : JSON.parse(JSON.stringify(v)); }, key);
        const dialog = page.getByTestId('chatgpt-transfer-dialog');
        const tid = id => page.getByTestId(id);
        const idle = () => page.waitForFunction(() => !window.Alpine.$data(document.body).chatGPTBackupBusy);
        const open = async tab => {
            await tid('chatgpt-transfer-open').click();
            await dialog.waitFor({ state: 'visible' });
            if (tab) await tid(tab).click();
        };
        const closeByEscape = async () => {
            await page.keyboard.press('Escape');
            await dialog.waitFor({ state: 'hidden' });
        };
        await open();
        assert.match(await tid('chatgpt-export-note').textContent(), /明文凭据文件/);
        assert.match(await tid('chatgpt-export-note').textContent(), /不含本地标签、备注/);
        await tid('chatgpt-backup-tab').click();
        assert.match(await tid('chatgpt-backup-note').textContent(), /网关原生格式/);
        assert.match(await tid('chatgpt-backup-note').textContent(), /标签、备注、授权时间与暂停状态/);
        assert.match(await tid('chatgpt-backup-note').textContent(), /只能由本网关用密码恢复/);
        const exportButton = tid('chatgpt-backup-export');
        const password = tid('chatgpt-backup-password'), confirm = tid('chatgpt-backup-password-confirm');
        assert.equal(await password.getAttribute('type'), 'password');
        assert.equal(await password.getAttribute('autocomplete'), 'new-password');
        assert.equal(await confirm.getAttribute('autocomplete'), 'new-password');
        assert(await exportButton.isDisabled());
        await password.fill('short');
        await confirm.fill('short');
        assert(await exportButton.isDisabled());
        await password.fill(EXPORT_PASSWORD);
        await confirm.fill(EXPORT_PASSWORD + '-different');
        assert(await exportButton.isDisabled());
        await confirm.fill(EXPORT_PASSWORD);
        assert(await exportButton.isEnabled());
        await tid('chatgpt-backup-select-none').click();
        assert(await exportButton.isDisabled());
        await page.locator('[data-testid="chatgpt-backup-account"][data-account-id="acc-2"]').check();
        assert(await exportButton.isEnabled());
        const getsBefore = state.accountGets;
        await exportButton.click();
        await idle();
        assert.match(await tid('chatgpt-backup-error').textContent(), /账户已变化，请刷新后重试/);
        assert(state.accountGets > getsBefore, '导出冲突应刷新账户列表');
        assert.equal(state.exports[0].ifMatch, '"v1"', '导出应复用已缓存的账户版本');
        const [download] = await Promise.all([page.waitForEvent('download'), exportButton.click()]);
        await idle();
        assert.equal(download.suggestedFilename(), 'chatgpt-accounts-20261105-080000.aigw-backup');
        assert.deepEqual(state.exports[1], { body: { accountIds: ['acc-2'], password: EXPORT_PASSWORD }, ifMatch: '"v1b"' });
        assert.equal(await password.inputValue(), '');
        assert.equal(await confirm.inputValue(), '');
        console.log('通过：两个新页签与格式说明、导出密码规则和账户选择、If-Match、冲突刷新、下载名与密码清空');

        await password.fill(EXPORT_PASSWORD);
        await confirm.fill(EXPORT_PASSWORD);
        await tid('chatgpt-restore-tab').click();
        assert.equal(await password.inputValue(), '', '切换页签应清空备份密码');
        const restoreFile = tid('chatgpt-restore-file');
        const restorePassword = tid('chatgpt-restore-password');
        const preview = tid('chatgpt-restore-preview');
        const apply = tid('chatgpt-restore-apply');
        assert.equal(await restorePassword.getAttribute('type'), 'password');
        assert.equal(await restorePassword.getAttribute('autocomplete'), 'off');
        assert(await tid('chatgpt-restore-metadata-keep').isChecked());
        assert(!(await tid('chatgpt-restore-paused').isChecked()));
        // 超过上限的文件不读入状态，也不产生预检请求。
        await restoreFile.setInputFiles({ name: 'oversized.aigw-backup', mimeType: 'application/octet-stream', buffer: Buffer.alloc(16 * 1024 * 1024 + 1, 65) });
        assert.match(await tid('chatgpt-restore-error').textContent(), /超过 16 MiB/);
        assert.equal(await data('chatGPTRestoreBackupText'), '');
        await restoreFile.setInputFiles({ name: 'fake.aigw-backup', mimeType: 'application/octet-stream', buffer: Buffer.from(CONTAINER) });
        await page.waitForFunction(() => !!window.Alpine.$data(document.body).chatGPTRestoreBackupText);
        assert(await preview.isDisabled());
        await restorePassword.fill(WRONG_PASSWORD);
        await preview.click();
        await idle();
        assert.match(await tid('chatgpt-restore-error').textContent(), /密码错误或备份已损坏/);
        await restorePassword.fill(RESTORE_PASSWORD);
        await preview.click();
        await idle();
        assert.deepEqual(state.previews.at(-1), { backup: CONTAINER, password: RESTORE_PASSWORD, metadata: 'keep', restorePaused: false, actions: [] });
        assert.equal(await tid('chatgpt-restore-entry').count(), 3);
        const planText = await tid('chatgpt-restore-plan').textContent();
        for (const value of ['one@example.invalid', 'plus', '已有账户', '匹配本地账户', 'codex-main', '重映射', '可能是旧备份']) assert(planText.includes(value), value + ' 应在计划中展示');
        assert(await apply.isEnabled());
        await tid('chatgpt-restore-action-1').selectOption('update');
        await idle();
        assert.deepEqual(state.previews.at(-1).actions, [{ index: 1, action: 'update', targetAccountId: 'acc-1' }]);
        await tid('chatgpt-restore-action-2').selectOption('skip');
        await idle();
        assert.deepEqual(state.previews.at(-1).actions, [{ index: 1, action: 'update', targetAccountId: 'acc-1' }, { index: 2, action: 'skip' }]);
        await tid('chatgpt-restore-action-2').selectOption('add');
        await idle();
        await tid('chatgpt-restore-metadata-replace').check();
        assert.equal(await data('chatGPTRestorePlan'), null, '元数据选项变化应作废计划');
        await tid('chatgpt-restore-paused').check();
        await preview.click();
        await idle();
        assert.equal(state.previews.at(-1).metadata, 'replace');
        assert.equal(state.previews.at(-1).restorePaused, true);
        assert.match(await tid('chatgpt-restore-plan').textContent(), /标签与备注将用备份值覆盖/);
        assert.match(await tid('chatgpt-restore-plan').textContent(), /恢复后为暂停状态/);
        console.log('通过：文件大小边界、密码错误文案、恢复请求体、逐条计划、动作切换与元数据/暂停变化提示');

        await apply.click();
        await idle();
        assert.match(await tid('chatgpt-restore-error').textContent(), /请重新预检/);
        assert.equal(await data('chatGPTRestorePlan'), null);
        assert.equal(await data('chatGPTRestoreBackupText'), CONTAINER, '应用过期保留文件内容供重新预检');
        await preview.click();
        await idle();
        const expectedToken = (await data('chatGPTRestorePlan')).previewToken;
        const expectedIfMatch = lastPreviewETag;
        await apply.click();
        await idle();
        assert.equal(state.applies.at(-1).ifMatch, expectedIfMatch);
        assert.equal(state.applies.at(-1).body.previewToken, expectedToken);
        assert.deepEqual(state.applies.at(-1).body, { ...state.previews.at(-1), previewToken: expectedToken });
        assert.equal(await restorePassword.inputValue(), '');
        assert.equal(await restoreFile.inputValue(), '');
        assert.equal(await data('chatGPTRestoreBackupText'), '');
        assert.equal(await data('chatGPTRestorePlan'), null);
        assert.match(await tid('chatgpt-restore-result').textContent(), /新增 2 个，更新 1 个，跳过 0 个/);
        assert.equal(await tid('chatgpt-restore-mapping-row').count(), 3);
        assert.match(await tid('chatgpt-restore-mapping').textContent(), /acc-9[\s\S]*acc-new-9[\s\S]*重映射/);
        assert.match(await tid('chatgpt-restore-result').textContent(), /不是一次原子操作/);
        console.log('通过：预检过期需重新预检、应用携带 previewToken 与预检 ETag、成功后清空密码/文件、结果计数与重映射表');
        const configDialog = tid('config-transfer-dialog');
        await tid('chatgpt-restore-use-mappings').click();
        await configDialog.waitFor({ state: 'visible' });
        await dialog.waitFor({ state: 'hidden' });
        assert.deepEqual(await data('chatGPTRestoreAccountMappings'), { 'acc-1': 'acc-1', 'acc-9': 'acc-new-9', 'acc-src-3': 'acc-src-3' });
        assert.equal(await data('configTransferTab'), 'import');
        await tid('config-transfer-import-content').fill(CONFIG_YAML);
        await tid('config-transfer-preview').click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).configTransferPreviewing);
        const prefilled = tid('config-transfer-mapping-prefilled');
        await prefilled.waitFor({ state: 'visible' });
        assert.match(await prefilled.textContent(), /已按备份恢复结果预填账户映射，可修改/);
        assert.deepEqual(await data('configTransferImportAccountMappings'), { 'acc-9': 'acc-new-9', 'acc-src-3': 'acc-src-3' });
        const mappingSelect = id => page.locator(`[data-testid="config-transfer-import-account-mapping"][data-source-account-id="${id}"]`);
        assert.equal(await mappingSelect('acc-9').inputValue(), 'acc-new-9');
        assert.equal(await mappingSelect('acc-other').inputValue(), '');
        // 预填视同改动映射：刚拿到的预览凭证作废，需要用户核对后重新预览。
        assert.equal(await data('configTransferImportToken'), '');
        await mappingSelect('acc-src-3').selectOption('acc-2');
        await mappingSelect('acc-9').selectOption('');
        await tid('config-transfer-preview').click();
        await page.waitForFunction(() => !window.Alpine.$data(document.body).configTransferPreviewing);
        assert.deepEqual(state.configPreviews.at(-1).options.accountMappings, { 'acc-src-3': 'acc-2' });
        assert.deepEqual(await data('configTransferImportAccountMappings'), { 'acc-src-3': 'acc-2' }, '手动映射与手动清空都不应被预填覆盖');
        assert.notEqual(await data('configTransferImportToken'), '', '没有新预填时保留预览凭证');
        assert(!JSON.stringify(state.configPreviews).includes(RESTORE_PASSWORD), '配置迁移请求不应带备份密码');
        await page.keyboard.press('Escape');
        await configDialog.waitFor({ state: 'hidden' });
        console.log('通过：用于配置迁移后打开配置导入、按目录预填映射并提示、手动映射与清空不被覆盖');

        await open('chatgpt-restore-tab');
        await tid('chatgpt-restore-file').setInputFiles({ name: 'fake.aigw-backup', mimeType: 'application/octet-stream', buffer: Buffer.from(CONTAINER) });
        await page.waitForFunction(() => !!window.Alpine.$data(document.body).chatGPTRestoreBackupText);
        await tid('chatgpt-restore-password').fill(RESTORE_PASSWORD);
        await tid('chatgpt-restore-preview').click();
        await idle();
        await tid('chatgpt-backup-tab').click();
        await tid('chatgpt-backup-password').fill(EXPORT_PASSWORD);
        await tid('chatgpt-backup-password-confirm').fill(EXPORT_PASSWORD);
        await closeByEscape();
        for (const key of ['chatGPTBackupPassword', 'chatGPTBackupPasswordConfirm', 'chatGPTRestorePassword', 'chatGPTRestoreBackupText', 'chatGPTRestoreFileName']) {
            assert.equal(await data(key), '', `关闭弹窗应清空 ${key}`);
        }
        assert.equal(await data('chatGPTRestorePlan'), null);
        assert.equal(await data('chatGPTRestoreResult'), null);
        const storage = await app(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage }, href: location.href }));
        for (const secret of [EXPORT_PASSWORD, RESTORE_PASSWORD, WRONG_PASSWORD, 'ciphertext']) {
            assert(!storage.includes(secret), '浏览器存储与 URL 不应包含 ' + secret);
            assert(!consoleTexts.some(text => text.includes(secret)), '控制台不应输出 ' + secret);
        }
        assert.deepEqual(state.external, []);
        console.log('通过：关闭弹窗清空密码、文件与计划；localStorage/sessionStorage/URL/控制台无密码，无外部请求');

        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            await open('chatgpt-backup-tab');
            await tid('chatgpt-backup-password').fill('FAKE-short');
            assert(await dialog.locator('[role=dialog]').evaluate(el => el.scrollWidth <= el.clientWidth), `${width}px 下备份页签不应横向溢出`);
            await page.screenshot({ path: path.join(shotDir, `chatgpt-backup-${width}.png`) });
            await tid('chatgpt-restore-tab').click();
            await tid('chatgpt-restore-file').setInputFiles({ name: 'fake.aigw-backup', mimeType: 'application/octet-stream', buffer: Buffer.from(CONTAINER) });
            await page.waitForFunction(() => !!window.Alpine.$data(document.body).chatGPTRestoreBackupText);
            await tid('chatgpt-restore-password').fill(RESTORE_PASSWORD);
            await tid('chatgpt-restore-preview').click();
            await idle();
            await tid('chatgpt-restore-plan').waitFor({ state: 'visible' });
            assert(await dialog.locator('[role=dialog]').evaluate(el => el.scrollWidth <= el.clientWidth), `${width}px 下恢复计划不应横向溢出`);
            await page.screenshot({ path: path.join(shotDir, `chatgpt-restore-${width}.png`) });
            await closeByEscape();
        }
        assert.deepEqual(errors, []);
        console.log('通过：1440 / 390 宽度无横向溢出，截图位于 ' + shotDir + '；无页面脚本错误');
    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
