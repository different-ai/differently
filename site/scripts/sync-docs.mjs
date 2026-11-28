// Builds the Starlight content tree from the repository so docs stay single-sourced:
//   ../docs/**/*.md          -> docs/**            (README.md becomes the section index)
//   ../sdk/*/README.md       -> docs/sdks/*
//   ../api/openapi.yaml      -> docs/reference/api
//   ../cmd/differently       -> docs/reference/cli (from the CLI's own --help, needs Go)
//   ./pages-src/**           -> hand-written pages (Go SDK, notes)
// Relative links are rewritten to site routes, or to GitHub for files outside the site.
//
// It also writes what programs read instead of people, into public/ (git-ignored):
//   docs/**/index.md         every docs page as Markdown, at the page's address plus index.md
//   llms.txt                 an index of those pages (llmstxt.org)
//   agents.txt               docs/guides/agents.md as plain text
//   install.sh               ../install.sh
//   compose.yaml             ../deploy/compose/compose.yaml
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const site = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repo = path.resolve(site, '..');
const out = path.join(site, 'src/content/docs/docs');
const pub = path.join(site, 'public');
const ORIGIN = 'https://different-ai.github.io';
const BASE = '/differently';
const GITHUB = 'https://github.com/different-ai/differently';
const BRANCH = 'pre-release';

// Repository files that have their own page on the site.
const special = {
  'api/openapi.yaml': 'reference/api',
  'sdk/typescript/README.md': 'sdks/typescript',
  'sdk/python/README.md': 'sdks/python',
  'console/README.md': 'guides/console',
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

// Literal text inside a RegExp: the dots of a host name must not match any character.
const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const SITE_RE = escapeRe(`${ORIGIN}${BASE}`);

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
  // Section indexes go first in the sidebar.
  if (rel.endsWith('README.md')) extra.sidebar = { order: 0, label: 'Overview' };
  write(route, page(rel, fs.readFileSync(abs, 'utf8'), extra));
}

// 2. SDK READMEs
for (const [lang, title] of [['typescript', 'TypeScript SDK'], ['python', 'Python SDK']]) {
  const rel = `sdk/${lang}/README.md`;
  write(`sdks/${lang}`, page(rel, fs.readFileSync(path.join(repo, rel), 'utf8'), { title }));
}
write('guides/console', page('console/README.md', fs.readFileSync(path.join(repo, 'console/README.md'), 'utf8'), { title: 'Admin console' }));

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

// 6. The same pages for programs: Markdown twins, llms.txt, agents.txt, install.sh, compose.yaml
const twins = markdownTwins();
fs.writeFileSync(path.join(pub, 'llms.txt'), llmsTxt(twins));
fs.writeFileSync(path.join(pub, 'agents.txt'), twins.get('guides/agents/').text);
fs.copyFileSync(path.join(repo, 'install.sh'), path.join(pub, 'install.sh'));
fs.copyFileSync(path.join(repo, 'deploy/compose/compose.yaml'), path.join(pub, 'compose.yaml'));

console.log(`synced docs into ${path.relative(site, out)}`);

