// Records what the real web UI reads from a seeded server (seed.mjs) into mock/web/data.js: signs in as the demo person,
// opens every screen and issue, and keeps each GET answer. Run from the repository root:
//   node scripts/mockweb/record.mjs            (server at TASKLANE_MOCK_URL, default http://127.0.0.1:18282)
// Playwright comes from e2e/node_modules (cd e2e && npm ci); it uses the system Chrome, as the e2e suite does.
import { createRequire } from 'node:module';
import { writeFileSync } from 'node:fs';
const { chromium } = createRequire(import.meta.url)('../../e2e/node_modules/playwright');
const B=process.env.TASKLANE_MOCK_URL||'http://127.0.0.1:18282';
const out={};
const browser=await chromium.launch({channel:'chrome',headless:true});
const ctx=await browser.newContext({viewport:{width:1440,height:900}});
const page=await ctx.newPage();
page.on('response',async r=>{const u=new URL(r.url());if(!u.pathname.startsWith('/api/'))return;const m=r.request().method();if(m!=='GET')return;
  let body=null;try{body=await r.json();}catch(e){}
  const k=m+' '+u.pathname+u.search; if(!out[k]||r.status()===200) out[k]={status:r.status(),body};});
await page.goto(B+'/');
await page.fill('#email','maya@example.com');await page.fill('#pw','password123');await page.keyboard.press('Enter');
await page.waitForTimeout(1500);
const boards=await (await page.request.get(B+'/api/v1/projects/SHOP/boards',{headers:{'X-Requested-With':'tasklane'}})).json();
const ob=await (await page.request.get(B+'/api/v1/projects/OPS/boards',{headers:{'X-Requested-With':'tasklane'}})).json();
const bid=boards[0].id, oid=ob[0].id;
const routes=['#/','#/?q=project%20%3D%20SHOP&syntax=tql','#/?q=project%20%3D%20OPS&syntax=tql','#/?q=checkout&syntax=text','#/inbox/unread','#/inbox','#/inbox/archived','#/boards/SHOP','#/boards/OPS',`#/board/${bid}`,`#/backlog/${bid}`,`#/reports/${bid}`,`#/reports/${bid}/burndown`,`#/reports/${bid}/velocity`,`#/board/${oid}`,
 '#/dashboards','#/releases/SHOP','#/trash/SHOP','#/approvals','#/import/SHOP','#/account',
 '#/settings','#/settings/appearance','#/settings/list','#/settings/keyboard','#/settings/security','#/settings/sessions','#/settings/api-tokens','#/settings/administration','#/settings/notifications',
 '#/admin/roles','#/admin/security','#/admin/git','#/admin/jira','#/admin/email','#/admin/workflows','#/admin/audit','#/admin/identity','#/admin/automation','#/admin/automation/SHOP'];
for(let i=1;i<=22;i++) routes.push('#/issue/SHOP-'+i);
for(let i=1;i<=3;i++) routes.push('#/issue/OPS-'+i);
for(const r of routes){await page.goto(B+'/'+r);await page.waitForTimeout(r.startsWith('#/issue')?1800:1200);}
// workflows editor
const wf=Object.keys(out).find(k=>k==='GET /api/v1/workflows');
if(wf&&Array.isArray(out[wf].body)) for(const w of out[wf].body){await page.goto(B+'/#/admin/workflows/'+w.id);await page.waitForTimeout(1000);}
const dl=out['GET /api/v1/dashboards']; if(dl&&dl.body) for(const d of (dl.body.dashboards||dl.body||[])){ if(d.id){await page.goto(B+'/#/dashboards/'+d.id);await page.waitForTimeout(1500);} }
// the per-issue history/versions panes
const extra=['/api/v1/me','/api/v1/me/notification-prefs','/api/v1/fields','/api/v1/me/settings'];for(const P of ['SHOP','OPS']){for(const t of ['Epic','Story','Task','Bug','Subtask']){extra.push(`/api/v1/projects/${P}/form?op=create&type=${t}`,`/api/v1/projects/${P}/form?op=edit&type=${t}`);}extra.push(`/api/v1/projects/${P}/canned-responses`);}
const ks=[];for(let i=1;i<=22;i++)ks.push('SHOP-'+i);for(let i=1;i<=3;i++)ks.push('OPS-'+i);
for(const k of ks) extra.push(`/api/v1/issues/${k}/history`,`/api/v1/issues/${k}/text/description/versions`,`/api/v1/issues/${k}/drafts`);
for(const p of extra){const r=await page.request.get(B+p,{headers:{'X-Requested-With':'tasklane'}});out['GET '+p]={status:r.status(),body:await r.json().catch(()=>null)};}
// searches, the list and the board are computed by the mock from the issues, so their recorded answers are not kept
for(const k of Object.keys(out)) if(k.startsWith('GET /api/v1/issues?')) delete out[k];
// what the recording machine would otherwise show: its address, the local server URL and a headless browser's name
const clean=JSON.parse(JSON.stringify(out).split(B).join('https://tasklane.example.com').replace(/127\.0\.0\.1/g,'203.0.113.7').replace(/HeadlessChrome/g,'Chrome'));
const data={recorded_at:new Date().toISOString(),fixtures:clean};
writeFileSync(new URL('../../mock/web/data.js',import.meta.url),'// Recorded from a real Tasklane server by scripts/mockweb/record.mjs; do not edit by hand.\nwindow.TL_MOCK_DATA = '+JSON.stringify(data)+';\n');
console.log(Object.keys(out).length,'answers written to mock/web/data.js');
await browser.close();
