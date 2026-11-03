#!/usr/bin/env node
'use strict';

// 页签滚动回归：只读前端产物，所有接口都返回假数据，不访问真实配置或上游。
// 用法：node scripts/verify-nav-scroll.cjs [--baseline | --measure]
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

function loadPlaywright() {
    for (const candidate of [process.env.PLAYWRIGHT_MODULE, 'playwright', '/root/.npm/_npx/e41f203b7505f1fb/node_modules/playwright'].filter(Boolean)) {
        try { return require(candidate); } catch (error) {
            if (error.code !== 'MODULE_NOT_FOUND') throw error;
        }
    }
    throw new Error('找不到 Playwright，请通过 PLAYWRIGHT_MODULE 指定模块路径');
}

const { chromium } = loadPlaywright();
const webRoot = path.resolve(__dirname, '../cmd/gateway/web');
const origin = 'http://127.0.0.1:19999';
const baseline = process.argv.includes('--baseline');
const measure = process.argv.includes('--measure');
const config = {
    revision: 'fake-nav-scroll', restartRequired: [], path: '/tmp/fake/config.yaml',
    host: '127.0.0.1', port: 7789, timeout: 120000, streamActivityTimeout: 60000,
    failover: { enabled: true, maxAttempts: 2, maxRetryAfterMs: 5000, onTransportError: true, onServerError: true, onRateLimit: true },
    breaker: { enabled: true, consecutiveFailures: 3, openMs: 30000, halfOpenProbes: 1 },
    providers: { example: { baseUrl: 'https://upstream.invalid/v1', format: 'openai', maxConcurrent: 5 } },
    routes: Array.from({ length: 32 }, (_, index) => ({ match: `model-${index}-*`, provider: 'example', model: `model-${index}` }))
};
const logs = Array.from({ length: 50 }, (_, index) => ({
    id: `fake-log-${index}`, time: '2026-11-03T00:00:00Z', model: `model-${index}`,
    provider: 'example', status: 200, stream: false, durationMs: 100, attempts: 1
}));

async function readScroll(page) {
    return page.evaluate(() => {
        const app = window.Alpine.$data(document.body);
        const main = document.querySelector('main');
        const configSection = main.querySelector('section[x-show="isConfigTab()"]');
        const visual = configSection.querySelector('div[x-show="editMode === \'visual\'"]');
        const configScroll = visual.firstElementChild;
        const monitor = main.querySelector('section[x-show="activeTab === \'monitor\'"]');
        const logScroll = main.querySelector('section[x-show="activeTab === \'logs\'"] table').parentElement;
        const content = app.isConfigTab() ? configScroll : app.activeTab === 'monitor' ? monitor : logScroll;
        const container = window.matchMedia('(min-width: 1024px)').matches ? content : main;
        return { tab: app.activeTab, top: container.scrollTop, max: container.scrollHeight - container.clientHeight, main: main.scrollTop, content: content.scrollTop };
    });
}

async function waitForLayout(page, tab) {
    await page.waitForFunction((target) => window.Alpine?.$data(document.body).activeTab === target, tab);
    // 按实际动画状态等布局落定，不用固定等待时间掩盖恢复时序问题。
    await page.waitForFunction(() => !document.querySelector('main').getAnimations({ subtree: true }).some((animation) =>
        animation.playState === 'running' && animation.effect?.getComputedTiming().endTime !== Infinity));
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
}

async function selectTab(page, tab) {
    const labels = { routes: '路由', settings: '全局设置', providers: '提供商', monitor: '监控仪表盘', logs: '请求日志' };
    await page.locator('nav [role="tab"]').filter({ hasText: labels[tab] }).click();
    await waitForLayout(page, tab);
}

async function scrollTo(page, top) {
    await page.evaluate((value) => {
        const app = window.Alpine.$data(document.body);
        const main = document.querySelector('main');
        const configSection = main.querySelector('section[x-show="isConfigTab()"]');
        const visual = configSection.querySelector('div[x-show="editMode === \'visual\'"]');
        const content = app.isConfigTab() ? visual.firstElementChild
            : app.activeTab === 'monitor' ? main.querySelector('section[x-show="activeTab === \'monitor\'"]')
                : main.querySelector('section[x-show="activeTab === \'logs\'"] table').parentElement;
        const container = window.matchMedia('(min-width: 1024px)').matches ? content : main;
        container.scrollTo({ top: value === 'bottom' ? container.scrollHeight : value, behavior: 'instant' });
    }, top);
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    return readScroll(page);
}