// markdownTwins writes each generated page to public/docs/<route>/index.md
// with absolute links, so a program that fetched one page can follow its
// links and stay in Markdown. It returns the pages by route ('' is the docs
// home, 'guides/quickstart/' a page).
function markdownTwins() {
  const dir = path.join(pub, 'docs');
  fs.rmSync(dir, { recursive: true, force: true });
  const unescapeHtml = (t) => t.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
  // Notes to maintainers. Repeated until nothing is left, so removing one
  // comment cannot leave the start of another behind.
  const withoutComments = (t) => {
    for (let before; before !== t; ) {
      before = t;
      t = t.replace(/<!--[\s\S]*?-->\n?/g, '');
    }
    return t;
  };
  const pages = new Map();
  for (const abs of walk(out).filter((f) => f.endsWith('.md'))) {
    const rel = path.relative(out, abs).split(path.sep).join('/');
    const route = rel.replace(/(^|\/)index\.md$/, '$1').replace(/\.md$/, '/');
    const [, front, body] = fs.readFileSync(abs, 'utf8').match(/^---\n([\s\S]*?)\n---\n([\s\S]*)$/);
    const { title, description } = YAML.parse(front);
    const md = withoutComments(body)
      .replace(/<pre class="mermaid">([\s\S]*?)<\/pre>\n/g, (_, src) => '```mermaid\n' + unescapeHtml(src) + '\n```\n')
      .replace(/<small>(Source: .*)<\/small>/, '$1')
      .replace(new RegExp(`\\]\\(${escapeRe(BASE)}/`, 'g'), `](${ORIGIN}${BASE}/`)
      .replace(new RegExp(`\\]\\(${SITE_RE}/docs/((?:[^)\\s#/]+/)*)(?=[#)\\s])`, 'g'), (_, r) => `](${ORIGIN}${BASE}/docs/${r}index.md`);
    const text = `# ${title}\n\n${description ? `> ${description}\n\n` : ''}${md.trim()}\n`;
    const file = path.join(dir, route, 'index.md');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, text);
    pages.set(route, { title, description, text, url: `${ORIGIN}${BASE}/docs/${route}index.md` });
  }
  checkSiteLinks(pages);
  return pages;
}

