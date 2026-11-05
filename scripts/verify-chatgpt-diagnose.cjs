#!/usr/bin/env node
'use strict';

// 验证账户诊断的显式出网选择、消费确认、取消、快捷键隔离、焦点和响应式结果展示。
// 所有请求都在浏览器内拦截，不读取本机账户库，也不连接真实上游。
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19995';
const accounts = [{
    id: 'fake-diagnose', email: 'diagnose@example.invalid', status: 'ready', plan: 'plus', providers: ['saved-codex'],
    expiresAt: '2099-01-01T00:00:00Z', authorization: { state: 'valid', message: '访问令牌有效', canRefresh: true },
    usageState: { state: 'unknown', freshness: 'none', message: '尚未查询用量' },
    usage: { lastCheckedAt: new Date().toISOString() }, invocation: { state: 'unknown', message: '尚未实测' }
}];
const provider = (id, enabled = true) => ({ baseUrl: 'https://chatgpt.com/backend-api/codex', format: 'openai-responses', authType: 'chatgptOAuth', accountId: id, enabled });
const config = { revision: 'fake', providers: { 'saved-codex': provider('fake-diagnose'), 'saved-disabled': provider('fake-diagnose', false), other: provider('another-account') }, routes: [], failover: {}, breaker: {}, providerHealth: {}, cache: {} };
function reportFor(input) {
    const selected = new Set(input.checks || ['local']);
    return { accountId: 'fake-diagnose', durationMs: 1400, budgetMs: 45000, tokenExchanged: selected.has('refresh'), actualCall: selected.has('inference'),
        steps: ['local', 'refresh', 'usage', 'inference'].map(name => ({ name, status: selected.has(name) ? name === 'usage' ? 'failure' : 'success' : 'skipped',
            message: !selected.has(name) ? '未选择此步骤' : name === 'usage' ? '用量服务拒绝查询，这不单独证明刷新凭据失效' : name === 'inference' ? '此账户经所选 Provider 可以调用此模型' : '检查成功',
            category: name === 'usage' && selected.has(name) ? 'query_failed' : '', httpStatus: name === 'usage' && selected.has(name) ? 401 : 0,
            proxySource: selected.has(name) && name !== 'local' ? name === 'inference' ? 'provider_proxy' : 'oauth_proxy' : '', durationMs: 100,
            provider: name === 'inference' && selected.has(name) ? input.provider : '', model: name === 'inference' && selected.has(name) ? input.model : '',
            details: name === 'local' ? ['本地检查不出网，不能证明模型调用可用'] : [] })),
        account: { ...accounts[0], invocation: selected.has('inference') ? { state: 'success', message: '最近一次真实调用成功', provider: input.provider, model: input.model } : accounts[0].invocation }
    };
}

