#!/usr/bin/env node
'use strict';

/**
 * 提供商 / 路由配置导入导出的浏览器交互验证。
 *
 * 只使用页面内的假配置、假账户与假凭据；静态资源来自 cmd/gateway/web 的产物，
 * 所有请求都由 page.route 在本机假 origin 上拦截，不另起 HTTP 服务，不读取真实
 * config.yaml / .env / OAuth 凭据库，也不会访问任何上游。
 *
 * 用法：
 *   node scripts/verify-provider-route-transfer.cjs
 *   node scripts/verify-provider-route-transfer.cjs --baseline
 *
 * --baseline 只检查产物是否已经包含两个入口：缺失时明确报告并以 0 退出，
 * 供主会话在重建产物前确认基线。默认模式要求入口存在并跑完整交互断言。
 * 所有静态内容来自正式产物，不包含临时拼装路径。
 *
 * 本机 Playwright 路径与代理坑见 /root/.claude/notes/playwright-local.md。
 */

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

function loadPlaywright() {
    const candidates = [
        process.env.PLAYWRIGHT_MODULE,
        '/root/.npm/_npx/e41f203b7505f1fb/node_modules/playwright',
        'playwright'
    ].filter(Boolean);
    const errors = [];
    for (const candidate of candidates) {
        try {
            return require(candidate);
        } catch (err) {
            errors.push(`${candidate}: ${err.message}`);
        }
    }
    throw new Error('找不到 Playwright 模块。已尝试：\n' + errors.join('\n'));
}

const { chromium } = loadPlaywright();

const REPO_ROOT = path.resolve(__dirname, '..');
const WEB_ROOT = path.join(REPO_ROOT, 'cmd', 'gateway', 'web');
const ORIGIN = 'http://127.0.0.1:19999';
const FAKE_EXPORT_SECRET = 'sk-export-FAKE-ONLY-IN-DOWNLOAD-123456';
const FAKE_IMPORT_SECRET = 'sk-import-FAKE-NEVER-RENDERED-654321';
const FAKE_PROXY_PASSWORD = 'proxy-FAKE-PASSWORD-987654';

const IMPORT_YAML = `kind: ai-gateway.provider-route-transfer
version: 1
providers:
  example-provider:
    baseUrl: https://upstream.invalid/v1
    format: openai
    apiKey: ${FAKE_IMPORT_SECRET}
    maxConcurrent: 7
  new-provider:
    baseUrl: https://chatgpt.com/backend-api/codex
    format: openai-responses
    apiKey: ""
    authType: chatgptOAuth
    accountId: acc-source-1
routes:
  - match: example-*
    provider: example-provider
    model: example-model
  - match: new-*
    provider: new-provider
    model: new-model
`;

const IMPORT_YAML_NEEDS_MAPPING = `kind: ai-gateway.provider-route-transfer
version: 1
providers:
  new-provider:
    baseUrl: https://chatgpt.com/backend-api/codex
    format: openai-responses
    apiKey: ""
    authType: chatgptOAuth
    accountId: acc-source-9
routes:
  - match: new-*
    provider: new-provider
    model: new-model
`;

const PROVIDERS_ONLY_YAML = `kind: ai-gateway.provider-route-transfer
version: 1
providers:
  new-provider:
    baseUrl: https://upstream.invalid/v2
    format: anthropic
    apiKey: ${FAKE_IMPORT_SECRET}
routes: []
`;

function parseArgs(argv) {
    const options = { baseline: false };
    for (const arg of argv) {
        if (arg === '--baseline') options.baseline = true;
        else throw new Error(`未知参数：${arg}`);
    }
    return options;
}

function ensure(condition, message) {
    if (!condition) throw new Error('断言失败：' + message);
}

function hash(value) {
    return crypto.createHash('sha256').update(value).digest('hex');
}

function makeState() {
    return {
        revision: 'rev-1',
        applyCount: 0,
        getConfigCount: 0,
        lastPreview: null,
        requests: [],
        externalRequests: [],
        exports: [],
        previews: [],
        applies: [],
        putCount: 0,
        config: {
            path: '/tmp/fake-ai-gateway/config.yaml',
            host: '127.0.0.1',
            port: 19999,
            timeout: 120000,
            streamActivityTimeout: 60000,
            cache: { maxAgeDays: 7, maxRecords: 1000 },
            directMode: false,
            directTimeoutNoStream: 60000,
            directTimeoutStreamHeader: 60000,
            directTimeoutStreamActive: 120000,
            contextSafetyMargin: 4096,
            providers: {
                'example-provider': {
                    name: 'example-provider',
                    baseUrl: 'https://upstream.invalid/v1',
                    apiKey: '',
                    apiKeyConfigured: true,
                    apiKeys: [
                        { id: 'key-1', name: '主密钥', value: '', valueConfigured: true, enabled: true },
                        { id: 'key-2', name: '备用', value: '', valueConfigured: true, enabled: false }
                    ],
                    format: 'openai',
                    maxConcurrent: 5,
                    maxPerSecond: 0,
                    maxQueueWait: 30000,
                    enabled: true
                },
                'vision-provider': {
                    name: 'vision-provider',
                    baseUrl: 'https://upstream.invalid/vision',
                    apiKey: '',
                    apiKeyConfigured: true,
                    format: 'anthropic',
                    maxConcurrent: 3,
                    maxPerSecond: 0,
                    maxQueueWait: 30000,
                    enabled: true
                },
                'oauth-provider': {
                    name: 'oauth-provider',
                    baseUrl: 'https://upstream.invalid/oauth',
                    apiKey: '',
                    apiKeyConfigured: false,
                    format: 'openai-responses',
                    authType: 'chatgptOAuth',
                    accountId: 'acc-target-1',
                    maxConcurrent: 5,
                    maxPerSecond: 0,
                    maxQueueWait: 30000,
                    enabled: false
                }
            },
            routes: [
                {
                    match: 'example-*',
                    targets: [{ provider: 'example-provider', model: 'example-model' }],
                    enabled: true
                },
                {
                    match: 'vision-*',
                    provider: 'vision-provider',
                    model: 'vision-model',
                    vision: { provider: 'vision-provider', model: 'vision-model' },
                    enabled: true
                }
            ],
            failover: {
                enabled: false,
                maxAttempts: null,
                maxRetryAfterMs: null,
                onTransportError: true,
                onServerError: true,
                onClientError: true,
                onRateLimit: true,
                onQueueTimeout: true,
                onStreamHeaderTimeout: true,
                onRequestTimeout: false,
                onAuthError: true
            },
            breaker: {
                enabled: false,
                consecutiveFailures: null,
                openMs: null,
                halfOpenProbes: null
            },
            providerHealth: { checkIntervalSeconds: 0 }
        },
        accounts: [
            { id: 'acc-target-1', email: 'target1@example.invalid', plan: 'plus' },
            { id: 'acc-target-2', email: 'target2@example.invalid', plan: 'pro' },
            { id: 'acc-source-1', email: 'source1@example.invalid', plan: 'plus' }
        ]
    };
}

