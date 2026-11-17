// The live routing view's model, checked without a browser.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { along, flowOf, gateOf, hcurve, poolView, shortId, sortPools, statsOf } from '../src/lib/routing.ts';

const event = (seq: number, action: string, details: Record<string, string> = {}, time = '2026-11-16T12:00:00Z') =>
  ({ seq, time, actor: 'scheduler', action, resource: 'sbx_01m7khkzawmedjvh49vwcr6maz', outcome: 'success', details, prevHash: '', hash: '' }) as never;

test('audit events become routing events, and only routing ones', () => {
  const placed = flowOf(event(1, 'sandbox.placed', { pool: 'daytona', trust: 'external', strategy: 'private-first', external: 'overflow' }));
  assert.deepEqual({ kind: placed?.kind, pool: placed?.pool, note: placed?.note }, { kind: 'placed', pool: 'daytona', note: 'private-first · external overflow' });
  assert.equal(flowOf(event(2, 'sandbox.rejected', { reason: 'no permitted pool holds the required attestations' }))?.note, 'no permitted pool holds the required attestations');
  assert.equal(flowOf(event(3, 'sandbox.terminated', { pool: 'fc', reason: 'ttl expired' }))?.kind, 'ended');
  // "expired" only starts a termination, so it would double every ending.
  assert.equal(flowOf(event(4, 'sandbox.expired')), null);
  assert.equal(flowOf(event(5, 'pool.update')), null);
});

test('stats count the window and the last minute', () => {
  const now = Date.parse('2026-11-16T12:10:00Z');
  const at = (secondsAgo: number) => new Date(now - secondsAgo * 1000).toISOString();
  const flow = (kind: string, secondsAgo: number, pool?: string, trust?: string) => ({ seq: 0, time: at(secondsAgo), kind, sandbox: 'sbx_x', pool, trust, note: '' }) as never;
  const s = statsOf([
    flow('placed', 10, 'fc', 'private'), flow('placed', 30, 'daytona', 'external'), flow('placed', 200, 'fc', 'private'),
    flow('placed', 900, 'fc', 'private'), // outside the five minutes
    flow('rejected', 20), flow('failed', 40), flow('created', 5),
  ], now);
  assert.deepEqual(s, { perMinute: 2, placed: 3, external: 1, refused: 1, failed: 1, perPool: { fc: 2, daytona: 1 } });
});

test('the gate is closed unless the global policy opens it', () => {
  assert.equal(gateOf([]).mode, 'never');
  assert.equal(gateOf([{ scope: 'org:o1', external: 'allowed', updatedAt: '' }]).mode, 'never');
  const g = gateOf([{ scope: 'global', external: 'overflow', externalPools: ['daytona'], updatedAt: '' }]);
  assert.deepEqual([g.mode, g.approved], ['overflow', ['daytona']]);
});

test('a pool view shows load, reachability, budget and current attestations', () => {
  const gate = gateOf([{ scope: 'global', external: 'overflow', externalPools: ['daytona'], updatedAt: '' }]);
  const base = { adapter: 'daytona', trust: 'external', region: 'eu', enabled: true, draining: false, costModel: {}, source: 'api', health: { status: 'healthy' } };
  const now = Date.parse('2026-11-16T12:00:00Z');
  const v = poolView({ ...base, name: 'daytona', active: 9, maxConcurrency: 8, budget: { monthly: 500 }, spend: { month: '2026-11', estimated: 500.4, budgetReached: true },
    attestations: { 'soc2-type2': { validUntil: '2027-01-01T00:00:00Z' }, 'hipaa-baa': { validUntil: '2026-01-01T00:00:00Z' }, iso27001: {} } } as never, gate, now);
  assert.deepEqual([v.reachable, v.state, v.stateLabel, v.slots, v.filled, v.more, v.load], [true, 'warn', 'budget reached', 8, 8, 1, '9 / 8 slots']);
  assert.deepEqual(v.attestations, ['iso27001', 'soc2-type2']); // the expired one is not held
  assert.deepEqual(v.budget, { spent: 500.4, limit: 500, reached: true });
  // An external pool the policy does not name is not reachable, whatever the gate says.
  assert.equal(poolView({ ...base, name: 'freestyle', active: 0 } as never, gate, now).reachable, false);
  // Many sandboxes without a cap: a fixed row of slots plus a count.
  const big = poolView({ ...base, name: 'osb', trust: 'private', active: 40 } as never, gate, now);
  assert.deepEqual([big.slots, big.filled, big.more, big.load, big.reachable], [12, 12, 28, '40 running', true]);
  assert.equal(poolView({ ...base, name: 'off', enabled: false, active: 0 } as never, gate, now).state, 'off');
});

test('pools sort private first, ids shorten, curves start and end on their anchors', () => {
  const names = sortPools([{ name: 'z', trust: 'external' }, { name: 'b', trust: 'private' }, { name: 'a', trust: 'private' }] as never).map((p) => p.name);
  assert.deepEqual(names, ['a', 'b', 'z']);
  assert.equal(shortId('sbx_01m7khkzawmedjvh49vwcr6maz'), 'sbx_…r6maz');
  assert.equal(shortId('short'), 'short');
  const c = hcurve({ x: 0, y: 0 }, { x: 100, y: 50 });
  assert.deepEqual([along(c, 0), along(c, 1)], [{ x: 0, y: 0 }, { x: 100, y: 50 }]);
});
