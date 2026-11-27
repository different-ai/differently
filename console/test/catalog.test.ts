// The provider catalog and budget model, checked without a browser.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  ATTESTATIONS, addRoute, attestationValid, budgetView, costText, entryFor, hasPrices, listCost, monthStart,
  priceState, providers, rate, sameCost, search, stated, tags, totalsOf, usd,
} from '../src/lib/catalog.ts';

const pool = (over: Record<string, unknown>) =>
  ({ name: 'p', adapter: 'daytona', trust: 'external', enabled: true, draining: false, costModel: {}, source: 'api', active: 0, ...over }) as never;
const entry = (id: string) => providers.find((p) => p.id === id)!;

test('a pool finds its catalog entry by adapter, then by label, then by name', () => {
  assert.equal(entryFor(pool({ adapter: 'daytona', name: 'eu-burst' }))?.id, 'daytona');
  assert.equal(entryFor(pool({ adapter: 'agentcore', name: 'aws' }))?.id, 'aws-agentcore');
  // A remote plugin serves whichever provider its operator says it does.
  assert.equal(entryFor(pool({ adapter: 'remote', name: 'gpu', labels: { provider: 'modal' } }))?.id, 'modal');
  assert.equal(entryFor(pool({ adapter: 'remote', name: 'cloudflare' }))?.id, 'cloudflare');
  assert.equal(entryFor(pool({ adapter: 'remote', name: 'in-house' })), undefined);
  // The adapter wins over a misleading name.
  assert.equal(entryFor(pool({ adapter: 'freestyle', name: 'daytona' }))?.id, 'freestyle');
});

test('every adapter name in the catalog is unique', () => {
  const adapters = providers.map((p) => p.adapter).filter(Boolean);
  assert.equal(new Set(adapters).size, adapters.length);
});

test('list rates add up to the hourly figure the comparison page shows', () => {
  for (const p of providers) {
    if (!p.rates) continue;
    assert.ok(p.slot, `${p.id} has rates but no hourly figure`);
    const hour = (p.rates.sandboxHour ?? 0) + (p.rates.cpuHour ?? 0) + 2 * (p.rates.memoryGiBHour ?? 0);
    const shown = p.slot[p.slot.length - 1];
    assert.ok(Math.abs(hour - shown) < 0.0002, `${p.id}: rates give ${hour}, the page shows ${shown}`);
    // What a provider meters matches how the page says it bills.
    assert.equal(p.rates.basis === 'used', p.billing === 'usage', `${p.id}: basis ${p.rates.basis} with billing ${p.billing}`);
  }
});

test('list prices become a cost model, and a pool knows whether it charges them', () => {
  assert.deepEqual(listCost(entry('daytona')), { currency: 'USD', cpuHour: 0.0504, memoryGiBHour: 0.0162 });
  assert.deepEqual(listCost(entry('boat')), { currency: 'USD', sandboxHour: 0.018 });
  assert.equal(listCost(entry('opensandbox')), undefined);
  assert.equal(listCost(undefined), undefined);

  const daytona = entry('daytona');
  assert.equal(priceState(pool({ costModel: {} }), daytona), 'none');
  assert.equal(priceState(pool({ costModel: { cpuHour: 0.0504, memoryGiBHour: 0.0162 } }), daytona), 'list');
  assert.equal(priceState(pool({ costModel: { cpuHour: 0.04, memoryGiBHour: 0.0162 } }), daytona), 'custom');
  // A pool with prices on a provider that has no list price is on its own prices.
  assert.equal(priceState(pool({ costModel: { cpuHour: 0.02 } }), entry('opensandbox')), 'custom');
  assert.equal(priceState(pool({ costModel: { cpuHour: 0.02 } }), undefined), 'custom');

  assert.deepEqual([hasPrices(undefined), hasPrices({}), hasPrices({ currency: 'USD' }), hasPrices({ diskGiBHour: 0.001 })], [false, false, false, true]);
  assert.equal(sameCost({ cpuHour: 0.05 }, { cpuHour: 0.05, memoryGiBHour: 0 }), true);
});

test('money and rates read naturally', () => {
  assert.deepEqual([rate(0.05), rate(0.0162), rate(0.021875), rate(1), rate(0.1)], ['$0.05', '$0.0162', '$0.021875', '$1.00', '$0.10']);
  assert.deepEqual([usd(0), usd(12.345), usd(99.994), usd(100), usd(1234.5)], ['$0.00', '$12.35', '$99.99', '$100', '$1,235']);
  // A fraction of a cent is not shown as nothing.
  assert.deepEqual([usd(0.004), usd(0.005), usd(0.0001)], ['<$0.01', '$0.01', '<$0.01']);
  assert.equal(costText({ cpuHour: 0.0504, memoryGiBHour: 0.0162 }), '$0.0504 / vCPU-h + $0.0162 / GiB-h');
  assert.equal(costText({ sandboxHour: 0.018, diskGiBHour: 0.0001 }), '$0.018 / sandbox-h + $0.0001 / GiB disk-h');
  assert.equal(costText({}), '');
  assert.equal(costText(undefined), '');
});