function buildPreview(state, body) {
    const options = body?.options || {};
    const mode = options.mode === 'replace' ? 'replace' : 'merge';
    const content = String(body?.content || '');
    const hasRoutes = /(^|\n)routes:\s*\n\s*-/.test(content);
    const hasProviders = /(^|\n)providers:\s*\n\s+\S/.test(content);
    const sourceAccountId = content.includes('acc-source-9') ? 'acc-source-9' : 'acc-source-1';

    const providers = [
        {
            name: 'example-provider',
            keyCount: 2,
            apiKeyConfigured: true,
            sourceAccountId: '',
            routeIndexes: [0],
            selected: true,
            conflict: true
        },
        {
            name: 'new-provider',
            keyCount: 1,
            apiKeyConfigured: true,
            sourceAccountId,
            routeIndexes: [1],
            selected: true,
            conflict: false
        }
    ];
    const routes = [
        {
            index: 0,
            match: 'example-*',
            providerDependencies: ['example-provider'],
            selected: true,
            targetIndexes: [0]
        },
        {
            index: 1,
            match: 'new-*',
            providerDependencies: ['new-provider'],
            selected: true,
            targetIndexes: []
        }
    ];

    if (options.selection) {
        const selectedProviders = new Set(options.selection.providers || []);
        const selectedRoutes = new Set((options.selection.routes || []).map(Number));
        providers.forEach((provider) => { provider.selected = selectedProviders.has(provider.name); });
        routes.forEach((route) => { route.selected = selectedRoutes.has(route.index); });
    }

    const providerConflictAction = options.conflicts?.providers?.['example-provider'] === 'overwrite'
        ? 'overwrite'
        : 'keep';
    const routeConflictAction = options.conflicts?.routes?.['0'] === 'overwrite'
        ? 'overwrite'
        : 'keep';

    const blockers = [];
    if (sourceAccountId === 'acc-source-9' && !(options.accountMappings || {})['acc-source-9']) {
        blockers.push({
            code: 'unresolved_account_mapping',
            message: 'OAuth 提供商缺少目标账户映射，请先在下方选择目标账户后重新预览',
            provider: 'new-provider',
            routeIndex: 1
        });
    }
    if (mode === 'replace' && (!hasProviders || !hasRoutes)) {
        blockers.push({
            code: 'replace_requires_complete_bundle',
            message: '整体替换需要非空的提供商和路由集合',
            field: 'routes'
        });
    }

    const selectedProviders = new Set(providers.filter((provider) => provider.selected).map((provider) => provider.name));
    const selectedRoutes = new Set(routes.filter((route) => route.selected).map((route) => route.index));
    if (selectedRoutes.has(1) && !selectedProviders.has('new-provider') && !state.config.providers['new-provider']) {
        blockers.push({ code: 'missing_provider_reference', message: '所选路由依赖的提供商未导入且目标不存在', provider: 'new-provider', routeIndex: 1 });
    }
    const providerChanges = [];
    const routeChanges = [];

    if (selectedProviders.has('example-provider')) {
        providerChanges.push({
            name: 'example-provider',
            action: providerConflictAction,
            changedFields: ['maxConcurrent', 'apiKeys'],
            beforeKeyCount: 2,
            afterKeyCount: 2,
            affectedRoutes: [{ index: 0, match: 'example-*' }]
        });
    }
    if (selectedProviders.has('new-provider')) {
        providerChanges.push({
            name: 'new-provider',
            action: 'add',
            changedFields: ['baseUrl', 'apiKey', 'authType', 'accountId'],
            beforeKeyCount: 0,
            afterKeyCount: 1,
            affectedRoutes: [{ index: 1, match: 'new-*' }]
        });
    }
    if (selectedRoutes.has(0)) {
        routeChanges.push({
            sourceIndex: 0,
            targetIndex: 0,
            match: 'example-*',
            action: routeConflictAction,
            changedFields: ['targets'],
            providerDependencies: ['example-provider']
        });
    }
    if (selectedRoutes.has(1)) {
        routeChanges.push({
            sourceIndex: 1,
            targetIndex: null,
            match: 'new-*',
            action: 'add',
            changedFields: ['provider', 'model'],
            providerDependencies: ['new-provider']
        });
    }

    const summary = {
        providers: {
            add: selectedProviders.has('new-provider') ? 1 : 0,
            overwrite: selectedProviders.has('example-provider') && providerConflictAction === 'overwrite' ? 1 : 0,
            delete: mode === 'replace' && !selectedProviders.has('oauth-provider') ? 1 : 0,
            keep: selectedProviders.has('example-provider') && providerConflictAction === 'keep' ? 1 : 0,
            unchanged: 0
        },
        routes: {
            add: selectedRoutes.has(1) ? 1 : 0,
            overwrite: selectedRoutes.has(0) && routeConflictAction === 'overwrite' ? 1 : 0,
            delete: mode === 'replace' && !selectedRoutes.has(2) ? 1 : 0,
            keep: selectedRoutes.has(0) && routeConflictAction === 'keep' ? 1 : 0,
            unchanged: 0
        }
    };

    if (mode === 'replace') {
        summary.providers.keep = 0;
        summary.routes.keep = 0;
    }

    const warnings = [{
        code: 'oauth_accounts_external',
        message: 'OAuth 账户令牌不在迁移文件中，请先单独迁移账户再解决绑定',
        provider: 'new-provider'
    }];
    if (providerConflictAction === 'overwrite' || routeConflictAction === 'overwrite' || mode === 'replace') {
        warnings.push({
            code: 'manual_backup_recommended',
            message: '覆盖或整体替换前建议先手动导出当前配置作为备份'
        });
    }

    const existingOrder = [
        { match: 'example-*', source: routeConflictAction === 'overwrite' ? 'overwritten' : 'existing', index: 0 },
        { match: 'vision-*', source: 'existing', index: 1 }
    ];
    const addedOrder = selectedRoutes.has(1) ? [{ match: 'new-*', source: 'imported', index: 1 }] : [];
    const finalRouteOrder = (mode === 'replace'
        ? [{ match: 'example-*', source: 'imported', index: 0 }, ...addedOrder]
        : options.insertNewRoutes === 'before' ? [...addedOrder, ...existingOrder] : [...existingOrder, ...addedOrder])
        .map((item, position) => ({ ...item, position }));

    const canApply = blockers.length === 0;
    const previewToken = canApply
        ? 'tok-' + hash(JSON.stringify({ content, options })).slice(0, 20)
        : null;
    const data = {
        baseRevision: state.revision,
        sourceFormat: 'transfer-v1',
        ignoredGlobalFields: [],
        canReplace: hasProviders && hasRoutes,
        canApply,
        catalog: {
            providers,
            routes,
            sourceAccountIds: [sourceAccountId]
        },
        summary,
        changes: { providers: providerChanges, routes: routeChanges },
        finalRouteOrder,
        warnings,
        blockers,
        previewToken,
        expiresAt: previewToken ? new Date(Date.now() + 10 * 60 * 1000).toISOString() : null
    };
    state.lastPreview = {
        token: previewToken,
        content,
        options,
        baseRevision: state.revision
    };
    return { data };
}

