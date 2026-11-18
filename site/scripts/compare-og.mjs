// Renders the share image for /compare/ (public/media/sandbox-providers-compared.png, 1200x630).
//
//   node scripts/compare-og.mjs
//
// It reads the provider count and marks from src/data/providers.ts, writes a
// page and screenshots it with a Chrome you have installed (set CHROME to its
// path if it is not in the usual place). Run it again whenever a provider is
// added or removed, so the image and the page agree.
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const data = readFileSync(join(root, 'src/data/providers.ts'), 'utf8');
const list = data.slice(data.indexOf('export const providers'), data.indexOf('export const unavailable'));
const logos = [...list.matchAll(/logo: '([a-z0-9-]+\.png)'/g)].map((m) => m[1]);
const count = logos.length;
const label = data.match(/PRICES_READ_LABEL = '\d+ (\w+ \d{4})'/)?.[1] ?? '';
const marks = [...new Set(logos)];

const chrome = [process.env.CHROME, '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser'].find((p) => p && existsSync(p));
if (!chrome) throw new Error('No Chrome found. Set CHROME to the path of a Chrome or Chromium binary.');

const html = `<!doctype html><html><head><meta charset="utf-8">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap" rel="stylesheet">
<style>
  * { box-sizing: border-box; }
  html, body { margin: 0; width: 1200px; height: 630px; background: #ffffff; color: #0a0a0a; font-family: Inter, 'Helvetica Neue', Arial, sans-serif; -webkit-font-smoothing: antialiased; }
  .card { position: absolute; inset: 0; padding: 56px 64px; display: flex; flex-direction: column; }
  .brand { display: flex; align-items: center; gap: 14px; font-weight: 600; font-size: 30px; letter-spacing: -0.02em; }
  .brand svg { width: 38px; height: 38px; }
  h1 { margin: 54px 0 0; font-size: 92px; line-height: 1.02; font-weight: 600; letter-spacing: -0.045em; }
  h1 em { font-style: normal; color: #8a8a8a; }
  p { margin: 26px 0 0; font-size: 30px; color: #5c5c5c; letter-spacing: -0.01em; }
  .marks { margin-top: auto; display: flex; flex-wrap: wrap; gap: 12px; }
  .marks img { width: 52px; height: 52px; border-radius: 12px; border: 1px solid #e3e3e3; background: #ffffff; object-fit: contain; }
  .when { position: absolute; right: 64px; top: 62px; font-size: 24px; color: #8a8a8a; }
</style></head><body><div class="card">
  <div class="brand"><svg viewBox="0 0 32 32"><rect x="10" y="3" width="19" height="19" rx="4" fill="none" stroke="#0a0a0a" stroke-width="2.5"/><rect x="3" y="10" width="19" height="19" rx="4" fill="#0a0a0a"/></svg>differently</div>
  <div class="when">${label}</div>
  <h1>${count} sandbox providers, <em>compared.</em></h1>
  <p>Price, isolation, state, snapshots and compliance, with sources.</p>
  <div class="marks">${marks.map((m) => `<img src="${pathToFileURL(join(root, 'public/providers', m))}">`).join('')}</div>
</div></body></html>`;

const dir = mkdtempSync(join(tmpdir(), 'compare-og-'));
const page = join(dir, 'og.html');
const out = join(root, 'public/media/sandbox-providers-compared.png');
writeFileSync(page, html);
execFileSync(chrome, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--force-device-scale-factor=1', '--window-size=1200,630', '--virtual-time-budget=6000', `--screenshot=${out}`, pathToFileURL(page).href], { stdio: 'ignore' });
rmSync(dir, { recursive: true, force: true });
console.log(`wrote ${out}: ${count} providers, ${marks.length} marks`);
