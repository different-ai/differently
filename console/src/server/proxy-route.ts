import type { APIRoute } from 'astro';
import { auth } from './auth';

export const prerender = false;

const target = (process.env.DIFFERENTLY_URL ?? 'http://localhost:8080').replace(/\/$/, '');
const root = new URL(`${target}/`);
// Behind a reverse proxy the request URL may not carry the public origin; BETTER_AUTH_URL does.
const publicOrigin = process.env.BETTER_AUTH_URL ? new URL(process.env.BETTER_AUTH_URL).origin : null;
const forwarded = ['accept', 'content-type', 'idempotency-key', 'last-event-id', 'x-request-id'];
const returned = ['content-type', 'content-disposition', 'content-security-policy', 'x-content-type-options', 'location', 'retry-after', 'x-request-id'];

const problem = (status: number, code: string, message: string) => Response.json({ error: { code, message } }, { status });

/**
 * Forwards /api/v1/* and /api/version to Differently for signed-in people,
 * with the key only this server holds (DIFFERENTLY_API_KEY). Differently
 * can stay on a private network or behind a tunnel; browsers never see it.
 */
export const ALL: APIRoute = async ({ request, params, url }) => {
  // Check the path after resolving it, so an encoded "..%2F" cannot reach anything but /version and /v1/*.
  const dest = new URL(`${params.path ?? ''}${url.search}`, root);
  const rel = dest.pathname.startsWith(root.pathname) ? dest.pathname.slice(root.pathname.length) : '';
  if (dest.origin !== root.origin || (rel !== 'version' && !rel.startsWith('v1/'))) return problem(404, 'not_found', `no console route for ${url.pathname}`);
  // The session is a cookie, so refuse writes sent from another origin.
  const origin = request.headers.get('origin');
  if (request.method !== 'GET' && request.method !== 'HEAD' && origin && origin !== url.origin && origin !== publicOrigin) return problem(403, 'permission_denied', 'cross-origin request refused');
  if (!(await auth.api.getSession({ headers: request.headers }))) return problem(401, 'unauthenticated', 'sign in to the console');
  const key = process.env.DIFFERENTLY_API_KEY;
  if (!key) return problem(503, 'provider_unavailable', 'the console server has no DIFFERENTLY_API_KEY');

  const headers = new Headers({ authorization: `Bearer ${key}` });
  for (const h of forwarded) {
    const v = request.headers.get(h);
    if (v) headers.set(h, v);
  }
  const hasBody = request.method !== 'GET' && request.method !== 'HEAD';
  let upstream: Response;
  try {
    upstream = await fetch(dest, {
      method: request.method,
      headers,
      body: hasBody ? request.body : undefined,
      redirect: 'manual',
      ...(hasBody ? { duplex: 'half' } : {}),
    } as RequestInit);
  } catch {
    return problem(502, 'connection_error', `the console server could not reach Differently at ${target}`);
  }
  const out = new Headers();
  for (const h of returned) {
    const v = upstream.headers.get(h);
    if (v) out.set(h, v);
  }
  return new Response(upstream.body, { status: upstream.status, headers: out });
};