(async () => {
    const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined, args: ['--no-proxy-server'] });
    try {
        const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
        const errors = [], diagnoses = [], saves = [];
        let mode = 'normal', release;
        page.on('pageerror', e => errors.push(e.message));
        page.on('console', m => { if (/Alpine Expression Error/.test(m.text())) errors.push(m.text()); });
        await page.route('**/*', async route => {
            const req = route.request(), url = new URL(req.url());
            if (url.origin !== origin) return route.abort();
            if (url.pathname.endsWith('/diagnose')) {
                const input = req.postDataJSON();
                diagnoses.push(input);
                if (mode === 'hold') await new Promise(resolve => { release = resolve; });
                if (mode === 'error') return route.fulfill({ status: 409, json: { error: { message: '该账户已有诊断在进行，请等待完成' } } });
                try { return await route.fulfill({ json: { data: reportFor(input) } }); } catch { return; }
            }
            if (url.pathname === '/api/config') {
                if (req.method() !== 'GET') saves.push(req.method());
                return route.fulfill({ json: config });
            }
            if (url.pathname.startsWith('/api/chatgpt/accounts')) return route.fulfill({ json: { data: accounts } });
            if (url.pathname === '/health') return route.fulfill({ json: { status: 'ok', queues: {}, providerHealth: {}, breakers: null } });
            if (url.pathname.startsWith('/api/')) return route.fulfill({ json: { data: [], summary: {}, providers: {} } });
            const file = path.resolve(root, url.pathname === '/' ? 'index.html' : url.pathname.replace(/^\//, ''));
            if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
            const type = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' }[path.extname(file)] || 'application/octet-stream';
            return route.fulfill({ contentType: type, body: fs.readFileSync(file) });
        });
        await page.goto(origin + '/#providers');
        await page.waitForFunction(() => window.Alpine?.$data(document.body).chatgptAccounts.length === 1 && !window.Alpine.$data(document.body)._navScrollRestoreTab);
        const app = fn => page.evaluate(fn);
        const dialog = page.getByTestId('chatgpt-diagnose-dialog'), start = page.getByTestId('chatgpt-diagnose-start');
        const open = async () => { await page.getByTestId('chatgpt-diagnose-open').click(); await dialog.waitFor({ state: 'visible' }); };
        const idle = () => page.waitForFunction(() => !window.Alpine.$data(document.body).diagnosingChatGPTAccount);
        await open();
        assert.equal(diagnoses.length, 0, '打开诊断不能出网');
        // Alpine 过渡会把 $nextTick 推迟到进入动画的第二帧，等焦点落定后再断言。
        await page.waitForFunction(() => document.activeElement?.dataset?.testid === 'chatgpt-diagnose-start', null, { timeout: 2000 })
            .catch(() => assert.fail('初始焦点应在启动按钮'));
        await start.click(); await idle();
        assert.deepEqual(diagnoses[0], { checks: ['local'] });
        assert.equal(await page.getByTestId('chatgpt-diagnose-step').count(), 4);
        assert.match(await page.getByTestId('chatgpt-diagnose-results').textContent(), /没有发起实际模型调用/);
        console.log('通过：打开只准备本地表单，默认诊断只提交本地检查，逐步结果显示未执行项');
        // 未保存配置里新增的 Provider 不得变成诊断目标。
        await app(() => { window.Alpine.$data(document.body).config.providers.unsaved = { authType: 'chatgptOAuth', accountId: 'fake-diagnose' }; });
        await page.getByTestId('chatgpt-diagnose-refresh').check();
        await page.locator('#chatgpt-diagnose-refresh-mode').selectOption('force');
        await page.getByTestId('chatgpt-diagnose-usage').check();
        await page.getByTestId('chatgpt-diagnose-inference').check();
        const options = await page.getByTestId('chatgpt-diagnose-provider').locator('option').allTextContents();
        assert(!options.some(text => /unsaved|other/.test(text)), '目标必须来自此账户已保存的绑定');
        assert(options.some(text => /已停用，将跳过/.test(text)), '停用目标应说明跳过行为');
        await page.getByTestId('chatgpt-diagnose-provider').selectOption('saved-codex');
        await page.getByTestId('chatgpt-diagnose-model').fill('fake-model');
        assert(await start.isDisabled(), '未确认额度不能实际调用');
        await page.getByTestId('chatgpt-diagnose-confirm').check();
        await page.getByTestId('chatgpt-diagnose-model').fill('fake-model-updated');
        assert(!(await page.getByTestId('chatgpt-diagnose-confirm').isChecked()), '改变目标后必须重新消费确认');
        await page.getByTestId('chatgpt-diagnose-confirm').check();
        await start.click(); await idle();
        assert.deepEqual(diagnoses[1], { checks: ['local', 'refresh', 'usage', 'inference'], refreshMode: 'force', provider: 'saved-codex', model: 'fake-model-updated', confirm: 'consume_quota' });
        // 图标字体是按名称子集化的连字字体，未收录的名称会显示成英文单词；按渲染宽度判断是否退化。
        await page.evaluate(() => document.fonts.ready);
        const brokenIcons = await page.evaluate(() => [...document.querySelectorAll('[data-testid="chatgpt-diagnose-dialog"] .material-symbols-outlined, [data-testid="chatgpt-diagnose-open"] .material-symbols-outlined')]
            .filter(el => el.getClientRects().length)
            .filter(el => el.getBoundingClientRect().width > parseFloat(getComputedStyle(el).fontSize) * 1.5)
            .map(el => el.textContent.trim()));
        assert.deepEqual(brokenIcons, [], '诊断界面的图标必须都在字体子集内');
        const resultText = await page.getByTestId('chatgpt-diagnose-results').textContent();
        assert.match(resultText, /专用 OAuth 代理/); assert.match(resultText, /Provider 代理/);
        assert.match(resultText, /用量查询失败/); assert.match(resultText, /HTTP 401/);
        assert.match(resultText, /不计入普通请求统计/);
        assert.equal(await app(() => window.Alpine.$data(document.body).chatgptAccounts[0].invocation.state), 'success');
        assert(!(await page.getByTestId('chatgpt-diagnose-confirm').isChecked()), '执行后清除消费确认');
        console.log('通过：只选已保存绑定，实际调用须独立确认，改目标重新确认，结果按步骤与代理来源展示');
        await page.getByTestId('chatgpt-diagnose-confirm').check();
        await page.keyboard.press('Control+s'); await page.keyboard.press('Meta+s');
        assert.deepEqual(saves, [], '诊断浮层不能触发底层配置保存');
        await dialog.getByRole('button', { name: '关闭账户诊断', exact: true }).focus();
        await page.keyboard.press('Shift+Tab');
        assert(await start.evaluate(el => el === document.activeElement), '反向 Tab 应回到末尾可用控件');
        await page.keyboard.press('Tab');
        assert(await dialog.getByRole('button', { name: '关闭账户诊断', exact: true }).evaluate(el => el === document.activeElement), 'Tab 应回到首个控件');
        assert(await page.locator('body > .flex.h-screen').evaluate(el => el.inert), '底层操作区应不可交互');
        await page.keyboard.press('Escape'); await dialog.waitFor({ state: 'hidden' });
        await page.waitForFunction(() => document.activeElement?.dataset?.testid === 'chatgpt-diagnose-open', null, { timeout: 2000 })
            .catch(() => assert.fail('关闭后焦点应返回原账户'));
        console.log('通过：保存快捷键隔离、Tab 循环、底层不可交互，关闭后焦点返回');
        await open();
        await page.getByTestId('chatgpt-diagnose-usage').check();
        mode = 'hold';
        await start.click();
        await page.waitForFunction(() => window.Alpine.$data(document.body).diagnosingChatGPTAccount);
        assert(await start.isDisabled(), '执行中禁止重复提交');
        assert(await page.getByTestId('chatgpt-diagnose-usage').isDisabled(), '执行中禁止改选');
        await page.getByTestId('chatgpt-diagnose-cancel').click();
        await idle();
        assert.match(await page.getByTestId('chatgpt-diagnose-notice').textContent(), /已经完成.*不会回滚/);
        release();
        await page.waitForTimeout(50);
        assert.equal(await app(() => window.Alpine.$data(document.body).chatGPTDiagnoseReport), null, '取消后的迟到结果不应覆盖界面');
        mode = 'error'; await start.click(); await idle();
        assert.match(await dialog.getByRole('alert').textContent(), /已有诊断在进行/);
        await page.keyboard.press('Escape'); await dialog.waitFor({ state: 'hidden' });
        assert.equal(await app(() => window.Alpine.$data(document.body)._chatGPTDiagnoseController), null);
        console.log('通过：忙碌态防重复提交，取消中止等待并忽略迟到结果，冲突可见，关闭清理请求');
        mode = 'normal';
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: 900 });
            if (width === 390) {
                // 既有问题：窄屏侧栏不收起，账户行操作列被侧栏遮住。先记录同列已有导出按钮的遮挡情况作对照，
                // 再直接打开浮层验证诊断浮层本身的窄屏布局。
                const covered = await page.evaluate(() => [...document.querySelectorAll('[aria-label="导出 ChatGPT 账户"], [data-testid="chatgpt-diagnose-open"]')].map(el => {
                    el.scrollIntoView({ block: 'center', inline: 'center' });
                    const box = el.getBoundingClientRect();
                    const top = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
                    return { label: el.getAttribute('aria-label'), covered: !el.contains(top) };
                }));
                console.log('390px 行操作遮挡对照：' + JSON.stringify(covered));
                await app(() => { const data = window.Alpine.$data(document.body); data.openChatGPTDiagnoseDialog(data.chatgptAccounts[0]); });
                await dialog.waitFor({ state: 'visible' });
            } else {
                await open();
            }
            await page.getByTestId('chatgpt-diagnose-usage').check();
            await page.getByTestId('chatgpt-diagnose-inference').check();
            await page.getByTestId('chatgpt-diagnose-provider').selectOption('saved-codex');
            await page.getByTestId('chatgpt-diagnose-model').fill('fake-model');
            await page.getByTestId('chatgpt-diagnose-confirm').check();
            await start.click(); await idle();
            assert(await dialog.getByRole('dialog').evaluate(el => el.scrollWidth <= el.clientWidth), `${width}px 下浮层不应横向溢出`);
            await page.screenshot({ path: `/tmp/chatgpt-diagnose-${width}.png` });
            await page.keyboard.press('Escape'); await dialog.waitFor({ state: 'hidden' });
        }
        assert.deepEqual(errors, []);
        console.log('通过：桌面和窄屏结果可见且无横向溢出；没有页面表达式错误');

    } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