// checkSiteLinks fails the build when generated Markdown links to an address
// on this site that the build will not produce. The docs write such links in
// full (https://...), which Starlight's link check skips.
function checkSiteLinks(pages) {
  const custom = fs.readdirSync(path.join(site, 'src/pages')).map((f) => (f === 'index.astro' ? '' : f.replace(/\.astro$/, '/')));
  const exists = (p) =>
    p.startsWith('docs/') && /(^|\/)(index\.md)?$/.test(p)
      ? pages.has(p.slice(5).replace(/index\.md$/, ''))
      : custom.includes(p) || ['llms.txt', 'agents.txt', 'install.sh', 'compose.yaml'].includes(p) || fs.existsSync(path.join(pub, p));
  const broken = [];
  for (const [route, page] of pages)
    for (const [url] of page.text.matchAll(new RegExp(`${SITE_RE}/[^\\s)>"'\`|]*`, 'g'))) {
      const p = url.slice(`${ORIGIN}${BASE}/`.length).split(/[#?]/)[0].replace(/[.,;:]+$/, '');
      if (!exists(p)) broken.push(`docs/${route}: ${url}`);
    }
  if (broken.length) throw new Error(`links to pages this site does not have:\n  ${broken.join('\n  ')}`);
}

// llmsTxt lists the pages an assistant should read, most useful first. A
// route that no longer has a page fails the build, so the list cannot rot.
function llmsTxt(pages) {
  const link = (route, note, label) => {
    const page = pages.get(route);
    if (!page) throw new Error(`llms.txt lists docs/${route}, which has no page`);
    return `- [${label ?? page.title}](${page.url}): ${note}`;
  };
  return `# Differently

> Differently is an open-source, self-hosted control plane for sandboxes. Applications and agents call one API (REST, CLI, and SDKs for TypeScript, Python and Go) to create sandboxes, run commands and move files. Differently checks keys and quotas, decides which backend each sandbox runs on, and keeps an audit trail. By default work goes to an OpenSandbox server on your own infrastructure; managed providers such as Daytona or Freestyle get work only where policy allows it. Apache-2.0, no telemetry, pre-release (alpha).

To try it: \`curl -fsSL ${ORIGIN}${BASE}/install.sh | sh\`, then \`differently-server --dev\`, then \`differently run -- echo hello\` in a second terminal. Dev mode runs commands on the local machine with no isolation.

An agent asked to install, call or deploy Differently should read ${ORIGIN}${BASE}/agents.txt first. Every page below is Markdown; any docs page is available as Markdown by adding \`index.md\` to its address.

## Start here

${link('explainer/', 'what it is in plain words, the five terms to know, what you need to start and what can wait')}
${link('guides/quickstart/', 'install, start a local server, run a first sandbox, call it from code, step up to a real backend')}
- [Instructions for agents](${ORIGIN}${BASE}/agents.txt): step-by-step tasks with checks for installing it, calling it from code and deploying it
- [Install script](${ORIGIN}${BASE}/install.sh): installs the CLI and the server from the newest GitHub release, with a checksum check
${link('faq/', 'short answers on snapshots across providers, switching, outages, trust and readiness')}
${link('features/', 'what the control plane does, feature by feature')}

## Use it

${link('reference/api/', 'every HTTP route, parameter and schema, generated from the OpenAPI contract')}
- [OpenAPI contract](https://raw.githubusercontent.com/different-ai/differently/${BRANCH}/api/openapi.yaml): the machine-readable API description; a running server serves it at /openapi.yaml
${link('reference/cli/', 'every differently command and flag, generated from its help output')}
${link('sdks/typescript/', 'install, quickstart, streaming, errors')}
${link('sdks/python/', 'install, quickstart, streaming, errors')}
${link('sdks/go/', 'the Go client the CLI is built on')}
${link('guides/baselines/', 'one template name for each provider\'s own image or snapshot, and what stays on its pool')}
${link('guides/organization-onboarding/', 'organizations, projects, quotas, scoped keys and policies')}
${link('guides/keys-and-providers/', 'storing provider keys, switching providers, rotating keys')}
${link('guides/setup-file/', 'pools, policies, templates, organizations and quotas in a file, with differently diff and apply')}

## Deploy and operate

${link('deploy/', 'where it can run and what is tested, documented or unverified', 'Deployment overview')}
${link('deploy/docker/', 'Postgres, OpenSandbox and Differently on one Docker host from published images: one file, docker compose up')}
- [Compose file](${ORIGIN}${BASE}/compose.yaml): the file that guide downloads, with both config files inside it
${link('deploy/single-vm/', 'k3s, OpenSandbox and Differently on one machine')}
${link('deploy/kubernetes/', 'the Helm chart on an existing cluster')}
${link('deploy/prerequisites/', 'cluster, Postgres and a sandbox runtime')}
${link('deploy/day-2/', 'upgrades, migrations, backups, drains, uninstall')}
${link('guides/opensandbox-pools/', 'the default private runtime and its options')}
${link('guides/hybrid-overflow/', 'approving Daytona or Freestyle as policy-gated external capacity')}
${link('providers/', 'every backend, with its tested status, capabilities and configuration', 'Provider catalog')}
${link('security/hardening-checklist/', 'what to do before production')}

## How it works

${link('architecture/', 'layers, components, request lifecycle, state machine', 'Architecture')}
${link('scheduling/placement-policy/', 'how policies merge and how a pool is chosen')}
${link('scheduling/capacity/', 'pool slots, the resume queue, preemption and lifecycle limits')}
${link('security/trust-model/', 'threats, boundaries, enforced controls, residual risks')}
${link('storage-and-lifecycle/', 'persistence, snapshots, fork, portability limits')}
${link('adapters/specification/', 'the backend contract and the HTTP plugin protocol')}

## Optional

${link('feature-parity/', 'Differently next to other sandbox products, and the backend capability matrix')}
${link('competitive-landscape/', 'ownership models and where Differently fits')}
${link('roadmap/', 'phases and current status')}
${link('maintaining/', 'for people and agents changing Differently itself')}
- [Source code](${GITHUB}): the repository; AGENTS.md there is the entry point for contributors
`;
}

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
    `Generated from the help output of [\`cmd/differently\`](${GITHUB}/tree/${BRANCH}/cmd/differently). The [quickstart](${BASE}/docs/guides/quickstart/) installs it; from a checkout, \`make build\` puts it in \`bin/differently\`.`,
    '',
    'Configuration comes from flags, then `DIFFERENTLY_URL`, `DIFFERENTLY_API_KEY` and `DIFFERENTLY_PROJECT`, then the file written by `differently login` (and by `differently admin org create`, which saves the first project as the default for that server). Against a local `--dev` server the CLI picks up the operator token from `~/.differently/dev/token`.',
    '',
  ];
  for (const [args, text] of sections) {
    lines.push(`${'#'.repeat(Math.min(2 + Math.max(args.length - 1, 0), 4))} ${['differently', ...args].join(' ')}`, '', '```text', text.trimEnd(), '```', '');
  }
  return lines.join('\n');
}
