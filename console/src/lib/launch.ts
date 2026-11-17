// The launcher's model: which pools can take a batch, what it would cost,
// and how a batch is sent. Nothing here touches the DOM.
import type { CostModel, Pool, Resources } from '@differently/sdk';
import type { Gate } from './routing';

/** Guards against a slip of the hand; the server's quotas and budgets are the real limits. */
export const MAX_PER_POOL = 50;
export const MAX_TOTAL = 100;

/** The server's estimate for one sandbox-hour: the pool's prices times the sandbox's size. */
export function hourlyCost(cost: CostModel | undefined, r: Resources | undefined): number {
  if (!cost || !r) return 0;
  return (cost.sandboxHour ?? 0) + r.cpu * (cost.cpuHour ?? 0) + (r.memoryMiB / 1024) * (cost.memoryGiBHour ?? 0) + (r.diskGiB ?? 0) * (cost.diskGiBHour ?? 0);
}

export interface LaunchRow {
  pool: string;
  /** "external · eu · daytona" */
  sub: string;
  external: boolean;
  /** False when a sandbox pinned here would be refused or never placed; note says why. */
  open: boolean;
  note: string;
  /** Free slots under the pool's cap, when it has one. */
  free?: number;
  /** Estimated cost of one sandbox of the chosen size, per hour. 0 when the pool has no prices. */
  hourly: number;
}

/** What the launcher shows for one pool, for sandboxes of the given size. */
export function launchRow(p: Pool, gate: Gate, resources?: Resources): LaunchRow {
  const external = p.trust !== 'private';
  const cap = p.maxConcurrency ?? 0;
  const free = cap > 0 ? Math.max(0, cap - p.active) : undefined;
  const health = p.health?.status ?? 'unknown';
  let open = true;
  let note = '';
  if (!p.enabled) [open, note] = [false, 'disabled'];
  else if (p.draining) [open, note] = [false, 'draining: it takes no new sandboxes'];
  else if (external && gate.mode === 'never') [open, note] = [false, 'external placement is off in the global policy'];
  else if (external && !gate.approved.includes(p.name)) [open, note] = [false, 'not approved by the global policy'];
  else if (health !== 'healthy' && health !== 'unknown') note = `${health}: sandboxes wait until it recovers`;
  else if (p.spend?.budgetReached) note = 'budget reached: new sandboxes wait';
  else if (free === 0) note = 'full: new sandboxes wait for a slot';
  else if (free !== undefined) note = `${free} free slot${free === 1 ? '' : 's'}`;
  return {
    pool: p.name,
    sub: [external ? 'external' : 'private', p.region, p.adapter].filter(Boolean).join(' · '),
    external,
    open,
    note,
    free,
    hourly: hourlyCost(p.costModel, resources),
  };
}

/** A count field's value as a whole number from 0 to max. */
export function clampCount(v: unknown, max = MAX_PER_POOL): number {
  const n = Math.floor(Number(v));
  return Number.isFinite(n) && n > 0 ? Math.min(n, max) : 0;
}

export function totalOf(counts: Record<string, number>): number {
  return Object.values(counts).reduce((n, v) => n + clampCount(v), 0);
}

/** One entry per sandbox to create. Pools are interleaved so they fill at the same pace. */
export function planOf(counts: Record<string, number>): string[] {
  const left = Object.entries(counts)
    .map(([pool, n]) => ({ pool, n: clampCount(n) }))
    .filter((x) => x.n > 0);
  const plan: string[] = [];
  while (left.some((x) => x.n > 0)) {
    for (const x of left) {
      if (x.n > 0) {
        plan.push(x.pool);
        x.n--;
      }
    }
  }
  return plan;
}

/** What happened to one create request. */
export interface Outcome {
  pool: string;
  id?: string;
  error?: string;
}

export interface Summary {
  requested: number;
  created: number;
  refused: number;
  pools: { pool: string; created: number; refused: number; reasons: string[] }[];
}

/** Outcomes grouped by pool, with each distinct refusal reason once. */
export function summarize(outcomes: Outcome[]): Summary {
  const s: Summary = { requested: outcomes.length, created: 0, refused: 0, pools: [] };
  for (const o of outcomes) {
    let row = s.pools.find((p) => p.pool === o.pool);
    if (!row) s.pools.push((row = { pool: o.pool, created: 0, refused: 0, reasons: [] }));
    if (o.error === undefined) {
      row.created++;
      s.created++;
    } else {
      row.refused++;
      s.refused++;
      if (!row.reasons.includes(o.error)) row.reasons.push(o.error);
    }
  }
  return s;
}

/** Runs tasks with at most `limit` in flight. Results keep the tasks' order; onEach reports progress. */
export async function runLimited<T>(tasks: (() => Promise<T>)[], limit: number, onEach?: (done: number) => void): Promise<T[]> {
  const results = new Array<T>(tasks.length);
  let next = 0;
  let done = 0;
  const worker = async () => {
    while (next < tasks.length) {
      const i = next++;
      results[i] = await tasks[i]();
      onEach?.(++done);
    }
  };
  await Promise.all(Array.from({ length: Math.max(1, Math.min(limit, tasks.length)) }, worker));
  return results;
}
