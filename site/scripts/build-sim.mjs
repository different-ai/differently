// Builds what the /simulator/ page runs: the control plane and its simulated
// users (../cmd/differently-loadsim) as WebAssembly, with Go's loader beside it.
//   ../cmd/differently-loadsim -> public/sim/differently.wasm
//   $GOROOT/lib/wasm/wasm_exec.js -> public/sim/wasm_exec.js
// Both are git-ignored; public/sim/worker.js is the hand-written part. Needs Go.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import zlib from 'node:zlib';

const site = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repo = path.resolve(site, '..');
const out = path.join(site, 'public/sim');
const wasm = path.join(out, 'differently.wasm');

fs.mkdirSync(out, { recursive: true });
execFileSync('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', wasm, './cmd/differently-loadsim'], {
  cwd: repo,
  stdio: 'inherit',
  env: { ...process.env, GOOS: 'js', GOARCH: 'wasm' },
});
const goroot = execFileSync('go', ['env', 'GOROOT'], { cwd: repo, encoding: 'utf8' }).trim();
fs.copyFileSync(path.join(goroot, 'lib/wasm/wasm_exec.js'), path.join(out, 'wasm_exec.js'));

const bytes = fs.readFileSync(wasm);
const mb = (n) => (n / 1e6).toFixed(1);
console.log(`simulator: ${mb(bytes.length)} MB of WebAssembly, ${mb(zlib.gzipSync(bytes).length)} MB compressed`);
