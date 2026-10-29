import { DifferentlyError } from '@differently/sdk';
import { svg } from './icons';
import { entryPage } from './connection';

type Child = Node | string | number | null | undefined | false;

/** Builds an element. Attributes starting with "on" are listeners. */
export function el<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: Record<string, unknown> = {}, ...children: Child[]): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v as EventListener);
    else if (k === 'html') node.innerHTML = String(v);
    else node.setAttribute(k === 'className' ? 'class' : k, v === true ? '' : String(v));
  }
  for (const c of children) if (c !== null && c !== undefined && c !== false) node.append(typeof c === 'number' ? String(c) : c);
  return node;
}

export const icon = (name: string, size?: number) => el('span', { html: svg(name, size), style: 'display:inline-flex' });

export interface Column<T> { label: string; cell: (row: T) => Child; className?: string }

/** A list table; with no rows, the empty node instead. */
export function table<T>(columns: Column<T>[], rows: T[], empty: Node): Node {
  if (rows.length === 0) return empty;
  return el('div', { class: 'table-wrap' },
    el('table', { class: 'list' },
      el('thead', {}, el('tr', {}, ...columns.map((c) => el('th', { class: c.className }, c.label)))),
      el('tbody', {}, ...rows.map((r) => el('tr', {}, ...columns.map((c) => el('td', { class: c.className }, c.cell(r))))))));
}

/** The grey empty-state card. */
export function empty(text: string, iconNames: string[] = [], ...actions: Node[]): Node {
  const tiles = iconNames.length
    ? el('div', { class: 'tiles' }, ...iconNames.flatMap((n, i) => [i ? el('span', { html: svg('arrow', 18) }) : null, el('div', { class: 'tile', html: svg(n, 26) })]))
    : null;
  return el('div', { class: 'empty' }, tiles, el('p', {}, text), actions.length ? el('div', { class: 'page-actions' }, ...actions) : null);
}

export function mount(target: string | Element, ...nodes: Child[]): void {
  const t = typeof target === 'string' ? document.querySelector(target) : target;
  t?.replaceChildren(...nodes.filter((n): n is Node | string => n !== null && n !== undefined && n !== false).map((n) => (typeof n === 'number' ? String(n) : n)));
}

const tone: Record<string, string> = {
  running: 'ok', healthy: 'ok', active: 'ok', ok: 'ok', valid: 'ok', placed: 'ok',
  queued: 'warn', provisioning: 'warn', degraded: 'warn', draining: 'warn', terminating: 'warn',
  failed: 'bad', unavailable: 'bad', revoked: 'bad', denied: 'bad', rejected: 'bad', invalid: 'bad',
};
export const badge = (status: string) => el('span', { class: `badge ${tone[status] ?? ''}` }, status.replace(/_/g, ' '));

// Go's zero time ("0001-01-01T00:00:00Z") means the value was never set.
export const isSet = (iso?: string): iso is string => !!iso && !iso.startsWith('0001-');

export function relTime(iso?: string): string {
  if (!isSet(iso)) return '—';
  const d = new Date(iso).getTime();
  const s = Math.round((Date.now() - d) / 1000);
  const abs = Math.abs(s);
  const fmt = (n: number, u: string) => `${n} ${u}${n === 1 ? '' : 's'}`;
  const out = abs < 60 ? 'just now' : abs < 3600 ? fmt(Math.round(abs / 60), 'minute') : abs < 86400 ? fmt(Math.round(abs / 3600), 'hour') : abs < 86400 * 30 ? fmt(Math.round(abs / 86400), 'day') : new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  if (out === 'just now' || !/\d/.test(out[0])) return out;
  return s >= 0 ? `${out} ago` : `in ${out}`;
}

export function date(iso?: string): string {
  return isSet(iso) ? new Date(iso).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }) : '—';
}

export function toast(message: string, kind: 'info' | 'bad' = 'info'): void {
  const t = el('div', { class: `toast ${kind === 'bad' ? 'bad' : ''}`, role: 'status' }, message);
  document.getElementById('toasts')?.append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 6000 : 3500);
}

export function copyButton(text: string): HTMLButtonElement {
  return el('button', { class: 'btn small', type: 'button', onclick: async () => { await navigator.clipboard.writeText(text); toast('Copied'); } }, icon('copy', 14), 'Copy');
}

