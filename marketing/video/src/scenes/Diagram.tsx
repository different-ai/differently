import React from 'react';
import {AbsoluteFill, Easing, interpolate, interpolateColors, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, handoff, Mark, SNAP} from '../ui';
import {Padlock} from './Problem';

const d = tokens.dark;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN = Easing.bezier(0.7, 0, 0.84, 0);

type XY = {x: number; y: number};
type Box = XY & {w: number; h: number};
type Curve = {a: XY; c1: XY; c2: XY; b: XY};
type PoolSpec = Box & {title: string; sub: string; external: boolean; cap: number};

/** Horizontal-first cubic between two points. */
const hcurve = (a: XY, b: XY): Curve => ({a, c1: {x: (a.x + b.x) / 2, y: a.y}, c2: {x: (a.x + b.x) / 2, y: b.y}, b});
/** Vertical-first cubic between two points. */
const vcurve = (a: XY, b: XY): Curve => ({a, c1: {x: a.x, y: (a.y + b.y) / 2}, c2: {x: b.x, y: (a.y + b.y) / 2}, b});
/** Drops straight down from a, then turns towards b (an elbow). */
const elbow = (a: XY, b: XY): Curve => ({a, c1: {x: a.x, y: b.y}, c2: {x: (a.x + b.x) / 2, y: b.y}, b});
const pathOf = (c: Curve) => `M ${c.a.x} ${c.a.y} C ${c.c1.x} ${c.c1.y}, ${c.c2.x} ${c.c2.y}, ${c.b.x} ${c.b.y}`;
const along = (c: Curve, t: number): XY => {
  const m = 1 - t;
  return {
    x: m * m * m * c.a.x + 3 * m * m * t * c.c1.x + 3 * m * t * t * c.c2.x + t * t * t * c.b.x,
    y: m * m * m * c.a.y + 3 * m * m * t * c.c1.y + 3 * m * t * t * c.c2.y + t * t * t * c.b.y,
  };
};

const layout = (sq: boolean) => {
  const app: Box = sq ? {x: 175, y: 290, w: 260, h: 130} : {x: 220, y: 470, w: 290, h: 150};
  const server: Box = sq ? {x: 600, y: 290, w: 440, h: 150} : {x: 700, y: 470, w: 470, h: 170};
  const pg: XY = sq ? {x: 960, y: 290} : {x: 700, y: 690};
  const pools: PoolSpec[] = sq
    ? [
        {x: 285, y: 505, w: 450, h: 150, title: 'your microVMs', sub: 'Firecracker · private', external: false, cap: 3},
        {x: 795, y: 505, w: 450, h: 150, title: 'OpenSandbox', sub: 'self-hosted · private', external: false, cap: 3},
        {x: 300, y: 880, w: 400, h: 110, title: 'Daytona', sub: 'external · opt-in', external: true, cap: 3},
        {x: 780, y: 880, w: 400, h: 110, title: 'Freestyle', sub: 'external · opt-in', external: true, cap: 3},
      ]
    : [
        {x: 1470, y: 290, w: 470, h: 150, title: 'your microVMs', sub: 'Firecracker · private', external: false, cap: 4},
        {x: 1470, y: 460, w: 470, h: 150, title: 'OpenSandbox', sub: 'self-hosted · private', external: false, cap: 4},
        {x: 1500, y: 710, w: 410, h: 112, title: 'Daytona', sub: 'external · opt-in', external: true, cap: 3},
        {x: 1500, y: 855, w: 410, h: 112, title: 'Freestyle', sub: 'external · opt-in', external: true, cap: 3},
      ];
  const gate: XY = sq ? {x: 540, y: 690} : {x: 1130, y: 782};
  const chip: XY = sq ? {x: 815, y: 690} : {x: 1130, y: 905};
  const appR = {x: app.x + app.w / 2, y: app.y};
  const serverL = {x: server.x - server.w / 2, y: server.y};
  const serverR = {x: server.x + server.w / 2, y: server.y};
  const serverB = {x: server.x, y: server.y + server.h / 2};
  const toApp = hcurve(appR, serverL);
  const toPool = pools.map((p) =>
    sq
      ? vcurve({x: server.x + (p.x > server.x ? 130 : -170), y: server.y + server.h / 2}, {x: p.x, y: p.y - p.h / 2})
      : hcurve(serverR, {x: p.x - p.w / 2, y: p.y}),
  );
  const gateIn = sq ? {x: gate.x, y: gate.y - 62} : {x: gate.x - 70, y: gate.y};
  const gateOut = sq ? {x: gate.x, y: gate.y + 62} : {x: gate.x + 70, y: gate.y};
  const toGate = sq ? vcurve({x: gate.x, y: server.y + server.h / 2}, gateIn) : elbow({x: server.x + 130, y: server.y + server.h / 2}, gateIn);
  const fromGate = pools.map((p) => (sq ? vcurve(gateOut, {x: p.x, y: p.y - p.h / 2}) : hcurve(gateOut, {x: p.x - p.w / 2, y: p.y})));
  const toPg = sq ? hcurve(serverR, {x: pg.x - 70, y: pg.y}) : vcurve(serverB, {x: pg.x, y: pg.y - 48});
  return {app, server, pg, pools, gate, chip, toApp, toPool, toGate, fromGate, toPg, textW: sq ? 960 : 1660};
};

