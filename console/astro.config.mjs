// @ts-check
import { defineConfig } from 'astro/config';
import { existsSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import { fileURLToPath } from 'node:url';

// static (default): plain files the browser serves; it calls Differently
// directly with an API key the user enters. server: the same pages plus a
// server that signs people in (Better Auth) and proxies /api to
// Differently with a key only the server holds.
const mode = process.env.CONSOLE_MODE === 'server' ? 'server' : 'static';
// Server mode reads its settings from process.env; take them from .env in development too.
if (mode === 'server' && existsSync('.env')) process.loadEnvFile('.env');
const differently = (process.env.DIFFERENTLY_URL ?? 'http://localhost:8080').replace(/\/$/, '');
// Serve under a subpath, e.g. CONSOLE_BASE=/console/ for a project page or a path behind a proxy.
const base = `/${(process.env.CONSOLE_BASE ?? '').replace(/^\/+|\/+$/g, '')}/`.replace('//', '/');

/** Adds the sign-in and proxy routes; only in server mode. */
const serverRoutes = {
  name: 'differently-console-server',
  hooks: {
    /** @param {{ injectRoute: (route: { pattern: string; entrypoint: string; prerender?: boolean }) => void }} options */
    'astro:config:setup': ({ injectRoute }) => {
      injectRoute({ pattern: '/api/auth/[...all]', entrypoint: './src/server/auth-route.ts', prerender: false });
      injectRoute({ pattern: '/api/sign-in-options', entrypoint: './src/server/options-route.ts', prerender: false });
      injectRoute({ pattern: '/api/[...path]', entrypoint: './src/server/proxy-route.ts', prerender: false });
    },
  },
};

/**
 * In static development, forwards /v1 and /version to Differently so the
 * console and the API share an origin and no CORS setup is needed.
 */
/** @type {import('vite').Plugin} */
const devForward = {
  name: 'differently-console-dev-forward',
  apply: 'serve',
  // Runs before Astro's own dev handler, which would answer 404.
  enforce: 'pre',
  configureServer(server) {
    const target = new URL(differently);
    const lib = target.protocol === 'https:' ? https : http;
    server.middlewares.use((req, res, next) => {
      const path = req.url ?? '';
      if (!(path.startsWith('/v1/') || path === '/version' || path.startsWith('/version?'))) return next();
      const upstream = lib.request(
        { protocol: target.protocol, hostname: target.hostname, port: target.port, method: req.method, path: target.pathname.replace(/\/$/, '') + path, headers: { ...req.headers, host: target.host } },
        (r) => { res.writeHead(r.statusCode ?? 502, r.headers); r.pipe(res); },
      );
      upstream.on('error', () => {
        res.writeHead(502, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: { code: 'connection_error', message: `could not reach ${differently}` } }));
      });
      req.pipe(upstream);
    });
  },
};

export default defineConfig({
  output: 'static',
  base,
  adapter: mode === 'server' ? (await import('@astrojs/node')).default({ mode: 'standalone' }) : undefined,
  integrations: mode === 'server' ? [serverRoutes] : [],
  devToolbar: { enabled: false },
  vite: {
    plugins: mode === 'static' ? [devForward] : [],
    resolve: {
      alias: { '@differently/sdk': fileURLToPath(new URL('../sdk/typescript/src/index.ts', import.meta.url)) },
    },
    define: { 'import.meta.env.PUBLIC_CONSOLE_MODE': JSON.stringify(mode) },
  },
});