async function measureSwitch(page, tab, expected) {
    return page.evaluate(async ({ target, expectedTop }) => {
        const app = window.Alpine.$data(document.body);
        const main = document.querySelector('main');
        const visual = main.querySelector('section[x-show="isConfigTab()"] div[x-show="editMode === \'visual\'"]');
        const container = window.matchMedia('(min-width: 1024px)').matches ? visual.firstElementChild : main;
        const started = performance.now();
        const wrongVisibleFrames = [];
        let frames = 0;
        app.selectNav(target);
        while (frames < 120) {
            await new Promise((resolve) => requestAnimationFrame(resolve));
            frames++;
            const style = getComputedStyle(main);
            const correctPosition = Math.abs(container.scrollTop - expectedTop) <= 2;
            if (style.visibility === 'visible' && Number(style.opacity) > 0.01 && !correctPosition) {
                wrongVisibleFrames.push({ frame: frames, top: container.scrollTop });
            }
            if (!app._navScrollRestoreTab && correctPosition) break;
        }
        const fadeEffect = app._navFadeAnimation?.effect;
        const fade = fadeEffect ? {
            duration: fadeEffect.getTiming().duration,
            opacity: fadeEffect.getKeyframes().map((frame) => Number(frame.opacity))
        } : null;
        return { tab: target, elapsedMs: Math.round(performance.now() - started), frames, top: container.scrollTop, wrongVisibleFrames, fade };
    }, { target: tab, expectedTop: expected });
}

function assertTop(actual, expected, message) {
    assert.ok(Math.abs(actual.top - expected) <= 2, `${message}：实际 ${actual.top}，期望 ${expected}`);
}

