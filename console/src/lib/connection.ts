import { Differently, DifferentlyError } from '@differently/sdk';

/** static: the browser calls Differently with a key the user enters. server: the console's own server proxies the API and signs people in. */
export const mode = import.meta.env.PUBLIC_CONSOLE_MODE;
export const base = import.meta.env.BASE_URL;

const KEY = 'differently.apiKey';
const URL_KEY = 'differently.url';

function read(store: Storage | undefined, key: string): string | null {
  try { return store?.getItem(key) ?? null; } catch { return null; }
}

/** The server URL the static console talks to. */
export function serverURL(): string {
  return read(localStorage, URL_KEY) || import.meta.env.PUBLIC_DIFFERENTLY_URL || location.origin;
}

export function hasKey(): boolean {
  return mode === 'server' || !!(read(sessionStorage, KEY) || read(localStorage, KEY));
}

/** Saves a static connection. The key stays in this tab unless remember is set. */
export function saveConnection(url: string, key: string, remember: boolean): void {
  try {
    localStorage.setItem(URL_KEY, url.replace(/\/$/, ''));
    sessionStorage.setItem(KEY, key);
    if (remember) localStorage.setItem(KEY, key);
    else localStorage.removeItem(KEY);
  } catch { /* storage unavailable: the key lives only in memory for this page */ }
}

export function forgetConnection(): void {
  try { sessionStorage.removeItem(KEY); localStorage.removeItem(KEY); } catch {}
}

/** A client for the connection the console runs with. */
export function client(override?: { url: string; key: string }): Differently {
  if (override) return new Differently({ baseUrl: override.url, apiKey: override.key, maxRetries: 0, timeoutMs: 15_000 });
  if (mode === 'server') {
    // The proxy at /api/v1 adds the server's own key; this placeholder never leaves the console.
    return new Differently({ baseUrl: `${location.origin}${base}api`, apiKey: 'console-session', maxRetries: 1 });
  }
  const key = read(sessionStorage, KEY) || read(localStorage, KEY);
  if (!key) throw new DifferentlyError({ code: 'unauthenticated', message: 'not connected' });
  return new Differently({ baseUrl: serverURL(), apiKey: key, maxRetries: 1 });
}

/** Where to send someone who is not signed in or connected. */
export function entryPage(): string {
  return base + (mode === 'server' ? 'sign-in/' : 'connect/');
}

export async function session(): Promise<{ user: { email: string; name?: string } } | null> {
  if (mode !== 'server') return null;
  const res = await fetch(`${base}api/auth/get-session`, { credentials: 'same-origin' });
  if (!res.ok) return null;
  return (await res.json()) ?? null;
}