test('a budget is none, ok, near or reached', () => {
  const view = (over: Record<string, unknown>, usage?: number) => budgetView(pool(over), usage);
  assert.deepEqual(view({}), { spent: 0, share: 0, state: 'none', text: 'No spend this month' });
  // Without a budget the server reports no spend; the usage report fills it in.
  assert.deepEqual(view({}, 4.2), { spent: 4.2, share: 0, state: 'none', text: '$4.20 this month' });
  assert.deepEqual(view({ budget: { monthly: 100 }, spend: { month: '2026-11', estimated: 25, budgetReached: false } }), { spent: 25, limit: 100, share: 0.25, state: 'ok', text: '$25.00 of $100' });
  assert.equal(view({ budget: { monthly: 100 }, spend: { month: '2026-11', estimated: 80, budgetReached: false } }).state, 'near');
  const over = view({ budget: { monthly: 100 }, spend: { month: '2026-11', estimated: 130, budgetReached: true } });
  assert.deepEqual([over.state, over.share, over.text], ['reached', 1, '$130 of $100']);
  // The server's word is final, even if the figures have not caught up.
  assert.equal(view({ budget: { monthly: 100 }, spend: { month: '2026-11', estimated: 99, budgetReached: true } }).state, 'reached');
  // The server's spend is used before the usage report's.
  assert.equal(view({ budget: { monthly: 50 }, spend: { month: '2026-11', estimated: 10, budgetReached: false } }, 99).spent, 10);
});

test('totals count spend, budgets and the pools that need attention', () => {
  const pools = [
    pool({ name: 'osb', adapter: 'opensandbox', trust: 'private' }),
    pool({ name: 'daytona', costModel: { cpuHour: 0.05 }, budget: { monthly: 100 }, spend: { month: '2026-11', estimated: 100, budgetReached: true } }),
    pool({ name: 'freestyle', adapter: 'freestyle', costModel: { cpuHour: 0.04 } }),
    pool({ name: 'e2b', adapter: 'e2b', costModel: { cpuHour: 0.05 }, budget: { monthly: 40 }, spend: { month: '2026-11', estimated: 5, budgetReached: false } }),
  ];
  assert.deepEqual(totalsOf(pools as never, { freestyle: 12.5 }), { spent: 117.5, budgeted: 140, withBudget: 2, reached: 1, unpriced: 1, externalOpen: 1 });
  assert.deepEqual(totalsOf([]), { spent: 0, budgeted: 0, withBudget: 0, reached: 0, unpriced: 0, externalOpen: 0 });
});

test('the month starts at midnight UTC on the first', () => {
  assert.equal(monthStart(new Date('2026-11-17T22:10:00Z')).toISOString(), '2026-11-01T00:00:00.000Z');
  // Late on the last day in a western time zone is already next month in UTC.
  assert.equal(monthStart(new Date('2026-11-30T23:30:00-08:00')).toISOString(), '2026-12-01T00:00:00.000Z');
});

test('stated attestations are what the provider says, by the names the server documents', () => {
  assert.deepEqual(stated(entry('vercel')).map((a) => a.name), ['soc2-type2', 'hipaa-baa', 'iso27001']);
  assert.deepEqual(stated(entry('northflank')).map((a) => a.name), ['soc2-type2']);
  // "Unconfirmed", "not found" and "your own scope" are not statements to record.
  assert.deepEqual(stated(entry('morph')), []);
  assert.deepEqual(stated(entry('opensandbox')), []);
  assert.deepEqual(stated(undefined), []);
  assert.deepEqual(ATTESTATIONS.map((a) => a.name), ['soc2-type2', 'hipaa-baa', 'iso27001', 'gdpr-dpa']);

  const now = new Date('2026-11-17T00:00:00Z');
  assert.deepEqual([attestationValid({}, now), attestationValid({ validUntil: '0001-01-01T00:00:00Z' }, now), attestationValid({ validUntil: '2027-06-30T00:00:00Z' }, now), attestationValid({ validUntil: '2026-06-30T00:00:00Z' }, now)], [true, true, true, false]);
});

test('a provider is added here, through a plugin, with the CLI, or not yet', () => {
  const kinds = ['daytona', 'freestyle', 'opensandbox', 'remote'];
  assert.equal(addRoute(entry('daytona'), kinds).how, 'here');
  assert.equal(addRoute(entry('e2b'), kinds).how, 'cli');
  assert.equal(addRoute(entry('modal'), kinds).how, 'plugin');
  assert.equal(addRoute(entry('novita'), kinds).how, 'none');
  // A server built with experimental adapters can add E2B from the form too.
  assert.equal(addRoute(entry('e2b'), [...kinds, 'e2b']).how, 'here');
});

test('the catalog filters by kind and by what is typed', () => {
  assert.equal(search('all', '').length, providers.length);
  assert.deepEqual(search('open', '').map((p) => p.id), ['opensandbox', 'agent-sandbox', 'cubesandbox', 'gke-sandboxes']);
  assert.deepEqual(search('all', 'azure').map((p) => p.id), ['azure-sandboxes', 'azure-sessions']);
  assert.deepEqual(search('hosted', '  FIRECRACKER ').map((p) => p.id), ['e2b', 'groundbox', 'vercel', 'fly', 'cloudflare', 'codesandbox', 'tensorlake']);
  assert.deepEqual(search('cloud', 'daytona'), []);
  assert.deepEqual(tags(entry('e2b')), ['Own kernel', 'Keeps memory', 'Forks', 'Port URLs', 'Your infrastructure']);
});