async function runViewport(browser, viewport) {
    const page = await browser.newPage({ viewport });
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.route('**/*', async (route) => {
        const url = new URL(route.request().url());
        assert.equal(url.origin, origin, '不应访问外部地址');
        if (url.pathname === '/api/config') return route.fulfill({ json: config });
        if (url.pathname === '/api/chatgpt/accounts') return route.fulfill({ json: { data: [] } });
        if (url.pathname === '/api/logs') return route.fulfill({ json: { data: logs } });
        if (url.pathname === '/api/metrics' || url.pathname === '/health') return route.fulfill({ json: {} });
        if (url.pathname.startsWith('/api/')) return route.fulfill({ json: {} });
        const relative = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
        const file = path.resolve(webRoot, relative);
        assert.ok(file.startsWith(webRoot + path.sep), '资源必须位于前端目录');
        const contentType = relative.endsWith('.html') ? 'text/html; charset=utf-8' : relative.endsWith('.js') ? 'application/javascript' : relative.endsWith('.css') ? 'text/css' : 'font/woff2';
        return route.fulfill({ contentType, body: fs.readFileSync(file) });
    });
    try {
        await page.goto(origin + '/#routes', { waitUntil: 'load' });
        await page.waitForFunction(() => window.Alpine?.$data(document.body).revision === 'fake-nav-scroll');
        await waitForLayout(page, 'routes');
        await scrollTo(page, 0);
        await selectTab(page, 'settings');
        const settingsBottom = await scrollTo(page, 'bottom');
        assert.ok(settingsBottom.max > 200, '全局设置必须足够长，能复现滚动');
        await selectTab(page, 'routes');
        const routeAfterReturn = await readScroll(page);
        console.log(`${viewport.width}px 基线场景：设置底部=${settingsBottom.top}，切回路由=${routeAfterReturn.top}`);
        if (baseline) {
            assert.ok(routeAfterReturn.top > 20, '修复前应能复现路由被带离顶部');
            console.log(`${viewport.width}px 已复现：设置页滚动串到路由页`);
            return;
        }
        if (measure) {
            const toSettings = await measureSwitch(page, 'settings', settingsBottom.top);
            await waitForLayout(page, 'settings');
            const toRoutes = await measureSwitch(page, 'routes', 0);
            await waitForLayout(page, 'routes');
            console.log(`${viewport.width}px 恢复耗时：${JSON.stringify([toSettings, toRoutes])}`);
            assert.deepEqual(errors, [], '浏览器不应有脚本错误');
            return;
        }
        assertTop(routeAfterReturn, 0, '路由原在顶部，切回后应仍在顶部');
        const timings = [await measureSwitch(page, 'settings', settingsBottom.top)];
        await waitForLayout(page, 'settings');
        timings.push(await measureSwitch(page, 'routes', 0));
        await waitForLayout(page, 'routes');
        for (const timing of timings) {
            assert.deepEqual(timing.wrongVisibleFrames, [], '切换中不应显示错误的滚动位置');
            assert.ok(timing.frames <= 4, `位置恢复不应等待整段动画：${JSON.stringify(timing)}`);
            assert.deepEqual(timing.fade, { duration: 250, opacity: [0.6, 1] }, '页签应使用 250ms 柔和淡入');
        }
        console.log(`${viewport.width}px 无跳动切换：${JSON.stringify(timings)}`);
        await selectTab(page, 'settings');
        assertTop(await readScroll(page), settingsBottom.top, '全局设置应恢复自己的底部位置');
        await selectTab(page, 'routes');
        const routeMiddle = await scrollTo(page, 450);
        await selectTab(page, 'settings');
        const settingsMiddle = await scrollTo(page, 260);
        await selectTab(page, 'routes');
        assertTop(await readScroll(page), routeMiddle.top, '路由应恢复自己的中间位置');
        await selectTab(page, 'routes');
        assertTop(await readScroll(page), routeMiddle.top, '重复点击当前页签不应改变位置');
        await selectTab(page, 'settings');
        assertTop(await readScroll(page), settingsMiddle.top, '设置应恢复自己的中间位置');
        await page.goBack();
        await waitForLayout(page, 'routes');
        assertTop(await readScroll(page), routeMiddle.top, '浏览器后退应恢复路由位置');
        await page.goForward();
        await waitForLayout(page, 'settings');
        assertTop(await readScroll(page), settingsMiddle.top, '浏览器前进应恢复设置位置');
        await selectTab(page, 'providers');
        assertTop(await readScroll(page), 0, '首次访问提供商应从顶部开始');
        await selectTab(page, 'settings');
        assertTop(await readScroll(page), settingsMiddle.top, '经过提供商页后设置位置不应丢失');
        await selectTab(page, 'logs');
        const logMiddle = await scrollTo(page, 320);
        assert.ok(logMiddle.max > 320, '日志列表应足够长');
        await selectTab(page, 'monitor');
        assertTop(await readScroll(page), 0, '首次进入监控应从顶部开始');
        const monitorMiddle = await scrollTo(page, 180);
        await selectTab(page, 'logs');
        assertTop(await readScroll(page), logMiddle.top, '日志页应恢复自己的滚动位置');
        await selectTab(page, 'monitor');
        assertTop(await readScroll(page), monitorMiddle.top, '监控页应恢复自己的滚动位置');
        await selectTab(page, 'routes');
        assertTop(await readScroll(page), routeMiddle.top, '跨过监控与日志后路由位置不应丢失');
        // 在隐藏动画尚未结束时往返，防止旧异步恢复覆盖新切换。
        await page.evaluate(async () => {
            const app = window.Alpine.$data(document.body);
            app.selectNav('settings');
            await new Promise((resolve) => requestAnimationFrame(resolve));
            app.selectNav('providers');
            await new Promise((resolve) => requestAnimationFrame(resolve));
            app.selectNav('routes');
        });
        await waitForLayout(page, 'routes');
        assertTop(await readScroll(page), routeMiddle.top, '快速切换后路由仍应恢复原位置');
        await selectTab(page, 'settings');
        assertTop(await readScroll(page), settingsMiddle.top, '快速切换不应覆盖设置的已保存位置');
        await page.reload({ waitUntil: 'load' });
        await page.waitForFunction(() => window.Alpine?.$data(document.body).revision === 'fake-nav-scroll');
        await waitForLayout(page, 'settings');
        assertTop(await readScroll(page), 0, '刷新后不应持久化滚动位置');
        assert.deepEqual(errors, [], '浏览器不应有脚本错误');
        console.log(`${viewport.width}px 通过：顶部/底部/中间位置、重复点击、前进后退、五个页签、快速切换、刷新`);
    } finally {
        await page.close();
    }
}

(async () => {
    const browser = await chromium.launch({ headless: true });
    try {
        for (const viewport of [{ width: 1440, height: 900 }, { width: 900, height: 900 }]) {
            await runViewport(browser, viewport);
        }
    } finally {
        await browser.close();
    }
})().catch((error) => {
    console.error(error);
    process.exitCode = 1;
});
