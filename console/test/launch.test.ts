// The sandbox launcher's model, checked without a browser.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clampCount, hourlyCost, launchRow, planOf, runLimited, summarize, totalOf } from '../src/lib/launch.ts';
import { gateOf } from '../src/lib/routing.ts';

const pool = (over: Record<string, unknown>) =>
  ({ name: 'p', adapter: 'daytona', trust: 'external', region: 'eu', enabled: true, draining: false, costModel: {}, source: 'api', active: 0, health: { status: 'healthy' }, ...over }) as never;
const open = gateOf([{ scope: 'global', external: 'overflow', externalPools: ['daytona'], updatedAt: '' }]);

test('cost follows the pool prices and the sandbox size', () => {
  const size = { cpu: 2, memoryMiB: 4096, diskGiB: 10 };
  const near = (got: number, want: number) => assert.ok(Math.abs(got - want) < 1e-9, `${got} is not ${want}`);
  near(hourlyCost({ cpuHour: 0.05, memoryGiBHour: 0.0125 }, size), 0.15);
  near(hourlyCost({ sandboxHour: 1, diskGiBHour: 0.01 }, size), 1.1);
  assert.equal(hourlyCost(undefined, size), 0);
  assert.equal(hourlyCost({ cpuHour: 1 }, undefined), 0);
});

test('a pool is offered only where a pinned sandbox could be placed', () => {
  const row = (over: Record<string, unknown>, gate = open) => launchRow(pool(over), gate, { cpu: 1, memoryMiB: 1024, diskGiB: 0 });
  const daytona = row({ name: 'daytona', maxConcurrency: 8, active: 3, costModel: { cpuHour: 0.05 } });
  assert.deepEqual([daytona.open, daytona.note, daytona.free, daytona.hourly, daytona.sub], [true, '5 free slots', 5, 0.05, 'external · eu · daytona']);
  assert.deepEqual([row({ name: 'freestyle' }).open, row({ name: 'freestyle' }).note], [false, 'not approved by the global policy']);
  assert.equal(row({ name: 'daytona' }, gateOf([])).note, 'external placement is off in the global policy');
  // Your own pools need no approval, whatever the gate says.
  assert.equal(row({ name: 'osb', trust: 'private' }, gateOf([])).open, true);
  assert.deepEqual([row({ name: 'daytona', enabled: false }).open, row({ name: 'daytona', draining: true }).open], [false, false]);
  // Full, over budget or unhealthy pools still accept the request; the sandboxes wait.
  assert.deepEqual([row({ name: 'daytona', maxConcurrency: 2, active: 2 }).open, row({ name: 'daytona', maxConcurrency: 2, active: 2 }).note], [true, 'full: new sandboxes wait for a slot']);
  assert.equal(row({ name: 'daytona', spend: { month: '2026-11', estimated: 9, budgetReached: true } }).note, 'budget reached: new sandboxes wait');
  assert.equal(row({ name: 'daytona', health: { status: 'unavailable' } }).note, 'unavailable: sandboxes wait until it recovers');
  assert.equal(row({ name: 'daytona', maxConcurrency: 4, active: 3 }).note, '1 free slot');
});

test('counts are whole numbers within the limit, and the plan interleaves pools', () => {
  assert.deepEqual([clampCount('7'), clampCount(-3), clampCount('x'), clampCount(2.9), clampCount(999)], [7, 0, 0, 2, 50]);
  const counts = { freestyle: 2, daytona: 4, off: 0, x: 1 };
  assert.equal(totalOf(counts), 7);
  assert.deepEqual(planOf(counts), ['freestyle', 'daytona', 'x', 'freestyle', 'daytona', 'daytona', 'daytona']);
  assert.deepEqual(planOf({}), []);
});

test('outcomes are grouped by pool with each refusal reason once', () => {
  const s = summarize([
    { pool: 'daytona', id: 'sbx_1' }, { pool: 'daytona', id: 'sbx_2' },
    { pool: 'freestyle', error: 'quota exceeded' }, { pool: 'freestyle', error: 'quota exceeded' }, { pool: 'freestyle', id: 'sbx_3' },
  ]);
  assert.deepEqual([s.requested, s.created, s.refused], [5, 3, 2]);
  assert.deepEqual(s.pools, [
    { pool: 'daytona', created: 2, refused: 0, reasons: [] },
    { pool: 'freestyle', created: 1, refused: 2, reasons: ['quota exceeded'] },
  ]);
});

test('requests run a few at a time and keep their order', async () => {
  let inFlight = 0;
  let peak = 0;
  const seen: number[] = [];
  const tasks = Array.from({ length: 9 }, (_, i) => async () => {
    peak = Math.max(peak, ++inFlight);
    await new Promise((r) => setTimeout(r, 5 - (i % 3)));
    inFlight--;
    return i * 2;
  });
  const out = await runLimited(tasks, 3, (done) => seen.push(done));
  assert.deepEqual(out, [0, 2, 4, 6, 8, 10, 12, 14, 16]);
  assert.equal(peak, 3);
  assert.deepEqual(seen, [1, 2, 3, 4, 5, 6, 7, 8, 9]);
  assert.deepEqual(await runLimited([], 3), []);
});
