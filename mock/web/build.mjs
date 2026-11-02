// Assembles the web UI mock: the real app (internal/web/static) plus the pretend server (mock-api.js, data.js).
//   node mock/web/build.mjs <out-dir>        e.g. node mock/web/build.mjs /tmp/site/web, then serve /tmp/site
// The page is the app's own index.html with root paths made relative (Pages serves it under /tasklane/web/) and the mock's
// scripts loaded first, so a script added to the app is in the mock without anyone touching this folder.
import { readFileSync, writeFileSync, mkdirSync, readdirSync, copyFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const app = join(here, '..', '..', 'internal', 'web', 'static');
const out = process.argv[2];
if (!out) { console.error('usage: node mock/web/build.mjs <out-dir>'); process.exit(2); }
mkdirSync(out, { recursive: true });

for (const f of readdirSync(app)) if (/\.(js|css)$/.test(f)) copyFileSync(join(app, f), join(out, f));
for (const f of ['mock-api.js', 'data.js']) copyFileSync(join(here, f), join(out, f));

const NOTE = `<style>
#tl-demo{position:fixed;left:12px;bottom:12px;z-index:29;display:flex;gap:8px;align-items:center;max-width:calc(100vw - 24px);
  background:var(--overlay);color:var(--subtle);border:1px solid var(--border);border-radius:999px;padding:4px 6px 4px 12px;font-size:12px;line-height:18px;box-shadow:0 2px 8px rgba(0,0,0,.25)}
#tl-demo b{color:var(--text)}#tl-demo a{white-space:nowrap}
#tl-demo button{background:none;border:0;color:var(--subtle);cursor:pointer;font-size:14px;padding:0 6px;min-height:24px}
@media(max-width:600px){#tl-demo{bottom:auto;top:auto;left:8px;right:8px;bottom:8px;border-radius:var(--r)}}
</style>
<div id="tl-demo" role="note"><span><b>Demo</b> · the real Tasklane UI with sample data; changes stay in this tab until you reload.
<a href="https://github.com/larytet/tasklane" target="_blank" rel="noopener">Source</a></span>
<button type="button" aria-label="Hide the demo note" onclick="this.parentNode.remove()">×</button></div>`;

let html = readFileSync(join(app, 'index.html'), 'utf8')
  .replace(/(src|href)="\/([^"/][^"]*)"/g, '$1="$2"')
  .replace('<title>Tasklane</title>', '<title>Tasklane · web UI demo</title>\n<meta name="robots" content="noindex">')
  .replace('<script src="lib.js"></script>', '<script src="data.js"></script>\n<script src="mock-api.js"></script>\n<script src="lib.js"></script>')
  .replace('</body>', NOTE + '\n</body>');
if (!html.includes('mock-api.js')) { console.error('index.html no longer loads lib.js first: update build.mjs'); process.exit(1); }
writeFileSync(join(out, 'index.html'), html);
console.log('web mock written to ' + out);