/** Asks before something irreversible. */
export function confirmAction(title: string, body: string, okLabel = 'Confirm', danger = true): Promise<boolean> {
  const d = document.getElementById('confirm') as HTMLDialogElement;
  (document.getElementById('confirm-title') as HTMLElement).textContent = title;
  (document.getElementById('confirm-body') as HTMLElement).textContent = body;
  const ok = document.getElementById('confirm-ok') as HTMLButtonElement;
  ok.textContent = okLabel;
  ok.className = `btn ${danger ? 'primary' : 'primary'}`;
  d.returnValue = '';
  d.showModal();
  return new Promise((resolve) => d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true }));
}

/** A row menu of actions. */
export function rowMenu(items: { label: string; run: () => unknown; danger?: boolean; hidden?: boolean }[]): Node {
  const shown = items.filter((i) => !i.hidden);
  if (!shown.length) return document.createTextNode('');
  const list = el('div', { class: 'menu-list', hidden: true });
  const btn = el('button', { class: 'btn ghost icon-btn', type: 'button', 'aria-label': 'Actions', html: svg('more', 18) });
  const close = () => { list.hidden = true; document.removeEventListener('click', outside); };
  const outside = (e: Event) => { if (!wrap.contains(e.target as Node)) close(); };
  btn.addEventListener('click', () => { list.hidden = !list.hidden; if (!list.hidden) setTimeout(() => document.addEventListener('click', outside)); });
  for (const i of shown) list.append(el('button', { type: 'button', class: i.danger ? 'danger' : '', onclick: () => { close(); void run(i.run); } }, i.label));
  const wrap = el('div', { class: 'menu' }, btn, list);
  return wrap;
}

/** Runs an action and reports failures as a toast. */
export async function run<T>(fn: () => T | Promise<T>): Promise<T | undefined> {
  try { return await fn(); } catch (e) { report(e); return undefined; }
}

export function message(e: unknown): string {
  if (e instanceof DifferentlyError) {
    if (e.code === 'permission_denied') return 'This key does not have permission for that.';
    if (e.code === 'connection_error') return 'Could not reach the server.';
    return e.message;
  }
  return e instanceof Error ? e.message : String(e);
}

export function report(e: unknown): void {
  if (e instanceof DifferentlyError && e.code === 'unauthenticated') { location.href = entryPage(); return; }
  toast(message(e), 'bad');
}

/** Loads a page section, showing a notice instead when it fails. */
export async function section(target: string, load: () => Promise<Child | Child[]>): Promise<void> {
  try {
    const out = await load();
    mount(target, ...(Array.isArray(out) ? out : [out]));
  } catch (e) {
    if (e instanceof DifferentlyError && e.code === 'unauthenticated') { location.href = entryPage(); return; }
    const text = e instanceof DifferentlyError && e.code === 'permission_denied'
      ? 'This key cannot see this page. Sign in with an operator key, or one with the needed scope.'
      : message(e);
    mount(target, el('div', { class: 'notice bad' }, text));
  }
}

/** Wires a <dialog> with a form: opens from [data-open=id], enables Save when the form is valid, submits through onSubmit. */
export function dialog(id: string, onSubmit: (form: HTMLFormElement) => Promise<boolean | void>): { open: () => void; close: () => void; form: HTMLFormElement } {
  const d = document.getElementById(id) as HTMLDialogElement;
  const form = d.querySelector('form') as HTMLFormElement;
  const save = form.querySelector('[data-save]') as HTMLButtonElement | null;
  const sync = () => { if (save) save.disabled = !form.checkValidity(); };
  form.addEventListener('input', sync);
  form.addEventListener('change', sync);
  d.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => d.close()));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (save) save.disabled = true;
    try { if ((await onSubmit(form)) !== false) d.close(); } catch (err) { report(err); } finally { sync(); }
  });
  const open = () => { form.reset(); sync(); d.showModal(); };
  document.querySelectorAll(`[data-open="${id}"]`).forEach((b) => b.addEventListener('click', open));
  return { open, close: () => d.close(), form };
}

export const field = (form: HTMLFormElement, name: string): string => String(new FormData(form).get(name) ?? '').trim();
