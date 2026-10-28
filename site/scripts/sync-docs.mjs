// Builds the Starlight content tree from the repository so docs stay single-sourced:
//   ../docs/**/*.md          -> docs/**            (README.md becomes the section index)
//   ../sdk/*/README.md       -> docs/sdks/*
//   ../api/openapi.yaml      -> docs/reference/api
//   ../cmd/differently       -> docs/reference/cli (from the CLI's own --help, needs Go)
//   ./pages-src/**           -> hand-written pages (Go SDK, notes)
// Relative links are rewritten to site routes, or to GitHub for files outside the site.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const site = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repo = path.resolve(site, '..');
const out = path.join(site, 'src/content/docs/docs');
const BASE = '/differently';
const GITHUB = 'https://github.com/different-ai/differently';
const BRANCH = 'pre-release';

// Repository files that have their own page on the site.
const special = {
  'api/openapi.yaml': 'reference/api',
  'sdk/typescript/README.md': 'sdks/typescript',
  'sdk/python/README.md': 'sdks/python',
  'pkg/client/client.go': 'sdks/go',
  'pkg/client': 'sdks/go',
  'README.md': '',
};

function routeFor(repoPath) {
  repoPath = repoPath.replace(/\/$/, '');
  if (repoPath in special) return special[repoPath];
  if (repoPath === 'docs') return '';
  if (repoPath.startsWith('docs/')) {
    const rel = repoPath.slice(5);
    if (rel.endsWith('.md')) return rel.replace(/(^|\/)README\.md$/, '').replace(/\.md$/, '');
    if (fs.existsSync(path.join(repo, repoPath)) && fs.statSync(path.join(repo, repoPath)).isDirectory()) return rel;
  }
  return null;
}

