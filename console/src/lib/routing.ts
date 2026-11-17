// The live routing view's model: it turns the API's pools, policies and audit
// events into what the page draws. Nothing here touches the DOM, so the rules
// can be read (and tested) on their own.
import type { AuditEvent, PlacementPolicy, Pool } from '@differently/sdk';

/** What happened to one sandbox, as the view shows it. */
export type FlowKind = 'created' | 'placed' | 'rejected' | 'failed' | 'ended';

export interface Flow {
  seq: number;
  time: string;
  kind: FlowKind;
  sandbox: string;
  org?: string;
  pool?: string;
  trust?: string;
  /** The rest of the log line: the strategy, or the reason. */
  note: string;
}

const KINDS: Record<string, FlowKind> = {
  'sandbox.create': 'created',
  'sandbox.placed': 'placed',
  'sandbox.rejected': 'rejected',
  'sandbox.failed': 'failed',
  // "sandbox.expired" only starts a termination; "sandbox.terminated" follows
  // it and carries the pool, so one sandbox ending is one line.
  'sandbox.terminated': 'ended',
};

/** The routing event an audit event describes, or null when it is about something else. */
export function flowOf(e: AuditEvent): Flow | null {
  const kind = KINDS[e.action];
  if (!kind) return null;
  const d = e.details ?? {};
  let note = d.reason ?? '';
  if (kind === 'placed') note = [d.strategy, d.trust === 'external' ? `external ${d.external}` : ''].filter(Boolean).join(' · ');
  if (kind === 'created') note = [d.classification, d.cpu ? `${d.cpu} CPU` : ''].filter(Boolean).join(' · ');
  return { seq: e.seq, time: e.time, kind, sandbox: e.resource, org: e.orgId, pool: d.pool || undefined, trust: d.trust || undefined, note };
}

export interface Stats {
  /** Placements in the last minute. */
  perMinute: number;
  /** Counts over the window. */
  placed: number;
  external: number;
  refused: number;
  failed: number;
  perPool: Record<string, number>;
}

/** Counts routing events over a window ending at now. */
export function statsOf(flows: Flow[], now: number, windowMs = 5 * 60_000): Stats {
  const s: Stats = { perMinute: 0, placed: 0, external: 0, refused: 0, failed: 0, perPool: {} };
  for (const f of flows) {
    const age = now - new Date(f.time).getTime();
    if (age > windowMs || age < -60_000) continue;
    if (f.kind === 'placed') {
      s.placed++;
      if (f.trust === 'external') s.external++;
      if (f.pool) s.perPool[f.pool] = (s.perPool[f.pool] ?? 0) + 1;
      if (age <= 60_000) s.perMinute++;
    } else if (f.kind === 'rejected') s.refused++;
    else if (f.kind === 'failed') s.failed++;
  }
  return s;
}

export type GateMode = 'never' | 'overflow' | 'allowed';

export interface Gate {
  mode: GateMode;
  /** External pools the global policy approves by name. */
  approved: string[];
  /** One line on what the mode means for work leaving your own pools. */
  meaning: string;
}

const MEANING: Record<GateMode, string> = {
  never: 'closed: nothing leaves your own pools',
  overflow: 'opens only when your own pools are full',
  allowed: 'open: approved providers rank beside your pools',
};

/** The fail-closed gate in front of external pools, from the global policy. Unset means never. */
export function gateOf(policies: PlacementPolicy[]): Gate {
  const global = policies.find((p) => p.scope === 'global');
  const mode: GateMode = global?.external === 'overflow' || global?.external === 'allowed' ? global.external : 'never';
  return { mode, approved: global?.externalPools ?? [], meaning: MEANING[mode] };
}

