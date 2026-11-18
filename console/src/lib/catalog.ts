// The provider catalog as the console uses it: which catalog entry a pool
// belongs to, what a provider's list prices are as a pool's cost model, where
// a pool stands against its monthly budget, and what an operator could record
// about a provider. Nothing here touches the DOM.
//
// The data is the website's comparison (site/src/data/providers.ts), so the
// console and the site cannot disagree. Everything in it was read from
// provider pages on PRICES_READ_LABEL: list prices are not invoices, and a
// stated certification is the provider's claim, not something Differently
// checked.
import { PRICES_READ_LABEL, billingLabel, providers, slotText, supportLabel } from '../../../site/src/data/providers.ts';
import type { Provider } from '../../../site/src/data/providers.ts';
import type { Attestation, CostModel, Pool } from '@differently/sdk';

export { PRICES_READ_LABEL, billingLabel, providers, slotText, supportLabel };
export type { Provider };

const byId = new Map(providers.map((p) => [p.id, p]));
const byAdapter = new Map(providers.filter((p) => p.adapter).map((p) => [p.adapter as string, p]));

/**
 * The catalog entry a pool belongs to. A pool on a provider's own adapter is
 * that provider's. A pool on another adapter (a remote plugin, a simulated
 * pool) is matched by its `provider` label, then by its name.
 */
export function entryFor(pool: Pick<Pool, 'adapter' | 'name' | 'labels'>): Provider | undefined {
  return byAdapter.get(pool.adapter) ?? byId.get(pool.labels?.provider ?? '') ?? byId.get(pool.name);
}

const RATES = ['sandboxHour', 'cpuHour', 'memoryGiBHour', 'diskGiBHour'] as const;

/** A provider's list prices as a pool cost model; undefined where its pricing does not map onto one. */
export function listCost(p: Provider | undefined): CostModel | undefined {
  const r = p?.rates;
  if (!r) return undefined;
  const cost: CostModel = { currency: 'USD' };
  if (r.sandboxHour) cost.sandboxHour = r.sandboxHour;
  if (r.cpuHour) cost.cpuHour = r.cpuHour;
  if (r.memoryGiBHour) cost.memoryGiBHour = r.memoryGiBHour;
  return cost;
}

/** Whether a cost model charges anything, so that spend can be estimated from it. */
export const hasPrices = (c?: CostModel): boolean => RATES.some((k) => (c?.[k] ?? 0) > 0);

/** Whether two cost models charge the same. */
export const sameCost = (a?: CostModel, b?: CostModel): boolean => RATES.every((k) => Math.abs((a?.[k] ?? 0) - (b?.[k] ?? 0)) < 1e-9);

/** A rate with the decimals it needs, at least two: "$0.05", "$0.0162". */
export function rate(n: number): string {
  const [whole, decimals = ''] = n.toFixed(6).replace(/0+$/, '').split('.');
  return `$${whole}.${decimals.padEnd(2, '0')}`;
}

/** An amount of money: cents below a hundred dollars, whole dollars above, and "<$0.01" for less than half a cent. */
export const usd = (n: number): string => (n >= 100 ? `$${Math.round(n).toLocaleString('en-US')}` : n > 0 && n < 0.005 ? '<$0.01' : `$${n.toFixed(2)}`);

const UNIT: Record<(typeof RATES)[number], string> = { sandboxHour: 'sandbox-h', cpuHour: 'vCPU-h', memoryGiBHour: 'GiB-h', diskGiBHour: 'GiB disk-h' };

/** "$0.0504 / vCPU-h + $0.0162 / GiB-h", or "" for a pool with no prices. */
export function costText(c?: CostModel): string {
  return RATES.filter((k) => (c?.[k] ?? 0) > 0).map((k) => `${rate(c![k]!)} / ${UNIT[k]}`).join(' + ');
}

/** How a pool's prices relate to the catalog's: the list price, its own, or none at all. */
export type PriceState = 'none' | 'list' | 'custom';
export function priceState(pool: Pick<Pool, 'costModel'>, entry: Provider | undefined): PriceState {
  if (!hasPrices(pool.costModel)) return 'none';
  const list = listCost(entry);
  return list && sameCost(pool.costModel, list) ? 'list' : 'custom';
}

export type BudgetState = 'none' | 'ok' | 'near' | 'reached';

export interface BudgetView {
  /** Estimated spend this month, in the pool's currency. */
  spent: number;
  limit?: number;
  /** Spend as a share of the budget, 0 to 1. */
  share: number;
  state: BudgetState;
  text: string;
}

/** From four fifths of a budget on, the console says the pool is close to it. */
export const NEAR = 0.8;

/**
 * Where a pool stands against its monthly budget. The server reports spend for
 * pools that have a budget; for the others the caller passes this month's
 * estimate from the usage report.
 */
