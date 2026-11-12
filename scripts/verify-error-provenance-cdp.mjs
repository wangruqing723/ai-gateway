#!/usr/bin/env node
// Node 24 内置 WebSocket + 本机 Chrome CDP 验收；不安装第三方依赖。
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdirSync, readFileSync, readdirSync, writeFileSync, existsSync } from 'node:fs';
import { resolve, join, extname } from 'node:path';
import { tmpdir } from 'node:os';

const root = resolve(import.meta.dirname, '../cmd/gateway/web');
const fixtureDir = join(tmpdir(), `ai-gateway-error-provenance-${process.pid}`); mkdirSync(fixtureDir, { recursive: true });
const tpl = readFileSync(join(root, 'src/index.template.html'), 'utf8');
const parts = readdirSync(join(root, 'src/app')).filter(n => n.endsWith('.js.part')).sort().map(n => readFileSync(join(root, 'src/app', n), 'utf8').replace(/\n$/, '')).join('\n');
const html = tpl.replace('        //<<<GW_APP_MEMBERS>>>', () => parts);
writeFileSync(join(fixtureDir, 'index.html'), html);
const attack = '<img src=x onerror="window.__errorHTMLExecuted=true">';
const attempt = extra => ({ sequence: 1, attemptNumber: 1, provider: 'fixture', targetModel: 'model', providerFormat: 'openai-responses', upstreamStatus: 200, durationMs: 12, outcome: 'failure', error: '合成失败', ...extra });
const log = (id, extra) => ({ id, time: '2026-11-12 12:00:00', model: 'model', provider: 'fixture', status: 200, stream: true, durationMs: 12, attempts: 1, ...extra });
const logs = [
  log('r00035', { error: '上游错误', errorSource: 'upstream', errorCategory: 'upstream_stream_error', attemptDetails: [attempt({ errorSource: 'upstream', errorCategory: 'upstream_stream_error', upstreamErrorType: 'error', upstreamErrorCode: 'upstream_http2_stream_error', upstreamErrorMessage: 'Upstream HTTP/2 stream failed', upstreamRequestId: 'fixture-request', responseStarted: true, errorBodyState: 'available', errorBody: attack })] }),
  log('success-after-failure', { attempts: 2, attemptDetails: [attempt({ upstreamStatus: 503, errorSource: 'upstream', errorBodyState: 'not_provided' }), attempt({ sequence: 2, outcome: 'success', error: '' })] }),
  log('old-record', { status: 502, error: 'conversion_error', attemptDetails: [attempt({ error: 'conversion_error' })] }),
  ...['gateway', 'transport', 'client'].map(source => log(source, { status: 502, error: '合成失败', errorSource: source, attemptDetails: [attempt({ errorSource: source, errorBodyState: 'not_captured', responseStarted: false })] })),
  log('truncated', { status: 500, error: '合成失败', errorSource: 'upstream', attemptDetails: [attempt({ errorSource: 'upstream', errorCategory: 'upstream_stream_error', outcome: 'final_error', upstreamStatus: 200, errorBodyState: 'truncated', errorBodyTruncated: true, errorBody: '错误片段 [REDACTED]' })] }),
  log('cancelled', { status: 0, error: '客户端取消', errorSource: 'client', attemptDetails: [attempt({ errorSource: 'client', errorCategory: 'cancelled', outcome: 'cancelled', upstreamStatus: 0, errorBodyState: 'not_captured', responseStarted: false })] }),
  log('build-skip', { status: 0, error: '构建失败', errorSource: 'gateway', attemptDetails: [attempt({ errorSource: 'gateway', errorCategory: 'build_skip', outcome: 'build_skip', upstreamStatus: 0, errorBodyState: 'not_captured', responseStarted: false })] })
];
const server = createServer((req, res) => { const url = new URL(req.url, 'http://127.0.0.1'); if (url.pathname === '/api/logs') return send({ data: logs }); if (url.pathname === '/api/config') return send({ revision:'fixture', providers:{}, routes:[], failover:{}, breaker:{}, providerHealth:{}, cache:{} }); if (url.pathname === '/health') return send({ status:'ok', queues:{}, providerHealth:{}, providerKeys:{} }); if (url.pathname.startsWith('/api/')) return send({ data:[], events:[], summary:{}, providers:{}, vision:{} }); const file = url.pathname === '/' ? join(fixtureDir,'index.html') : join(root, url.pathname); if (!file.startsWith(root) && !file.startsWith(fixtureDir)) return end(404); if (!existsSync(file)) return end(404); res.writeHead(200, {'content-type': extname(file)==='.html'?'text/html':extname(file)==='.js'?'application/javascript':extname(file)==='.css'?'text/css':'application/octet-stream'}); res.end(readFileSync(file)); function send(body){res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify(body));} function end(code){res.writeHead(code);res.end();} });
await new Promise(r => server.listen(0, '127.0.0.1', r)); const port = server.address().port;
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', ['--headless=new', `--remote-debugging-port=${19000 + process.pid % 500}`, `--user-data-dir=${fixtureDir}/profile`, '--no-first-run', '--no-default-browser-check', '--window-size=1440,1000', 'about:blank'], {stdio:'ignore'});
const cdpPort = 19000 + process.pid % 500; let target; for (let i=0;i<60&&!target;i++){await new Promise(r=>setTimeout(r,150)); try {target=(await (await fetch(`http://127.0.0.1:${cdpPort}/json/list`)).json()).find(t=>t.type==='page');} catch {}}
if (!target) { chrome.kill(); server.close(); throw new Error('Chrome 未启动'); }
const ws = new WebSocket(target.webSocketDebuggerUrl); await new Promise((r,j)=>{ws.addEventListener('open',r);ws.addEventListener('error',j)}); let seq=0; const pending=new Map(); const problems=[]; ws.addEventListener('message',e=>{const m=JSON.parse(e.data);if(m.id&&pending.has(m.id)){pending.get(m.id)(m);pending.delete(m.id)} if(m.method==='Runtime.exceptionThrown') problems.push(m.params.exceptionDetails.text)}); const send=(method,params={})=>new Promise(r=>{const id=++seq;pending.set(id,r);ws.send(JSON.stringify({id,method,params}))}); const evaluate=async expression=>{const r=await send('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});if(r.result?.exceptionDetails)throw Error(r.result.exceptionDetails.text);return r.result?.result?.value}; const shot=async name=>{const r=await send('Page.captureScreenshot',{format:'png'});writeFileSync(join(fixtureDir,`${name}.png`),Buffer.from(r.result.data,'base64'))};
const results = [];
const check = (name, ok) => { results.push(`${ok ? 'PASS' : 'FAIL'} ${name}`); if (!ok) throw Error(name); };
try {
    await send('Runtime.enable'); await send('Page.enable');
    for (const [width, height, name] of [[1440, 1000, 'desktop'], [390, 844, 'mobile']]) {
        await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
        await send('Page.navigate', { url: `http://127.0.0.1:${port}/#logs` });
        for (let i = 0; i < 40; i++) {
            if (await evaluate(`Boolean(Array.from(document.querySelectorAll('tr[role="button"]')).find(x => x.innerText.includes('r00035')))`)) break;
            await new Promise(r => setTimeout(r, 100));
        }
        const rowExpr = id => `Array.from(document.querySelectorAll('tr[role="button"]')).find(x => x.innerText.includes(${JSON.stringify(id)}))`;
        const rowText = id => evaluate(`${rowExpr(id)}?.innerText || ''`);
        const open = async id => { await evaluate(`${rowExpr(id)}?.click()`); await new Promise(r => setTimeout(r, 100)); };
        const detailText = id => evaluate(`${rowExpr(id)}?.nextElementSibling?.innerText || ''`);
        check(`${name} 日志加载`, (await rowText('r00035')).includes('r00035'));
        check(`${name} HTTP200失败`, (await rowText('r00035')).includes('流中断'));
        check(`${name} 成功不标失败`, !/失败|上游返回错误/.test(await rowText('success-after-failure')));
        for (const [id, label] of [['r00035', '上游返回错误'], ['gateway', '网关处理错误'], ['transport', '连接异常'], ['client', '客户端中断'], ['old-record', '来源未确定']]) check(`${name} ${label}`, (await rowText(id)).includes(label));
        await open('r00035');
        const detail = await detailText('r00035');
        check(`${name} 错误码与原消息`, detail.includes('upstream_http2_stream_error') && detail.includes('Upstream HTTP/2 stream failed'));
        check(`${name} 响应已开始说明`, detail.includes('响应已开始，未执行故障转移'));
        await evaluate(`${rowExpr('r00035')}.nextElementSibling.querySelector('summary').click()`);
        check(`${name} HTML纯文本`, await evaluate(`${rowExpr('r00035')}.nextElementSibling.querySelector('pre').textContent === ${JSON.stringify(attack)} && !document.querySelector('pre img') && !window.__errorHTMLExecuted`));
        await shot(name);
        await open('success-after-failure');
        const successDetail = await detailText('success-after-failure');
        check(`${name} 成功仍可看失败尝试`, successDetail.includes('上游返回错误') && successDetail.includes('上游未提供错误正文'));
        for (const [id, label] of [['old-record', '原文状态未记录'], ['gateway', '未采集错误正文'], ['truncated', '已截断']]) { await open(id); check(`${name} ${label}`, (await detailText(id)).includes(label)); }
        check(`${name} final_error保留HTTP200`, (await detailText('truncated')).includes('200'));
        for (const [id, label] of [['cancelled', '客户端中断'], ['build-skip', '网关处理错误']]) {
            await open(id);
            check(`${name} ${id}无伪造HTTP状态`, await evaluate(`${rowExpr(id)}.nextElementSibling.querySelector('table tbody tr td:nth-child(4)').textContent.trim() === '--'`));
            check(`${name} ${id}来源`, (await detailText(id)).includes(label));
        }
        check(`${name} 无页面横向溢出`, await evaluate('document.documentElement.scrollWidth <= window.innerWidth'));
    }
    check('无页面 JS 异常', problems.length === 0);
} finally {
    console.log(results.join('\n'));
    console.log(`截图目录：${fixtureDir}`);
    ws.close(); chrome.kill(); server.close();
}