export interface PoolView {
  name: string;
  /** "private · dc1 · opensandbox" */
  sub: string;
  external: boolean;
  /** ok, warn or bad colour the health dot; off greys the whole pool out. */
  state: 'ok' | 'warn' | 'bad' | 'off';
  stateLabel: string;
  /** External pools only: whether policy lets work reach it at all. */
  reachable: boolean;
  active: number;
  /** Slot boxes to draw and how many are filled; more counts sandboxes beyond them. */
  slots: number;
  filled: number;
  more: number;
  load: string;
  budget?: { spent: number; limit: number; reached: boolean };
  attestations: string[];
}

export const MAX_SLOTS = 12;

/** What the view shows for one pool. */
export function poolView(p: Pool, gate: Gate, now: number = Date.now()): PoolView {
  const external = p.trust !== 'private';
  const health = p.health?.status ?? 'unknown';
  let state: PoolView['state'] = health === 'healthy' ? 'ok' : health === 'unknown' ? 'warn' : 'bad';
  let stateLabel = health;
  if (!p.enabled) [state, stateLabel] = ['off', 'disabled'];
  else if (p.draining) [state, stateLabel] = ['warn', 'draining'];
  else if (p.spend?.budgetReached) [state, stateLabel] = ['warn', 'budget reached'];
  const reachable = !external || (gate.mode !== 'never' && gate.approved.includes(p.name));
  const cap = p.maxConcurrency ?? 0;
  const slots = cap > 0 && cap <= MAX_SLOTS ? cap : Math.min(Math.max(p.active, 1), MAX_SLOTS);
  const filled = Math.min(p.active, slots);
  const held = Object.entries(p.attestations ?? {})
    .filter(([, a]) => !a.validUntil || new Date(a.validUntil).getTime() > now)
    .map(([name]) => name)
    .sort();
  return {
    name: p.name,
    sub: [external ? 'external' : 'private', p.region, p.adapter].filter(Boolean).join(' · '),
    external,
    state,
    stateLabel,
    reachable,
    active: p.active,
    slots,
    filled,
    more: p.active - filled,
    load: cap > 0 ? `${p.active} / ${cap} slots` : `${p.active} running`,
    budget: p.budget?.monthly ? { spent: p.spend?.estimated ?? 0, limit: p.budget.monthly, reached: !!p.spend?.budgetReached } : undefined,
    attestations: held,
  };
}

/** Private pools first, then external ones; by name within each group. */
export function sortPools(pools: Pool[]): Pool[] {
  return [...pools].sort((a, b) => Number(a.trust !== 'private') - Number(b.trust !== 'private') || a.name.localeCompare(b.name));
}

/** "sbx_01m7khkzawme…" becomes "sbx_…r6maz": enough to tell sandboxes apart in a narrow log. */
export function shortId(id: string): string {
  const i = id.indexOf('_');
  return id.length > 14 && i > 0 ? `${id.slice(0, i + 1)}…${id.slice(-5)}` : id;
}

export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, { hour12: false });
}

export function money(n: number): string {
  return n >= 100 ? `$${Math.round(n)}` : `$${n.toFixed(2)}`;
}

// Wire geometry: horizontal-first cubic curves, like the diagram in the intro video.
export interface XY { x: number; y: number }
export interface Curve { a: XY; c1: XY; c2: XY; b: XY }

export function hcurve(a: XY, b: XY): Curve {
  const mid = (a.x + b.x) / 2;
  return { a, c1: { x: mid, y: a.y }, c2: { x: mid, y: b.y }, b };
}

export function pathOf(c: Curve): string {
  return `M ${c.a.x} ${c.a.y} C ${c.c1.x} ${c.c1.y}, ${c.c2.x} ${c.c2.y}, ${c.b.x} ${c.b.y}`;
}

/** The point at t (0 to 1) along a curve. */
export function along(c: Curve, t: number): XY {
  const m = 1 - t;
  return {
    x: m * m * m * c.a.x + 3 * m * m * t * c.c1.x + 3 * m * t * t * c.c2.x + t * t * t * c.b.x,
    y: m * m * m * c.a.y + 3 * m * m * t * c.c1.y + 3 * m * t * t * c.c2.y + t * t * t * c.b.y,
  };
}

export const ease = (t: number): number => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);