export function budgetView(pool: Pick<Pool, 'budget' | 'spend'>, usageCost = 0): BudgetView {
  const limit = pool.budget?.monthly || undefined;
  const spent = pool.spend?.estimated ?? usageCost;
  if (!limit) return { spent, share: 0, state: 'none', text: spent > 0 ? `${usd(spent)} this month` : 'No spend this month' };
  const share = Math.min(1, spent / limit);
  const state: BudgetState = pool.spend?.budgetReached || spent >= limit ? 'reached' : share >= NEAR ? 'near' : 'ok';
  return { spent, limit, share, state, text: `${usd(spent)} of ${usd(limit)}` };
}

export interface Totals {
  /** Estimated spend this month across pools that have prices. */
  spent: number;
  /** The sum of the monthly budgets that are set. */
  budgeted: number;
  /** Pools with a budget. */
  withBudget: number;
  /** Pools that have used up their budget and take no new sandboxes. */
  reached: number;
  /** Pools without prices: their spend cannot be estimated, so they cannot have a budget. */
  unpriced: number;
  /** External pools, run by someone else, that have no budget. */
  externalOpen: number;
}

/** The figures at the top of the page. `usage` is this month's estimated cost by pool name. */
export function totalsOf(pools: Pool[], usage: Record<string, number> = {}): Totals {
  const t: Totals = { spent: 0, budgeted: 0, withBudget: 0, reached: 0, unpriced: 0, externalOpen: 0 };
  for (const p of pools) {
    const b = budgetView(p, usage[p.name]);
    t.spent += b.spent;
    if (b.limit) { t.budgeted += b.limit; t.withBudget++; }
    if (b.state === 'reached') t.reached++;
    if (!hasPrices(p.costModel)) t.unpriced++;
    if (p.trust !== 'private' && !b.limit) t.externalOpen++;
  }
  return t;
}

/** The first instant of the month in UTC, which is when a monthly budget starts over. */
export const monthStart = (now: Date): Date => new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));

export interface Named { name: string; label: string }

/** The attestation names the server's documentation uses; an operator may also write their own. */
export const ATTESTATIONS: Named[] = [
  { name: 'soc2-type2', label: 'SOC 2 Type II' },
  { name: 'hipaa-baa', label: 'HIPAA BAA' },
  { name: 'iso27001', label: 'ISO 27001' },
  { name: 'gdpr-dpa', label: 'GDPR DPA' },
];

/**
 * What a provider's own pages state, as attestations an operator could record
 * for a pool on it. This is the provider's claim: record one only after seeing
 * the report or the signed agreement.
 */
export function stated(p: Provider | undefined): Named[] {
  if (!p) return [];
  const has = [p.soc2 === 'yes', p.hipaa === 'yes', p.iso === 'yes'];
  return ATTESTATIONS.slice(0, 3).filter((_, i) => has[i]);
}

/** Whether a recorded attestation still counts. */
export function attestationValid(a: Pick<Attestation, 'validUntil'>, now: Date): boolean {
  if (!a.validUntil || a.validUntil.startsWith('0001-')) return true;
  return new Date(a.validUntil).getTime() > now.getTime();
}

/** A provider in a few words, for a card. */
export function tags(p: Provider): string[] {
  return [
    p.caps.kernel === 'yes' && 'Own kernel',
    p.caps.memory === 'yes' && 'Keeps memory',
    p.caps.fork === 'yes' && 'Forks',
    p.caps.url === 'yes' && 'Port URLs',
    p.caps.own === 'yes' && 'Your infrastructure',
    p.billing === 'usage' && 'Bills CPU used',
  ].filter(Boolean) as string[];
}

export type AddHow = 'here' | 'plugin' | 'cli' | 'none';

/**
 * How a provider can be connected from this console. `kinds` are the adapter
 * kinds its Add form knows: those in the server's default build.
 */
export function addRoute(p: Provider, kinds: string[]): { how: AddHow; text: string } {
  if (p.adapter && kinds.includes(p.adapter)) return { how: 'here', text: 'Add it here.' };
  if (p.adapter) return { how: 'cli', text: 'Its adapter is experimental: it needs a server built with experimental adapters, and is added with the CLI or the config file.' };
  if (p.support === 'plugin') return { how: 'plugin', text: 'Runs through a remote plugin that you deploy.' };
  return { how: 'none', text: 'No adapter yet.' };
}

export type KindKey = 'hosted' | 'cloud' | 'open';
export const kindKey = (p: Provider): KindKey => (p.kind === 'Hosted service' ? 'hosted' : p.kind === 'Open-source runtime' ? 'open' : 'cloud');

/** Catalog entries matching a kind and a search, in catalog order. */
export function search(kind: KindKey | 'all', query: string): Provider[] {
  const q = query.trim().toLowerCase();
  return providers.filter((p) => (kind === 'all' || kindKey(p) === kind) && (!q || `${p.name} ${p.id} ${p.isolation} ${p.kind}`.toLowerCase().includes(q)));
}