const usePop = (at: number, config = SNAP) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  return spring({frame: frame - at, fps, config});
};

const draw = (frame: number, at: number, len = 16) => interpolate(frame, [at, at + len], [0, 1], {...clamp, easing: OUT});

// When each sandbox lands in a pool (beats). Private pools fill to quota first; the
// external pools only fill after the policy flips to overflow.
const fillAt = (pool: number, k: number) => (pool < 2 ? 0.9 + (k * 2 + pool) * 0.38 : 8.7 + (k * 2 + (pool - 2)) * 0.35);

const Pool: React.FC<{p: PoolSpec; i: number; sq: boolean; open: number}> = ({p, i, sq, open}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const pop = usePop(beat(p.external ? 4.7 + (i - 2) * 0.2 : 0.55 + i * 0.25));
  const filled = Array.from({length: p.cap}).filter((_, k) => frame >= beat(fillAt(i, k))).length;
  const full = filled >= p.cap;
  const dim = p.external ? 0.45 + 0.55 * open : 1;
  return (
    <div
      style={{
        position: 'absolute',
        left: p.x - p.w / 2,
        top: p.y - p.h / 2,
        width: p.w,
        height: p.h,
        border: `2.5px ${p.external ? 'dashed' : 'solid'} ${d.ink}`,
        borderRadius: 12,
        background: p.external ? 'transparent' : d.surface,
        transform: `scale(${pop})`,
        opacity: Math.min(1, pop * 2) * dim,
        boxSizing: 'border-box',
        padding: sq ? '12px 16px' : '12px 18px',
        display: 'flex',
        flexDirection: 'column',
        justifyContent: 'space-between',
      }}
    >
      <div style={{display: 'flex', alignItems: 'baseline', gap: 10, whiteSpace: 'nowrap'}}>
        <span style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 23 : 26, color: d.ink, letterSpacing: '-0.02em'}}>{p.title}</span>
        <span style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 16 : 18, color: d.muted}}>{p.sub}</span>
      </div>
      <div style={{display: 'flex', gap: 10, alignItems: 'center'}}>
        {Array.from({length: p.cap}).map((_, k) => {
          const c = spring({frame: frame - beat(fillAt(i, k)), fps, config: {damping: 11, stiffness: 260, mass: 0.5}});
          return (
            <div key={k} style={{width: sq ? 70 : 78, height: p.external ? 34 : 42, borderRadius: 8, border: `2px solid ${d.line}`, position: 'relative', boxSizing: 'border-box'}}>
              <div style={{position: 'absolute', inset: -2, borderRadius: 8, background: d.ink, transform: `scale(${c})`, display: 'flex', alignItems: 'center', justifyContent: 'center', fontFamily: mono, fontSize: sq ? 14 : 16, color: d.bg}}>
                sbx
              </div>
            </div>
          );
        })}
        {!p.external ? (
          <div style={{marginLeft: 'auto', textAlign: 'right'}}>
            <div style={{fontFamily: mono, fontSize: sq ? 14 : 16, color: full ? d.ink : d.muted, fontWeight: full ? 500 : 400}}>{full ? 'quota full' : `quota ${filled}/${p.cap}`}</div>
            <div style={{width: sq ? 90 : 110, height: 6, borderRadius: 3, background: d.line, marginTop: 6, overflow: 'hidden'}}>
              <div style={{width: `${(filled / p.cap) * 100}%`, height: '100%', background: d.ink}} />
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
};

const AUDIT = [
  {at: 10.7, text: 'placed    sbx_7f2  → microVMs   private-first'},
  {at: 11.1, text: 'rejected  sbx_9c1    no_eligible_pool'},
  {at: 11.5, text: 'placed    sbx_3ad  → daytona    overflow'},
];

const BADGES = [
  {t: 'quotas', at: 2.5},
  {t: 'audit', at: 10.4},
  {t: 'trust report', at: 11.8},
];

/**
 * Orchestration: one API in front of your own pools (microVMs, OpenSandbox), quotas that
 * fill up, external providers behind a fail-closed gate that opens only when policy says
 * overflow, and an audit trail for every placement.
 */
export const Diagram: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const L = layout(sq);
  const appPop = usePop(0);
  const serverPop = usePop(beat(0.3));
  const pgPop = usePop(beat(0.5));

  const gatePop = usePop(beat(4.6));
  const open = interpolate(frame, [beat(8.2), beat(8.6)], [0, 1], {...clamp, easing: OUT});
  const deniedAt = beat(6.2);
  const stamp = usePop(deniedAt, {damping: 9, stiffness: 300, mass: 0.5});
  const stampGone = interpolate(frame, [beat(7.3), beat(7.6)], [0, 1], clamp);
  const flipAt = beat(7.5);
  const flip = frame >= flipAt + 14;
  const chipText = flip ? 'external: "overflow"' : 'external: "never"';
  const chipTyped =
    frame < flipAt
      ? chipText.length
      : frame < flipAt + 14
        ? Math.round(interpolate(frame, [flipAt, flipAt + 14], [chipText.length, 10]))
        : Math.round(interpolate(frame, [flipAt + 14, flipAt + 28], [10, chipText.length], clamp));
  const badge = (at: number) => spring({frame: frame - beat(at), fps: 60, config: {damping: 11, stiffness: 260, mass: 0.5}});
  const auditIn = interpolate(frame, [beat(10.3), beat(10.8)], [0, 1], {...clamp, easing: OUT});

  // Hand-off into the p50 scene: the server box's light fill grows to cover the screen
  // while its mark glides to the centre, exactly where the next scene picks it up.
  const handT = interpolate(frame, [beat(13), beat(15) - 1], [0, 1], {...clamp, easing: Easing.bezier(0.65, 0, 0.35, 1)});
  const handing = frame >= beat(13);
  const markSize = sq ? 52 : 58;
  const markCx = L.server.x - L.server.w / 2 + (sq ? 28 : 32) + markSize / 2;
  const drift = 1 + Math.min(frame, beat(13)) * 0.00006;
  const H = handoff(width, height);
  const lerp = (a: number, b: number) => a + (b - a) * handT;
  const box = {
    w: lerp(L.server.w * drift, width + 4),
    h: lerp(L.server.h * drift, height + 4),
    cx: lerp(L.server.x, width / 2),
    cy: lerp(L.server.y, height / 2),
    r: lerp(14 * drift, 0),
  };
  const mk = {x: lerp(L.server.x + (markCx - L.server.x) * drift, H.x), y: lerp(L.server.y, H.y), size: lerp(markSize * drift, H.size)};

  // Packets: each sandbox travels app → server → the pool that takes it, landing as its chip pops.
  const packets: React.ReactNode[] = [];
  const dot = (key: string, p: XY, r = 7) => packets.push(<circle key={key} cx={p.x} cy={p.y} r={r} fill={d.ink} />);
  L.pools.forEach((pool, i) => {
    for (let k = 0; k < pool.cap; k++) {
      const land = beat(fillAt(i, k));
      const t0 = land - 26;
      if (frame < t0 || frame >= land) continue;
      const q = (frame - t0) / 26;
      if (i < 2) dot(`p${i}${k}`, q < 0.45 ? along(L.toApp, q / 0.45) : along(L.toPool[i], (q - 0.45) / 0.55));
      else dot(`p${i}${k}`, q < 0.5 ? along(L.toGate, q / 0.5) : along(L.fromGate[i], (q - 0.5) / 0.5));
    }
  });
  if (frame >= beat(5.6) && frame < beat(6.9)) {
    const go = interpolate(frame, [beat(5.6), deniedAt], [0, 1], {...clamp, easing: IN});
    const back = interpolate(frame, [deniedAt, beat(6.9)], [0, 1], {...clamp, easing: OUT});
    dot('denied', along(L.toGate, go * (1 - back * 0.5)), 10);
  }

  const wireStroke = (c: Curve, p: number, key: string, color: string = d.ink, dashed = false, opacity = 1) => (
    <path
      key={key}
      d={pathOf(c)}
      stroke={color}
      strokeWidth={2.5}
      fill="none"
      pathLength={1}
      strokeDasharray={dashed ? '0.012 0.02' : '1 1'}
      strokeDashoffset={dashed ? 0 : 1 - p}
      opacity={dashed ? opacity * p : opacity}
      strokeLinecap="round"
    />
  );

  return (
    <AbsoluteFill style={{background: d.bg, overflow: 'hidden'}}>
      <AbsoluteFill style={{transform: `scale(${drift * (1 + 0.1 * handT)})`, transformOrigin: `${L.server.x}px ${L.server.y}px`, opacity: 1 - handT}}>
        <svg width={width} height={height} style={{position: 'absolute', inset: 0}}>
          {wireStroke(L.toApp, draw(frame, beat(0.2)), 'w-app')}
          {wireStroke(L.toPg, draw(frame, beat(0.5)), 'w-pg', d.muted)}
          {L.toPool.slice(0, 2).map((c, i) => wireStroke(c, draw(frame, beat(0.5 + i * 0.25)), `w-pool${i}`))}
          {frame >= beat(4.6) ? (
            <>
              {wireStroke(L.toGate, draw(frame, beat(4.6), 20), 'w-gate')}
              {L.fromGate.slice(2).map((c, i) => wireStroke(c, draw(frame, beat(4.8 + i * 0.2)), `w-ext${i}`, d.ink, true, 0.35 + 0.65 * open))}
            </>
          ) : null}
          {packets}
        </svg>

        {/* app */}
        <div style={{position: 'absolute', left: L.app.x - L.app.w / 2, top: L.app.y - L.app.h / 2, width: L.app.w, height: L.app.h, border: `2.5px solid ${d.ink}`, borderRadius: 12, background: d.surface, boxSizing: 'border-box', display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', transform: `scale(${appPop})`, opacity: Math.min(1, appPop * 2)}}>
          <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 32 : 36, color: d.ink, letterSpacing: '-0.025em'}}>your app</div>
          <div style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 19 : 21, color: d.muted, marginTop: 6}}>TS · Python · Go · CLI</div>
        </div>

        {/* server */}
        <div style={{position: 'absolute', left: L.server.x - L.server.w / 2, top: L.server.y - L.server.h / 2, width: L.server.w, height: L.server.h, borderRadius: 14, background: d.ink, boxSizing: 'border-box', display: 'flex', alignItems: 'center', gap: 22, padding: sq ? '0 28px' : '0 32px', transform: `scale(${serverPop})`, opacity: handing ? 0 : Math.min(1, serverPop * 2)}}>
          <Mark size={markSize} color={d.bg} />
          <div>
            <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 30 : 36, color: d.bg, letterSpacing: '-0.03em', whiteSpace: 'nowrap'}}>differently-server</div>
            <div style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 20 : 23, color: tokens.light.muted, marginTop: 4}}>one API · policy · placement</div>
          </div>
        </div>
        <div style={{position: 'absolute', left: L.server.x, top: L.server.y - L.server.h / 2 - (sq ? 42 : 52), transform: 'translateX(-50%)', display: 'flex', gap: 12}}>
          {BADGES.map((b) => {
            const p = badge(b.at);
            return (
              <div key={b.t} style={{transform: `scale(${p})`, opacity: Math.min(1, p * 2), border: `2px solid ${d.ink}`, borderRadius: 999, padding: sq ? '5px 14px' : '6px 16px', fontFamily: inter, fontWeight: 600, fontSize: sq ? 19 : 22, color: d.ink, whiteSpace: 'nowrap', background: d.bg}}>
                {b.t}
              </div>
            );
          })}
        </div>

        {/* postgres */}
        <div style={{position: 'absolute', left: L.pg.x - 70, top: L.pg.y - 45, transform: `scale(${pgPop})`, opacity: Math.min(1, pgPop * 2)}}>
          <svg width={140} height={96} viewBox="0 0 140 96">
            <path d="M 4 18 L 4 78 A 66 14 0 0 0 136 78 L 136 18" fill={d.surface} stroke={d.muted} strokeWidth={2.5} />
            <ellipse cx={70} cy={18} rx={66} ry={14} fill={d.surface} stroke={d.muted} strokeWidth={2.5} />
            <text x={70} y={64} textAnchor="middle" fontFamily={inter} fontWeight={600} fontSize={21} fill={d.ink}>
              Postgres
            </text>
          </svg>
        </div>

        {L.pools.map((p, i) => (
          <Pool key={p.title} p={p} i={i} sq={sq} open={open} />
        ))}

        {/* gate */}
        <div style={{position: 'absolute', left: L.gate.x - 70, top: L.gate.y - 60, width: 140, height: 120, transform: `scale(${gatePop})`, opacity: Math.min(1, gatePop * 2)}}>
          <div style={{position: 'absolute', left: 0, top: 0, width: 12, height: 120, borderRadius: 6, background: d.ink}} />
          <div style={{position: 'absolute', right: 0, top: 0, width: 12, height: 120, borderRadius: 6, background: d.ink}} />
          {[-1, 1].map((s) => (
            <div key={s} style={{position: 'absolute', top: 18, height: 84, width: 58 * (1 - open * 0.85), left: s < 0 ? 12 : undefined, right: s > 0 ? 12 : undefined, border: `3px solid ${d.ink}`, borderRadius: 6, boxSizing: 'border-box', opacity: 1 - open * 0.6}} />
          ))}
          <div style={{position: 'absolute', left: 70 - 22, top: 60 - 26, transform: `translateY(${-open * 50}px)`, opacity: 1 - open * 0.3}}>
            <Padlock size={44} color={d.ink} open={open} />
          </div>
        </div>
        <div style={{position: 'absolute', left: L.chip.x, top: L.chip.y, transform: `translate(-50%, -50%) scale(${gatePop})`, fontFamily: mono, fontSize: sq ? 21 : 24, color: d.ink, background: d.surface, border: `2px solid ${d.line}`, borderRadius: 10, padding: '8px 16px', whiteSpace: 'pre'}}>
          {chipText.slice(0, chipTyped)}
          <span style={{opacity: frame >= flipAt && frame < flipAt + 36 && Math.floor(frame / 8) % 2 === 0 ? 1 : 0}}>▍</span>
        </div>

        {frame >= deniedAt ? (
          <div
            style={{
              position: 'absolute',
              left: L.gate.x - 330,
              top: L.gate.y - (sq ? 30 : 140),
              transform: `scale(${stamp * (1 - stampGone)}) rotate(-4deg)`,
              background: d.ink,
              color: d.bg,
              fontFamily: mono,
              fontWeight: 500,
              fontSize: sq ? 22 : 26,
              padding: '10px 18px',
              borderRadius: 10,
              whiteSpace: 'nowrap',
            }}
          >
            422 no_eligible_pool
          </div>
        ) : null}

        {/* audit log */}
        {auditIn > 0 ? (
          <div
            style={{
              position: 'absolute',
              left: sq ? 40 : 60,
              width: sq ? width - 80 : 800,
              top: sq ? 780 : 800,
              height: sq ? 260 : 240,
              transform: `translateY(${(1 - auditIn) * 320}px)`,
              background: d.surface,
              border: `2px solid ${d.line}`,
              borderRadius: 14,
              padding: '18px 24px',
              boxSizing: 'border-box',
              fontFamily: mono,
              fontSize: sq ? 19 : 21,
              color: d.ink,
            }}
          >
            <div style={{color: d.muted, marginBottom: 12}}>$ differently admin audit --tail</div>
            {AUDIT.map((l) => {
              const p = interpolate(frame, [beat(l.at), beat(l.at) + 8], [0, 1], clamp);
              return (
                <div key={l.text} style={{opacity: p, transform: `translateX(${(1 - p) * 20}px)`, whiteSpace: 'pre', lineHeight: 1.7}}>
                  {l.text}
                </div>
              );
            })}
          </div>
        ) : null}
      </AbsoluteFill>

      {handing ? (
        <>
          <div style={{position: 'absolute', left: box.cx - box.w / 2, top: box.cy - box.h / 2, width: box.w, height: box.h, borderRadius: box.r, background: interpolateColors(handT, [0, 1], [d.ink, tokens.light.bg])}} />
          <div
            style={{
              position: 'absolute',
              left: (L.server.x - L.server.w / 2 + (sq ? 28 : 32) + markSize + 22 - L.server.x) * drift + L.server.x,
              top: L.server.y,
              transform: `translateY(-50%) scale(${drift})`,
              transformOrigin: 'left center',
              opacity: 1 - Math.min(1, handT * 4),
            }}
          >
            <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 30 : 36, color: d.bg, letterSpacing: '-0.03em', whiteSpace: 'nowrap'}}>differently-server</div>
            <div style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 20 : 23, color: tokens.light.muted, marginTop: 4}}>one API · policy · placement</div>
          </div>
          <div style={{position: 'absolute', left: mk.x - mk.size / 2, top: mk.y - mk.size / 2}}>
            <Mark size={mk.size} color={d.bg} />
          </div>
        </>
      ) : null}

      <AbsoluteFill style={{justifyContent: 'flex-start', alignItems: 'center', paddingTop: sq ? 80 : 60, opacity: 1 - handT * 4}}>
        <Card text="Your pools first." from={0} to={beat(2.5)} width={L.textW} max={sq ? 88 : 100} tone="dark" />
        <Card text="Quotas per team." from={beat(2.5)} to={beat(5)} width={L.textW} max={sq ? 88 : 100} tone="dark" />
        <Card text="Full? It fails *closed*." from={beat(5)} to={beat(7.5)} width={L.textW} max={sq ? 88 : 100} tone="dark" />
        <Card text="Overflow only if policy allows." from={beat(7.5)} to={beat(10.3)} width={L.textW} max={sq ? 88 : 100} tone="dark" />
        <Card text="Every placement, audited." from={beat(10.3)} to={beat(13)} width={L.textW} max={sq ? 88 : 100} tone="dark" exit />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