function contentDisposition() {
    return 'attachment; filename="ai-gateway-provider-routes-20261102T120000Z.yaml"';
}

function exportYaml() {
    return `kind: ai-gateway.provider-route-transfer
version: 1
providers:
  example-provider:
    baseUrl: https://upstream.invalid/v1
    format: openai
    apiKey: ${FAKE_EXPORT_SECRET}
    proxy: http://user:${FAKE_PROXY_PASSWORD}@proxy.invalid:8080
    maxConcurrent: 5
routes:
  - match: example-*
    provider: example-provider
    model: example-model
`;
}

async function main() {
    const options = parseArgs(process.argv.slice(2));
    const productHtml = path.join(WEB_ROOT, 'index.html');
    ensure(fs.existsSync(productHtml), `找不到页面产物：${productHtml}`);

    // Chromium 会读取宿主代理环境变量；这里显式断开，避免 127.0.0.1 被代理成 502。
    for (const key of ['http_proxy', 'https_proxy', 'all_proxy', 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY']) {
        delete process.env[key];
    }

    const browser = await chromium.launch({ headless: true, args: ['--no-proxy-server'] });
    const context = await browser.newContext({
        acceptDownloads: true,
        viewport: { width: 1440, height: 2600 }
    });
    const page = await context.newPage();
    const state = makeState();
    const results = [];

    const step = (message) => {
        results.push('OK ' + message);
        console.log('  ✓ ' + message);
    };

    const fulfillJson = (route, body, status = 200, extraHeaders = {}) => route.fulfill({
        status,
        headers: {
            'Content-Type': 'application/json; charset=utf-8',
            'Cache-Control': 'no-store',
            ...extraHeaders
        },
        body: JSON.stringify(body)
    });

    const fulfillError = (route, status, type, message) => fulfillJson(route, {
        error: { type, message }
    }, status);

    const mimeTypes = {
        '.html': 'text/html; charset=utf-8',
        '.js': 'text/javascript; charset=utf-8',
        '.css': 'text/css; charset=utf-8',
        '.woff2': 'font/woff2',
        '.svg': 'image/svg+xml',
        '.ico': 'image/x-icon'
    };

    await page.route('**/*', async (route) => {
        const request = route.request();
        const requestUrl = new URL(request.url());
        state.requests.push(request.url());

        if ((requestUrl.protocol === 'http:' || requestUrl.protocol === 'https:')
            && requestUrl.origin !== ORIGIN) {
            state.externalRequests.push(request.url());
            await route.abort();
            return;
        }
        if (requestUrl.protocol === 'blob:' || requestUrl.protocol === 'data:') {
            await route.continue().catch(() => {});
            return;
        }
        if (requestUrl.origin !== ORIGIN) return route.abort();

        const pathname = decodeURIComponent(requestUrl.pathname);
        const method = request.method();

        if (method === 'GET' && (pathname === '/' || pathname === '/index.html')) {
            return route.fulfill({
                status: 200,
                headers: { 'Content-Type': mimeTypes['.html'] },
                body: fs.readFileSync(productHtml)
            });
        }
        if (method === 'GET' && pathname.startsWith('/vendor/')) {
            const filePath = path.join(WEB_ROOT, pathname);
            if (!filePath.startsWith(WEB_ROOT + path.sep) || !fs.existsSync(filePath)) {
                return route.fulfill({ status: 404, body: 'not found' });
            }
            return route.fulfill({
                status: 200,
                headers: { 'Content-Type': mimeTypes[path.extname(filePath)] || 'application/octet-stream' },
                body: fs.readFileSync(filePath)
            });
        }

        if (method === 'GET' && pathname === '/api/config') {
            state.getConfigCount += 1;
            return fulfillJson(route, {
                ...state.config,
                revision: state.revision,
                restartRequired: []
            }, 200, { ETag: `"${state.revision}"` });
        }
        if (method === 'GET' && pathname === '/api/config/raw') {
            return route.fulfill({
                status: 200,
                headers: { 'Content-Type': 'application/yaml; charset=utf-8' },
                body: 'host: 127.0.0.1\nport: 19999\n'
            });
        }
        if (method === 'GET' && pathname === '/api/chatgpt/accounts') {
            return fulfillJson(route, { data: state.accounts });
        }
        if (method === 'GET' && pathname === '/health') {
            return fulfillJson(route, {
                status: 'ok',
                listenAddress: '127.0.0.1:19999',
                providerHealth: {}
            });
        }
        if (method === 'GET' && pathname === '/api/metrics') {
            return fulfillJson(route, {});
        }
        if (method === 'GET' && pathname === '/api/logs') {
            return fulfillJson(route, { logs: [], total: 0 });
        }
        if (method === 'PUT' && pathname === '/api/config') {
            state.putCount += 1;
            return fulfillJson(route, { status: 'success', revision: state.revision });
        }
        if (method === 'POST' && pathname === '/api/config/export') {
            const body = JSON.parse(request.postData() || '{}');
            state.exports.push({
                body,
                ifMatch: request.headers()['if-match'] || ''
            });
            return route.fulfill({
                status: 200,
                headers: {
                    'Content-Type': 'application/yaml; charset=utf-8',
                    'Content-Disposition': contentDisposition(),
                    'Cache-Control': 'no-store',
                    ETag: `"${state.revision}"`
                },
                body: exportYaml()
            });
        }
        if (method === 'POST' && pathname === '/api/config/import/preview') {
            const body = JSON.parse(request.postData() || '{}');
            state.previews.push({
                body,
                ifMatch: request.headers()['if-match'] || ''
            });
            return fulfillJson(route, buildPreview(state, body), 200, { ETag: `"${state.revision}"` });
        }
        if (method === 'POST' && pathname === '/api/config/import/apply') {
            const body = JSON.parse(request.postData() || '{}');
            state.applies.push({
                body,
                ifMatch: request.headers()['if-match'] || ''
            });
            const last = state.lastPreview;
            if (!body.previewToken) {
                return fulfillError(route, 400, 'invalid_preview_token', '缺少预览凭证');
            }
            if (!last || body.previewToken !== last.token) {
                return fulfillError(route, 409, 'preview_changed', '预览凭证与当前内容或选项不符');
            }
            if (last.baseRevision !== state.revision) {
                return fulfillError(route, 409, 'config_conflict', '配置版本已变化，请重新加载并预览');
            }
            if (body.content !== last.content
                || JSON.stringify(body.options || {}) !== JSON.stringify(last.options || {})) {
                return fulfillError(route, 409, 'preview_changed', '预览凭证与当前内容或选项不符');
            }
            state.applyCount += 1;
            state.revision = `rev-${state.applyCount + 1}`;
            state.lastPreview = null;
            return fulfillJson(route, {
                status: 'success',
                message: '配置已导入、保存并热重载',
                revision: state.revision,
                restartRequired: [],
                summary: {
                    providers: { add: 1, overwrite: 0, delete: 0, keep: 1, unchanged: 0 },
                    routes: { add: 1, overwrite: 0, delete: 0, keep: 1, unchanged: 0 }
                }
            }, 200, { ETag: `"${state.revision}"` });
        }
        if (pathname.startsWith('/api/')) {
            return fulfillError(route, 404, 'not_found', '测试 mock 未实现该接口');
        }
        return route.fulfill({ status: 404, body: 'not found' });
    });

    const appEval = (fn, arg) => page.evaluate(fn, arg);
    const dialog = page.locator('[data-testid="config-transfer-dialog"]');
    const importTab = page.locator('[data-testid="config-transfer-tab-import"]');
    const previewButton = page.locator('[data-testid="config-transfer-preview"]');
    const applyButton = page.locator('[data-testid="config-transfer-apply"]');
    const contentInput = page.locator('[data-testid="config-transfer-import-content"]');
    const previewPanel = page.locator('[data-testid="config-transfer-preview-panel"]');

    const waitForToken = async (nonEmpty = true) => {
        await page.waitForFunction((want) => {
            const app = window.Alpine?.$data(document.body);
            if (!app) return false;
            return want ? !!app.configTransferImportToken : !app.configTransferImportToken;
        }, nonEmpty, { timeout: 8000 });
    };

    const readApp = () => appEval(() => {
        const app = window.Alpine.$data(document.body);
        return {
            show: app.showConfigTransferDialog,
            tab: app.configTransferTab,
            revision: app.revision,
            scope: app.configTransferExportScope,
            exportProviders: [...app.configTransferExportProviders],
            exportRoutes: [...app.configTransferExportRoutes],
            importContent: app.configTransferImportContent,
            importToken: app.configTransferImportToken,
            importPreview: app.configTransferImportPreview,
            importCatalog: app.configTransferImportCatalog,
            importMode: app.configTransferImportMode,
            importInsert: app.configTransferImportInsert,
            importSelection: {
                providers: [...app.configTransferImportSelection.providers],
                routes: [...app.configTransferImportSelection.routes]
            },
            importMappings: { ...app.configTransferImportAccountMappings },
            importFile: app.configTransferImportFile,
            objectURL: app._configTransferObjectURL,
            canApply: app.configTransferCanApply(),
            draftDirty: app.configTransferDraftDirty(),
            isDirty: app.isDirty(),
            toasts: app.statusToasts.map((toast) => toast.message)
        };
    });

    try {
        console.log(`打开假 origin：${ORIGIN}`);
        console.log(`页面产物：${productHtml}`);
        await page.goto(ORIGIN + '/', { waitUntil: 'domcontentloaded' });
        await page.waitForFunction(() => {
            const app = window.Alpine?.$data(document.body);
            return !!(app && app.revision);
        }, null, { timeout: 15000 });
        step('页面与 Alpine 应用已启动，配置来自 mock');

        const providerEntry = page.locator('[data-testid="provider-transfer-entry"]');
        const routeEntry = page.locator('[data-testid="route-transfer-entry"]');
        const providerEntryCount = await providerEntry.count();
        const routeEntryCount = await routeEntry.count();

        if (options.baseline) {
            if (providerEntryCount > 0 && routeEntryCount > 0) {
                console.log('基线检查：产物已包含两个入口，可运行默认模式做完整验证。');
            } else {
                console.log(`基线检查：产物缺少入口（provider=${providerEntryCount}, route=${routeEntryCount}）。`);
                console.log('这是重建产物前的预期结果；主会话完成 web 重建后再跑默认模式。');
            }
            return;
        }

        ensure(providerEntryCount === 1, '提供商页应恰好有一个导入/导出入口');
        ensure(routeEntryCount === 1, '路由页应恰好有一个导入/导出入口');
        step('提供商页与路由页各有一个共用入口');

        // 入口可见性与共用弹窗
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('providers'));
        await providerEntry.waitFor({ state: 'visible' });
        await providerEntry.click();
        await dialog.waitFor({ state: 'visible' });
        ensure((await readApp()).show === true, '提供商入口应打开共用弹窗');
        await page.locator('[data-testid="config-transfer-close"]').click();
        await dialog.waitFor({ state: 'hidden' });

        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('routes'));
        await routeEntry.waitFor({ state: 'visible' });
        await routeEntry.click();
        await dialog.waitFor({ state: 'visible' });
        ensure((await readApp()).show === true, '路由入口应打开同一个共用弹窗');
        await page.locator('[data-testid="config-transfer-close"]').click();
        await dialog.waitFor({ state: 'hidden' });
        step('两个入口打开同一个弹窗，关闭后重新隐藏');

        // 草稿闸门：可视配置改动
        await page.evaluate(() => window.Alpine.$data(document.body).selectNav('providers'));
        await page.evaluate(() => {
            window.Alpine.$data(document.body).config.providers['example-provider'].maxConcurrent = 9;
        });
        let appState = await readApp();
        ensure(appState.isDirty === true, '修改可视配置后应处于未保存状态');
        await providerEntry.click();
        await page.waitForTimeout(150);
        appState = await readApp();
        ensure(appState.show === false, '有未保存的可视配置修改时不应打开迁移弹窗');
        ensure(appState.toasts.some((message) => message.includes('未保存')), '应提示先保存或取消未保存修改');
        await page.evaluate(() => window.Alpine.$data(document.body).loadConfig());
        step('可视配置有未保存修改时入口被阻止');

        // 草稿闸门：隐藏的 YAML 草稿
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.yamlContent = app.originalYaml + '\n# hidden draft\n';
            app.editMode = 'visual';
        });
        appState = await readApp();
        ensure(appState.isDirty === false, '可视化模式下 isDirty 不应包含隐藏 YAML 草稿');
        ensure(appState.draftDirty === true, '迁移草稿判据应覆盖隐藏 YAML 草稿');
        await providerEntry.click();
        await page.waitForTimeout(150);
        ensure((await readApp()).show === false, '隐藏 YAML 草稿有改动时不应打开迁移弹窗');
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.yamlContent = app.originalYaml;
        });
        step('隐藏 YAML 草稿同样触发入口拦截');

        // 草稿闸门：已打开的编辑表单
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.addProvider();
            app.providerForm.name = 'draft-provider';
            app.openConfigTransferDialog('export');
        });
        await page.waitForTimeout(150);
        ensure((await readApp()).show === false, '编辑表单有未保存修改时迁移弹窗不应打开');
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.showProviderModal = false;
            app.restoreDrawerOpener();
        });
        await page.evaluate(() => window.Alpine.$data(document.body).loadConfig());
        step('已打开的 Provider 编辑表单改动同样被拦截');

        // 导出：默认全部、选择路由带依赖、风险确认、下载与清理
        await providerEntry.click();
        await dialog.waitFor({ state: 'visible' });
        appState = await readApp();
        ensure(appState.scope === 'all', '导出默认应为全部配置');
        await page.locator('[data-testid="config-transfer-export-scope-selected"]').check();
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.clearConfigTransferExportProviders();
            app.clearConfigTransferExportRoutes();
        });
        await page.locator('[data-testid="config-transfer-export-route"][data-route-index="0"]').check();
        const dependencyTag = page.locator('[data-testid="config-transfer-export-dependency"][data-dependency-provider="example-provider"]');
        await dependencyTag.waitFor({ state: 'visible' });
        ensure((await dependencyTag.innerText()).includes('自动包含'), '选择路由后应标出自动包含的依赖提供商');
        ensure(await page.evaluate(() => window.Alpine.$data(document.body)
            .configTransferExportProviderAutoAdded('example-provider')), '依赖提供商标记应来自路由依赖闭包');
        step('导出选择模式标出路由依赖提供商');

        const exportCountBefore = state.exports.length;
        const downloadPromise = page.waitForEvent('download');
        await page.locator('[data-testid="config-transfer-export-submit"]').click();
        await page.locator('[x-ref="confirmOk"]').waitFor({ state: 'visible' });
        ensure((await page.evaluate(() => window.Alpine.$data(document.body).confirmDialog.title)).includes('导出含凭据'),
            '每次导出都应先显示含凭据风险确认');
        await page.locator('[x-ref="confirmOk"]').click();
        const download = await downloadPromise;
        const downloadPath = await download.path();
        ensure(!!downloadPath, '应实际产生下载文件');
        const downloaded = fs.readFileSync(downloadPath, 'utf8');
        ensure(downloaded.includes('kind: ai-gateway.provider-route-transfer'), '下载文件应是版本化迁移包');
        ensure(downloaded.includes(FAKE_EXPORT_SECRET), '导出响应应包含真实凭据（本例为假密钥）');
        ensure(download.suggestedFilename().endsWith('.yaml'), '下载文件名应为 YAML 附件');
        ensure(state.exports.length === exportCountBefore + 1, '应发出一次导出请求');
        const exportRequest = state.exports[state.exports.length - 1];
        ensure(exportRequest.body.scope === 'selected', '选择导出应发送 scope=selected');
        ensure(JSON.stringify(exportRequest.body.selection.routes) === '[0]', '选择导出应发送路由原始下标');
        ensure(exportRequest.ifMatch === '"rev-1"', '导出请求必须携带精确 If-Match');
        await dialog.waitFor({ state: 'hidden' });
        appState = await readApp();
        ensure(appState.objectURL === '', '下载后应清理对象 URL');
        ensure(appState.importContent === '', '关闭后应清空导入原文');
        ensure(appState.importToken === '', '关闭后应清空预览凭证');
        ensure(!(await page.evaluate(() => document.body.innerText)).includes(FAKE_EXPORT_SECRET),
            '明文凭据只应出现在下载文件中，不应渲染到 DOM');
        step('导出附件字节、If-Match、风险确认与对象 URL 清理均符合预期');

        // 导入：默认 merge / 全部 / keep / after
        await providerEntry.click();
        await dialog.waitFor({ state: 'visible' });
        await importTab.click();
        // 上传与粘贴走同一条读取路径；先验证文件输入，再用文本域覆盖同一份内容。
        await page.locator('[data-testid="config-transfer-import-file"]').setInputFiles({
            name: 'transfer.yaml',
            mimeType: 'text/yaml',
            buffer: Buffer.from(IMPORT_YAML, 'utf8')
        });
        await page.waitForFunction((expected) => {
            const app = window.Alpine?.$data(document.body);
            return !!app && app.configTransferImportContent === expected
                && app.configTransferImportFileName === 'transfer.yaml';
        }, IMPORT_YAML, { timeout: 8000 });
        step('上传 YAML 文件可读取到弹窗临时状态');
        // 迟到的读取回调在关闭后不能恢复原文，也不能覆盖后来的粘贴。
        await page.evaluate(async () => {
            const app = window.Alpine.$data(document.body);
            let resolve;
            const pending = app.onConfigTransferFileChange({ target: { files: [{ name: 'slow.yaml', size: 3, arrayBuffer: () => new Promise((done) => { resolve = done; }) }] } });
            app.closeConfigTransferDialog();
            resolve(new TextEncoder().encode('secret').buffer);
            await pending;
            if (app.configTransferImportContent !== '' || app.configTransferImportFile !== null) throw new Error('迟到读取在关闭后恢复了敏感状态');
        });
        await providerEntry.click();
        await dialog.waitFor({ state: 'visible' });
        await importTab.click();
        step('关闭弹窗后迟到的文件读取不会恢复原文');
        await contentInput.fill(IMPORT_YAML);
        appState = await readApp();
        ensure(appState.importContent === IMPORT_YAML, '粘贴内容应进入弹窗临时状态');
        const previewCountBefore = state.previews.length;
        await previewButton.click();
        await waitForToken(true);
        await previewPanel.waitFor({ state: 'visible' });
        ensure(state.previews.length === previewCountBefore + 1, '点击预览应只发出一次预览请求');
        const firstPreview = state.previews[state.previews.length - 1];
        ensure(firstPreview.body.options.mode === 'merge', '首次预览默认合并模式');
        ensure(firstPreview.body.options.insertNewRoutes === 'after', '首次预览默认新增路由后插');
        ensure(firstPreview.body.options.selection === undefined, '首次预览不漏选时不应显式发送 selection');
        ensure(firstPreview.body.options.conflicts === undefined, '冲突默认保留时不发送 conflicts');
        ensure(firstPreview.ifMatch === '"rev-1"', '预览请求必须携带精确 If-Match');
        appState = await readApp();
        ensure(appState.importMode === 'merge' && appState.importInsert === 'after', '界面默认值应为合并 + 后插');
        ensure(appState.importSelection.providers.length === 2 && appState.importSelection.routes.length === 2,
            '首次预览后应默认选中目录中的全部提供商和路由');
        ensure(await page.locator('[data-testid="config-transfer-import-provider"]:checked').count() === 2,
            '提供商复选框应默认全选');
        ensure(await page.locator('[data-testid="config-transfer-import-route"]:checked').count() === 2,
            '路由复选框应默认全选');
        ensure(await page.locator('[data-testid="config-transfer-import-provider-conflict"][data-provider-name="example-provider"]'
            ).inputValue() === 'keep', '冲突默认应为保留目标');
        const previewText = await previewPanel.innerText();
        ensure(previewText.includes('最终路由顺序'), '预览应展示最终路由顺序');
        ensure(previewText.includes('提示'), '预览应展示 warnings');
        ensure(previewText.includes('变更明细'), '预览应展示 changes');
        ensure(previewText.includes('新增'), '变更明细应包含动作标签');
        ensure(previewText.includes('保留'), '预览摘要应包含 keep 计数');
        ensure(!previewText.includes(FAKE_IMPORT_SECRET), '预览面板不得包含导入原文里的密钥');
        ensure(!previewText.includes('https://upstream.invalid'), '预览不得展示配置原值');
        ensure(!(await page.evaluate(() => document.body.innerText)).includes(FAKE_IMPORT_SECRET),
            '页面可见文本不得包含导入密钥');
        step('导入首轮预览默认全部选中、冲突保留、后插，且预览不含密钥/原值');

        // 冲突改为覆盖：token 立即失效，不自动预览
        const countBeforeConflictChange = state.previews.length;
        await page.selectOption(
            '[data-testid="config-transfer-import-provider-conflict"][data-provider-name="example-provider"]',
            'overwrite'
        );
        await page.waitForTimeout(150);
        appState = await readApp();
        ensure(appState.importToken === '', '修改冲突动作后旧 token 必须立即作废');
        ensure(appState.importPreview === null, '修改冲突动作后不应继续沿用旧预览');
        ensure(state.previews.length === countBeforeConflictChange, '修改选项不得自动触发预览');
        await previewButton.click();
        await waitForToken(true);
        const overwritePreview = state.previews[state.previews.length - 1];
        ensure(overwritePreview.body.options.conflicts.providers['example-provider'] === 'overwrite',
            '重新预览应发送逐项覆盖选择');
        appState = await readApp();
        ensure(appState.importPreview.summary.providers.overwrite === 1, '覆盖后提供商摘要应计入 overwrite');
        step('冲突 keep → overwrite 使 token 作废，重新预览后生效');

        // 取消选择一个提供商：显式 selection 生效并保留路由依赖标记
        const countBeforeSelection = state.previews.length;
        await page.locator('[data-testid="config-transfer-import-provider"][data-provider-name="new-provider"]').uncheck();
        await page.waitForTimeout(150);
        ensure((await readApp()).importToken === '', '修改选择后旧 token 必须立即作废');
        ensure(state.previews.length === countBeforeSelection, '修改选择不得自动预览');
        await previewButton.click();
        await page.waitForFunction(() => !!window.Alpine.$data(document.body).configTransferImportPreview);
        ensure((await readApp()).canApply === false
            && (await readApp()).importPreview.blockers.some((item) => item.code === 'missing_provider_reference'),
            '取消必需提供商后应显示缺依赖，不得错误签发 token');
        const selectionPreview = state.previews[state.previews.length - 1];
        ensure(!!selectionPreview.body.options.selection, '漏选后应显式发送 selection');
        ensure(!selectionPreview.body.options.selection.providers.includes('new-provider'),
            'selection.providers 应反映取消选择');
        ensure(JSON.stringify(selectionPreview.body.options.selection.routes) === '[0,1]',
            'selection.routes 应发送未过滤的源路由下标（数字）');
        const dependencyBadge = page.locator('[data-testid="config-transfer-import-provider"][data-provider-name="new-provider"]');
        await dependencyBadge.waitFor({ state: 'visible' });
        const providerRowText = await dependencyBadge.evaluate((el) => el.closest('div.rounded-md')?.innerText || '');
        ensure(providerRowText.includes('路由依赖'),
            '未选中但被已选路由依赖的提供商仍应标出依赖');
        step('取消勾选后显式 selection 生效，缺依赖阻止应用且标记保留');
        await page.locator('[data-testid="config-transfer-import-provider"][data-provider-name="new-provider"]').check();
        await previewButton.click();
        await waitForToken(true);

        // 插入位置
        await page.locator('[data-testid="config-transfer-insert-before"]').check();
        await page.waitForTimeout(100);
        ensure((await readApp()).importToken === '', '修改插入位置后旧 token 必须作废');
        await previewButton.click();
        await waitForToken(true);
        ensure(state.previews[state.previews.length - 1].body.options.insertNewRoutes === 'before',
            '插入位置应发送 before');
        step('新增路由前后插选项使 token 作废并进入选项摘要');
        ensure((await readApp()).importPreview.finalRouteOrder[0].match === 'new-*', '前插预览的首项必须是新路由');
        const orderText = await page.locator('[data-testid="config-transfer-final-order"]').innerText();
        ensure(orderText.includes('1.') && orderText.indexOf('new-*') < orderText.indexOf('example-*'), '页面顺序编号和位置必须对应服务端数据');

        // 账户映射
        await page.selectOption(
            '[data-testid="config-transfer-import-account-mapping"][data-source-account-id="acc-source-1"]',
            'acc-target-2'
        );
        await page.waitForTimeout(100);
        ensure((await readApp()).importToken === '', '修改账户映射后旧 token 必须作废');
        await previewButton.click();
        await waitForToken(true);
        ensure(state.previews[state.previews.length - 1].body.options.accountMappings['acc-source-1'] === 'acc-target-2',
            '账户映射应只发送到目标已有账户 ID');
        step('OAuth 账户映射通过已有安全账户列表选择并进入选项');

        // 缺映射 blocker：非 canApply 仍保留可修正选项
        await contentInput.fill(IMPORT_YAML_NEEDS_MAPPING);
        appState = await readApp();
        ensure(appState.importToken === '' && appState.importCatalog === null,
            '更换原文后应清空旧预览、目录与 token');
        await previewButton.click();
        await page.waitForFunction(() => {
            const app = window.Alpine.$data(document.body);
            return !!app.configTransferImportPreview;
        }, null, { timeout: 8000 });
        appState = await readApp();
        ensure(appState.canApply === false, '缺少账户映射时不应可应用');
        ensure(appState.importPreview.blockers.some((item) => item.code === 'unresolved_account_mapping'),
            '应返回可修正的账户映射阻止项');
        const blockersPanel = page.locator('[data-testid="config-transfer-blockers"]');
        await blockersPanel.waitFor({ state: 'visible' });
        ensure((await blockersPanel.innerText()).includes('unresolved_account_mapping'), '阻止项面板应显示固定错误码');
        const modeRadio = page.locator('[data-testid="config-transfer-mode-merge"]');
        const modeRadioCount = await modeRadio.count();
        const modeRadioVisible = await modeRadio.isVisible().catch(() => false);
        ensure(modeRadioVisible,
            `非 canApply 时仍应保留可调整的模式选项（count=${modeRadioCount}, catalog=${!!appState.importCatalog}, mode=${appState.importMode}）`);
        ensure(await applyButton.isDisabled(), '存在阻止项时应用按钮必须禁用');
        await page.selectOption(
            '[data-testid="config-transfer-import-account-mapping"][data-source-account-id="acc-source-9"]',
            'acc-target-2'
        );
        await previewButton.click();
        await waitForToken(true);
        ensure((await readApp()).canApply === true, '补齐账户映射后应可应用');
        step('可修正 blocker 保留选项，补齐映射后重新预览通过');

        // 整体替换完整性：只有提供商、没有路由的片段不允许替换
        await contentInput.fill(PROVIDERS_ONLY_YAML);
        await previewButton.click();
        await page.waitForFunction(() => {
            const app = window.Alpine.$data(document.body);
            return !!app.configTransferImportPreview;
        }, null, { timeout: 8000 });
        appState = await readApp();
        ensure(appState.importPreview.canReplace === false, '缺少路由的片段应标记 canReplace=false');
        ensure(await page.locator('[data-testid="config-transfer-mode-replace"]').isDisabled(),
            '不完整片段必须禁用整体替换选项');
        step('仅提供商片段被判定为不满足整体替换完整性');

        // 恢复一份可应用预览，供凭证过期断言使用
        await contentInput.fill(IMPORT_YAML);
        await previewButton.click();
        await waitForToken(true);

        // 凭证过期：本地时钟驱动禁用
        await page.evaluate(() => {
            const app = window.Alpine.$data(document.body);
            app.configTransferImportExpiresAt = new Date(Date.now() - 1000).toISOString();
            app.configTransferNow = Date.now();
        });
        await page.waitForTimeout(50);
        ensure(await page.evaluate(() => window.Alpine.$data(document.body).configTransferPreviewExpired()),
            '过期时间应被识别');
        ensure(await applyButton.isDisabled(), '凭证过期后应用按钮必须禁用');
        step('预览凭证过期后禁止沿用应用按钮');

        // 关闭清理
        await page.keyboard.press('Escape');
        await dialog.waitFor({ state: 'hidden' });
        appState = await readApp();
        ensure(appState.importContent === '', '关闭后原文必须清空');
        ensure(appState.importToken === '', '关闭后 token 必须清空');
        ensure(appState.importPreview === null && appState.importCatalog === null,
            '关闭后预览与目录引用必须清空');
        ensure(appState.importFile === null, '关闭后文件引用必须清空');
        ensure(appState.importSelection.providers.length === 0 && appState.importSelection.routes.length === 0,
            '关闭后选择状态必须清空');
        ensure(Object.keys(appState.importMappings).length === 0, '关闭后账户映射必须清空');
        ensure(appState.objectURL === '', '关闭后不得残留对象 URL');
        ensure(await page.locator('[data-testid="config-transfer-import-file"]').evaluate((el) => el.value) === '',
            '关闭后 file input 的 value 必须清空');
        step('Esc 关闭后原文、文件、预览、token、选择、映射与对象 URL 全部清空');

        // 整体替换：独立二次确认、取消不写入、确认后一次性应用
        await providerEntry.click();
        await dialog.waitFor({ state: 'visible' });
        await importTab.click();
        await contentInput.fill(IMPORT_YAML);
        await previewButton.click();
        await waitForToken(true);
        const applyCountBeforeReplace = state.applies.length;
        await page.locator('[data-testid="config-transfer-mode-replace"]').check();
        await page.waitForTimeout(100);
        ensure((await readApp()).importToken === '', '切换整体替换后旧 token 必须作废');
        ensure((await readApp()).importMode === 'replace', '整体替换应切换导入模式');
        await previewButton.click();
        await waitForToken(true);
        const replacePreviewRequest = state.previews[state.previews.length - 1];
        ensure(replacePreviewRequest.body.options.mode === 'replace', '整体替换应发送 mode=replace');
        ensure(replacePreviewRequest.body.options.selection === undefined, '整体替换不得发送 selection');
        ensure(replacePreviewRequest.body.options.insertNewRoutes === undefined, '整体替换不得发送插入位置');
        ensure(replacePreviewRequest.body.options.conflicts === undefined, '整体替换不得发送逐项冲突');
        appState = await readApp();
        ensure(appState.importPreview.summary.routes.delete >= 1, '整体替换摘要应展示删除范围');
        ensure(!(await page.locator('[data-testid="config-transfer-import-provider"]').first().isVisible()),
            '整体替换应隐藏局部选择列表');

        const getConfigBeforeApply = state.getConfigCount;
        await applyButton.click();
        await page.locator('[x-ref="confirmOk"]').waitFor({ state: 'visible' });
        ensure((await page.evaluate(() => window.Alpine.$data(document.body).confirmDialog.title)) === '确认整体替换',
            '整体替换必须有独立的破坏性确认');
        ensure((await page.evaluate(() => window.Alpine.$data(document.body).confirmDialog.message)).includes('删除'),
            '整体替换确认应说明删除范围');
        await page.locator('div[x-show="confirmDialog.open"]').getByRole('button', { name: '取消' }).click();
        await page.waitForTimeout(100);
        ensure(state.applies.length === applyCountBeforeReplace, '取消整体替换确认不得发出应用请求');
        ensure((await readApp()).show === true, '取消确认后迁移弹窗应保留，便于继续调整');

        await applyButton.click();
        await page.locator('[x-ref="confirmOk"]').waitFor({ state: 'visible' });
        await page.locator('[x-ref="confirmOk"]').click();
        await page.waitForFunction((before) => {
            const app = window.Alpine?.$data(document.body);
            return !!app && !app.showConfigTransferDialog && !app.configTransferApplying
                && app.statusToasts.some((toast) => toast.message.includes('已导入'));
        }, null, { timeout: 10000 });
        ensure(state.applies.length === applyCountBeforeReplace + 1, '确认后应只发出一次应用请求');
        const applyRequest = state.applies[state.applies.length - 1];
        ensure(applyRequest.body.options.mode === 'replace', '应用请求应复用 replace 选项');
        ensure(applyRequest.body.content === IMPORT_YAML, '应用请求应重发与预览完全相同的原文');
        ensure(!!applyRequest.body.previewToken, '应用请求必须携带预览凭证');
        ensure(applyRequest.ifMatch === '"rev-1"', '应用请求必须携带预览时的精确 If-Match');
        ensure(state.getConfigCount > getConfigBeforeApply, '应用成功后应重新读取脱敏配置');
        ensure(state.revision !== 'rev-1', '应用成功后 revision 应更新');
        appState = await readApp();
        ensure(appState.importContent === '' && appState.importToken === '',
            '应用成功后应清空当次敏感状态');
        ensure(state.putCount === 0, '迁移流程不得调用全量 PUT /api/config');
        step('整体替换二次确认、取消不写入、确认后一次性应用并刷新配置');

        // 无上游流量与无持久化敏感内容
        ensure(state.externalRequests.length === 0,
            '不得访问任何外部 origin：' + state.externalRequests.join(', '));
        ensure(!state.requests.some((url) => url.includes('upstream.invalid')),
            '不得请求导入内容中的上游地址');
        ensure(state.putCount === 0, '全程不得调用普通 PUT /api/config');
        const storageDump = await page.evaluate(() => JSON.stringify({
            local: { ...localStorage },
            session: { ...sessionStorage }
        }));
        ensure(!storageDump.includes(FAKE_IMPORT_SECRET) && !storageDump.includes(FAKE_EXPORT_SECRET),
            'localStorage/sessionStorage 不得保存迁移凭据');
        step('全程无外部上游请求、无普通 PUT、无浏览器持久化敏感内容');

        console.log('\n全部交互断言通过。');
        console.log(`覆盖：${results.length} 组检查；mock 预览 ${state.previews.length} 次、导出 ${state.exports.length} 次、应用 ${state.applies.length} 次。`);
    } finally {
        await context.close();
        await browser.close();
    }
}

main().catch((err) => {
    console.error('\n验证失败：' + (err && err.stack || err));
    process.exit(1);
});