function rewriteLinks(md, fromRepoPath) {
  const dir = path.posix.dirname(fromRepoPath);
  return md.replace(/(!?)\]\(([^)\s]+)((?:\s+"[^"]*")?)\)/g, (all, bang, target, title) => {
    if (/^(https?:|mailto:|#)/.test(target)) return all;
    const [p, hash = ''] = target.split('#');
    const resolved = path.posix.normalize(path.posix.join(dir, p));
    const route = bang ? null : routeFor(resolved);
    let url;
    if (route !== null) {
      url = `${BASE}/docs/${route ? route + '/' : ''}`;
    } else {
      const abs = path.join(repo, resolved);
      const kind = fs.existsSync(abs) && fs.statSync(abs).isDirectory() ? 'tree' : bang ? 'raw' : 'blob';
      url = `${GITHUB}/${kind}/${BRANCH}/${resolved}`;
    }
    return `${bang}](${url}${hash ? '#' + hash : ''}${title})`;
  });
}

const escapeHtml = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

// ```mermaid fences become <pre class="mermaid">, rendered client-side by public/mermaid-init.js.
const mermaid = (md) =>
  md.replace(/^```mermaid\n([\s\S]*?)^```\s*$/gm, (_, src) => `<pre class="mermaid">${escapeHtml(src.trimEnd())}</pre>\n`);

function page(fromRepoPath, md, extra = {}) {
  let title = extra.title;
  const h1 = md.match(/^# (.+)\n/m);
  if (h1 && md.indexOf(h1[0]) === md.search(/\S/)) {
    title ??= h1[1].trim();
    md = md.replace(h1[0], '');
  }
  title ??= path.basename(fromRepoPath, '.md');
  const fm = { title: title.replace(/`/g, ''), ...extra, ...(extra.title ? { title: extra.title } : {}) };
  const front = Object.entries(fm).map(([k, v]) => `${k}: ${typeof v === 'object' ? JSON.stringify(v) : JSON.stringify(v)}`).join('\n');
  const source = `\n\n---\n\n<small>Source: [\`${fromRepoPath}\`](${GITHUB}/blob/${BRANCH}/${fromRepoPath})</small>\n`;
  return `---\n${front}\n---\n${mermaid(rewriteLinks(md, fromRepoPath))}${fromRepoPath.endsWith('.md') ? source : ''}`;
}

function write(route, content) {
  const file = path.join(out, route ? `${route}.md` : 'index.md');
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, content);
}

function walk(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? walk(path.join(dir, e.name)) : [path.join(dir, e.name)],
  );
}

fs.rmSync(out, { recursive: true, force: true });

// 1. docs/
for (const abs of walk(path.join(repo, 'docs')).filter((f) => f.endsWith('.md'))) {
  const rel = path.relative(repo, abs).split(path.sep).join('/');
  let route = routeFor(rel);
  const extra = {};
  if (route === '' || route.endsWith('/') || rel.endsWith('README.md')) route = route ? `${route}/index` : '';
  // ADRs sort by number in the sidebar; section indexes go first.
  const adr = rel.match(/^docs\/adr\/(\d{4})-/);
  if (adr) extra.sidebar = { order: Number(adr[1]) };
  if (rel.endsWith('README.md')) extra.sidebar = { order: 0, label: 'Overview' };
  write(route, page(rel, fs.readFileSync(abs, 'utf8'), extra));
}

// 2. SDK READMEs
for (const [lang, title] of [['typescript', 'TypeScript SDK'], ['python', 'Python SDK']]) {
  const rel = `sdk/${lang}/README.md`;
  write(`sdks/${lang}`, page(rel, fs.readFileSync(path.join(repo, rel), 'utf8'), { title }));
}

// 3. Hand-written pages
const hand = path.join(site, 'pages-src');
for (const abs of walk(hand)) {
  const rel = path.relative(hand, abs).split(path.sep).join('/');
  fs.mkdirSync(path.dirname(path.join(out, rel)), { recursive: true });
  fs.copyFileSync(abs, path.join(out, rel));
}

// 4. API reference from OpenAPI
write('reference/api', apiPage(YAML.parse(fs.readFileSync(path.join(repo, 'api/openapi.yaml'), 'utf8'))));

// 5. CLI reference from the CLI's own help output
write('reference/cli', cliPage());

console.log(`synced docs into ${path.relative(site, out)}`);

function apiPage(spec) {
  const ref = (r) => r.split('/').pop();
  const typeOf = (s) => {
    if (!s) return '';
    if (s.$ref) return `[\`${ref(s.$ref)}\`](#${ref(s.$ref).toLowerCase()})`;
    if (s.type === 'array') return `${typeOf(s.items)}[]`;
    if (s.enum) return s.enum.map((v) => `\`${v}\``).join(' \\| ');
    if (s.oneOf || s.anyOf) return (s.oneOf || s.anyOf).map(typeOf).join(' \\| ');
    const t = Array.isArray(s.type) ? s.type.join(' \\| ') : s.type || 'object';
    return `\`${t}${s.format ? ` (${s.format})` : ''}\``;
  };
  const cell = (s = '') => String(s).replace(/\n+/g, ' ').replace(/[\\|]/g, '\\$&').trim();
  const resolveParam = (p) => (p.$ref ? spec.components.parameters[ref(p.$ref)] : p);
  const lines = [
    '---',
    'title: HTTP API reference',
    `description: ${JSON.stringify(spec.info.summary)}`,
    '---',
    '',
    `Generated from [\`api/openapi.yaml\`](${GITHUB}/blob/${BRANCH}/api/openapi.yaml) (OpenAPI ${spec.openapi}, API ${spec.info.version}). A running server also serves the contract at \`/openapi.yaml\`.`,
    '',
    spec.info.description.trim(),
    '',
  ];
  const methods = ['get', 'post', 'put', 'patch', 'delete'];
  for (const tag of spec.tags.map((t) => t.name)) {
    const ops = [];
    for (const [p, item] of Object.entries(spec.paths))
      for (const m of methods) if (item[m] && (item[m].tags || ['Other']).includes(tag)) ops.push([p, m, item[m], item]);
    if (!ops.length) continue;
    lines.push(`## ${tag}`, '');
    for (const [p, m, op, item] of ops) {
      lines.push(`### ${op.summary || `${m.toUpperCase()} ${p}`}`, '', `\`${m.toUpperCase()} ${p}\`${op.security?.length === 0 ? ' · no authentication' : ''}`, '');
      if (op.description) lines.push(op.description.trim(), '');
      const params = [...(item.parameters || []), ...(op.parameters || [])].map(resolveParam);
      if (params.length) {
        lines.push('| Parameter | In | Type | Description |', '|---|---|---|---|');
        for (const pr of params) lines.push(`| \`${pr.name}\`${pr.required ? ' *' : ''} | ${pr.in} | ${typeOf(pr.schema)} | ${cell(pr.description)} |`);
        lines.push('');
      }
      const body = op.requestBody?.content;
      if (body) for (const [ct, c] of Object.entries(body)) lines.push(`**Request body** (\`${ct}\`): ${typeOf(c.schema) || 'raw bytes'}`, '');
      const res = Object.entries(op.responses || {});
      if (res.length) {
        lines.push('| Status | Response |', '|---|---|');
        for (const [code, r0] of res) {
          const r = r0.$ref ? spec.components.responses[ref(r0.$ref)] : r0;
          const schema = r.content && Object.values(r.content)[0]?.schema;
          lines.push(`| ${code} | ${cell(r.description)}${schema ? ` — ${typeOf(schema)}` : ''} |`);
        }
        lines.push('');
      }
    }
  }
  lines.push('## Schemas', '');
  for (const [name, s] of Object.entries(spec.components.schemas || {})) {
    lines.push(`### ${name}`, '');
    if (s.description) lines.push(s.description.trim(), '');
    if (s.properties) {
      lines.push('| Field | Type | Description |', '|---|---|---|');
      for (const [f, fs_] of Object.entries(s.properties))
        lines.push(`| \`${f}\`${(s.required || []).includes(f) ? ' *' : ''} | ${typeOf(fs_)} | ${cell(fs_.description)} |`);
      lines.push('');
    } else lines.push(`Type: ${typeOf(s)}`, '');
  }
  lines.push('Fields and parameters marked `*` are required.', '');
  return lines.join('\n');
}

function cliPage() {
  const bin = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'differently-cli-')), 'differently');
  execFileSync('go', ['build', '-o', bin, './cmd/differently'], { cwd: repo, stdio: 'inherit' });
  const help = (args) => execFileSync(bin, [...args, '--help'], { encoding: 'utf8', env: { ...process.env, HOME: os.tmpdir() } });
  const sections = [];
  const visit = (args) => {
    const text = help(args);
    sections.push([args, text]);
    const block = text.split(/^Available Commands:\n/m)[1];
    if (!block) return;
    for (const line of block.split(/\n\n/)[0].split('\n')) {
      const name = line.trim().split(/\s+/)[0];
      if (name && !['help', 'completion'].includes(name)) visit([...args, name]);
    }
  };
  visit([]);
  const lines = [
    '---',
    'title: CLI reference',
    'description: Every differently command, generated from the CLI help output.',
    '---',
    '',
    `Generated from the help output of [\`cmd/differently\`](${GITHUB}/tree/${BRANCH}/cmd/differently). Build it with \`make build\`; the binary lands in \`bin/differently\`.`,
    '',
    'Configuration comes from flags, then `DIFFERENTLY_URL`, `DIFFERENTLY_API_KEY` and `DIFFERENTLY_PROJECT`, then the file written by `differently login`. Against a local `--dev` server the CLI picks up the operator token from `~/.differently/dev/token`.',
    '',
  ];
  for (const [args, text] of sections) {
    lines.push(`${'#'.repeat(Math.min(2 + Math.max(args.length - 1, 0), 4))} ${['differently', ...args].join(' ')}`, '', '```text', text.trimEnd(), '```', '');
  }
  return lines.join('\n');
}
