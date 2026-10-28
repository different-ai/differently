import React from 'react';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, Mark, SNAP} from '../ui';
import {Padlock} from './Problem';

const d = tokens.dark;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN = Easing.bezier(0.7, 0, 0.84, 0);

type XY = {x: number; y: number};
type Layout = {
  app: XY & {w: number; h: number};
  server: XY & {w: number; h: number};
  pg: XY;
  nodes: (XY & {w: number; h: number; vms: number})[];
  ext: XY & {w: number; h: number};
  gate: XY;
  chip: XY;
  textW: number;
};

const layout = (sq: boolean): Layout =>
  sq
    ? {
        app: {x: 175, y: 330, w: 260, h: 130},
        server: {x: 600, y: 330, w: 440, h: 150},
        pg: {x: 960, y: 330},
        nodes: [
          {x: 190, y: 640, w: 320, h: 170, vms: 3},
          {x: 540, y: 640, w: 320, h: 170, vms: 3},
        ],
        ext: {x: 885, y: 900, w: 300, h: 120},
        gate: {x: 860, y: 640},
        chip: {x: 885, y: 1018},
        textW: 960,
      }
    : {
        app: {x: 230, y: 495, w: 300, h: 160},
        server: {x: 770, y: 495, w: 500, h: 180},
        pg: {x: 770, y: 718},
        nodes: [
          {x: 1480, y: 330, w: 480, h: 145, vms: 4},
          {x: 1480, y: 495, w: 480, h: 145, vms: 4},
          {x: 1480, y: 660, w: 480, h: 145, vms: 4},
        ],
        ext: {x: 1480, y: 925, w: 480, h: 130},
        gate: {x: 1100, y: 925},
        chip: {x: 1100, y: 1025},
        textW: 1660,
      };

const usePop = (at: number, config = SNAP) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  return spring({frame: frame - at, fps, config});
};

const draw = (frame: number, at: number, len = 18) => interpolate(frame, [at, at + len], [0, 1], {...clamp, easing: OUT});

/** Cubic between two points, bending horizontally first (reads like a wire). */
const wire = (a: XY, b: XY) => {
  const mx = (a.x + b.x) / 2;
  return `M ${a.x} ${a.y} C ${mx} ${a.y}, ${mx} ${b.y}, ${b.x} ${b.y}`;
};
const wireV = (a: XY, b: XY) => {
  const my = (a.y + b.y) / 2;
  return `M ${a.x} ${a.y} C ${a.x} ${my}, ${b.x} ${my}, ${b.x} ${b.y}`;
};

/** Point along a cubic path string built by wire()/wireV(). */
const along = (a: XY, b: XY, t: number, vertical = false) => {
  const c1 = vertical ? {x: a.x, y: (a.y + b.y) / 2} : {x: (a.x + b.x) / 2, y: a.y};
  const c2 = vertical ? {x: b.x, y: (a.y + b.y) / 2} : {x: (a.x + b.x) / 2, y: b.y};
  const m = 1 - t;
  return {
    x: m * m * m * a.x + 3 * m * m * t * c1.x + 3 * m * t * t * c2.x + t * t * t * b.x,
    y: m * m * m * a.y + 3 * m * m * t * c1.y + 3 * m * t * t * c2.y + t * t * t * b.y,
  };
};

// Backend-neutral on purpose: Differently's job is orchestration across pools, not one VM technology.
const POOLS = ['rack-a', 'rack-b', 'gpu'];
const BADGES = ['quotas', 'audit', 'trust report'];

const Node: React.FC<{n: Layout['nodes'][number]; i: number; base: number; sq: boolean}> = ({n, i, base, sq}) => {
  const frame = useCurrentFrame();
  const pop = usePop(base);
  return (
    <div
      style={{
        position: 'absolute',
        left: n.x - n.w / 2,
        top: n.y - n.h / 2,
        width: n.w,
        height: n.h,
        border: `2.5px solid ${d.ink}`,
        borderRadius: 12,
        background: d.surface,
        transform: `scale(${pop})`,
        opacity: Math.min(1, pop * 2),
        boxSizing: 'border-box',
        padding: sq ? '14px 16px' : '14px 20px',
      }}
    >
      <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 21 : 25, color: d.ink, letterSpacing: '-0.01em'}}>
        private pool <span style={{color: d.muted, fontWeight: 500}}>· {POOLS[i]}</span>
      </div>
      <div style={{display: 'flex', gap: 12, marginTop: sq ? 16 : 12, flexWrap: 'wrap'}}>
        {Array.from({length: n.vms}).map((_, v) => {
          const p = spring({frame: frame - (base + 14 + v * 7 + i * 4), fps: 60, config: {damping: 11, stiffness: 260, mass: 0.5}});
          return (
            <div
              key={v}
              style={{
                width: sq ? 80 : 94,
                height: sq ? 58 : 58,
                borderRadius: 9,
                background: d.ink,
                color: d.bg,
                transform: `scale(${p})`,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                fontFamily: mono,
                fontSize: sq ? 16 : 19,
                fontWeight: 500,
              }}
            >
              sbx
            </div>
          );
        })}
      </div>
    </div>
  );
};

/** One API in the middle, your private pools on one side, external providers behind a fail-closed gate. */
export const Diagram: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const L = layout(sq);
  const appPop = usePop(beat(0.25));
  const serverPop = usePop(beat(1));
  const pgPop = usePop(beat(1.75));
  const appWire = draw(frame, beat(0.75));
  const pgWire = draw(frame, beat(1.75));

  // Gate act
  const gatePop = usePop(beat(10));
  const extPop = usePop(beat(10.4));
  const gateWire = draw(frame, beat(10), 24);
  const open = interpolate(frame, [beat(16), beat(16.5)], [0, 1], {...clamp, easing: OUT});
  const deniedAt = beat(12.75);
  const stamp = usePop(deniedAt, {damping: 9, stiffness: 300, mass: 0.5});
  const stampGone = interpolate(frame, [beat(14.8), beat(15.2)], [0, 1], clamp);
  const flip = frame >= beat(15) + 18;
  const chipText = flip ? 'external: "overflow"' : 'external: "never"';
  const chipTyped = frame < beat(15) ? chipText.length : frame < beat(15) + 18 ? Math.round(interpolate(frame, [beat(15), beat(15) + 18], [chipText.length, 10])) : Math.round(interpolate(frame, [beat(15) + 18, beat(15) + 34], [10, chipText.length], clamp));

  // Zoom-through the server's mark into the light outro.
  const zoomT = interpolate(frame, [beat(20.4), beat(22)], [0, 1], {...clamp, easing: Easing.bezier(0.8, 0, 0.9, 0.6)});
  const zoom = 1 + zoomT * 110;
  // Dive through the open (outlined) square of the server's mark: it is light inside,
  // so the screen fills with the outro's white instead of a black front square.
  const markSize = sq ? 52 : 58;
  const markCx = L.server.x - L.server.w / 2 + (sq ? 28 : 32) + markSize / 2;
  const markX = markCx - markSize / 2 + (25 / 32) * markSize;
  const markY = L.server.y - markSize / 2 + (7 / 32) * markSize;
  const drift = 1 + frame * 0.00006;

  const serverR = {x: L.server.x + L.server.w / 2, y: L.server.y};
  const serverB = {x: L.server.x, y: L.server.y + L.server.h / 2};
  const appR = {x: L.app.x + L.app.w / 2, y: L.app.y};
  const gateIn = sq ? {x: L.gate.x, y: L.gate.y - 40} : {x: L.gate.x - 60, y: L.gate.y};
  const serverOut = sq ? {x: L.server.x + L.server.w / 2 - 40, y: L.server.y + L.server.h / 2} : {x: L.server.x + 90, y: L.server.y + L.server.h / 2};
  const extIn = sq ? {x: L.ext.x, y: L.ext.y - L.ext.h / 2} : {x: L.ext.x - L.ext.w / 2, y: L.ext.y};
  const gateOut = sq ? {x: L.gate.x, y: L.gate.y + 40} : {x: L.gate.x + 60, y: L.gate.y};

  // Packets: requests flow app → server → nodes once the nodes exist.
  const packets: React.ReactNode[] = [];
  if (frame > beat(5)) {
    for (let k = 0; k < 6; k++) {
      const ph = ((frame - beat(5)) / 70 + k / 6) % 1;
      const node = L.nodes[k % L.nodes.length];
      const nodeIn = sq ? {x: node.x, y: node.y - node.h / 2} : {x: node.x - node.w / 2, y: node.y};
      const p = ph < 0.4 ? along(appR, {x: L.server.x - L.server.w / 2, y: L.server.y}, ph / 0.4) : ph > 0.55 ? along(sq ? {x: L.server.x - 60, y: L.server.y + L.server.h / 2} : serverR, nodeIn, (ph - 0.55) / 0.45, sq) : null;
      if (p) packets.push(<circle key={`p${k}`} cx={p.x} cy={p.y} r={7} fill={d.ink} />);
    }
  }
  // The denied request: travels to the gate, bounces back.
  if (frame >= beat(12) && frame < beat(13.6)) {
    const go = interpolate(frame, [beat(12), deniedAt], [0, 1], {...clamp, easing: IN});
    const back = interpolate(frame, [deniedAt, beat(13.6)], [0, 1], {...clamp, easing: OUT});
    const p = along(serverOut, gateIn, go * (1 - back * 0.55), sq);
    packets.push(<circle key="denied" cx={p.x} cy={p.y} r={10} fill={d.ink} />);
  }
  // Overflow allowed: a burst flows through the open gate.
  if (frame > beat(16.5)) {
    for (let k = 0; k < 4; k++) {
      const ph = ((frame - beat(16.5)) / 50 + k / 4) % 1;
      const p = ph < 0.5 ? along(serverOut, gateIn, ph / 0.5, sq) : along(gateOut, extIn, (ph - 0.5) / 0.5, sq);
      packets.push(<circle key={`o${k}`} cx={p.x} cy={p.y} r={7} fill={d.ink} />);
    }
  }

  return (
    <AbsoluteFill style={{background: d.bg, overflow: 'hidden'}}>
      <AbsoluteFill style={{transform: `scale(${zoom * drift})`, transformOrigin: `${markX}px ${markY}px`}}>
        <svg width={width} height={height} style={{position: 'absolute', inset: 0}}>
          <path d={wire(appR, {x: L.server.x - L.server.w / 2, y: L.server.y})} stroke={d.ink} strokeWidth={2.5} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - appWire} />
          {sq ? (
            <path d={wire(serverR, {x: L.pg.x - 70, y: L.pg.y})} stroke={d.muted} strokeWidth={2} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - pgWire} />
          ) : (
            <path d={wireV(serverB, {x: L.pg.x, y: L.pg.y - 48})} stroke={d.muted} strokeWidth={2} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - pgWire} />
          )}
          {L.nodes.map((n, i) => {
            const p = draw(frame, beat(2.6 + i * 0.35));
            const nodeIn = sq ? {x: n.x, y: n.y - n.h / 2} : {x: n.x - n.w / 2, y: n.y};
            const from = sq ? {x: L.server.x - 60, y: L.server.y + L.server.h / 2} : serverR;
            return <path key={i} d={sq ? wireV(from, nodeIn) : wire(from, nodeIn)} stroke={d.ink} strokeWidth={2.5} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - p} />;
          })}
          {frame >= beat(10) ? (
            <>
              <path d={sq ? wireV(serverOut, gateIn) : wireV(serverOut, gateIn)} stroke={d.ink} strokeWidth={2.5} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - gateWire} />
              <path d={sq ? wireV(gateOut, extIn) : wire(gateOut, extIn)} stroke={d.ink} strokeWidth={2.5} fill="none" strokeDasharray="3 9" strokeLinecap="round" opacity={0.35 + 0.65 * open} />
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
        <div style={{position: 'absolute', left: L.server.x - L.server.w / 2, top: L.server.y - L.server.h / 2, width: L.server.w, height: L.server.h, borderRadius: 14, background: d.ink, boxSizing: 'border-box', display: 'flex', alignItems: 'center', gap: 22, padding: sq ? '0 28px' : '0 32px', transform: `scale(${serverPop})`, opacity: Math.min(1, serverPop * 2)}}>
          <Mark size={sq ? 52 : 58} color={d.bg} />
          <div>
            <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 30 : 36, color: d.bg, letterSpacing: '-0.03em', whiteSpace: 'nowrap'}}>differently-server</div>
            <div style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 20 : 23, color: tokens.light.muted, marginTop: 4}}>one API · policy · placement</div>
          </div>
        </div>

        {/* orchestration badges */}
        <div style={{position: 'absolute', left: L.server.x, top: L.server.y - L.server.h / 2 - (sq ? 42 : 52), transform: 'translateX(-50%)', display: 'flex', gap: 12}}>
          {BADGES.map((b, k) => {
            const p = spring({frame: frame - beat(4 + k * 0.66), fps: 60, config: {damping: 11, stiffness: 260, mass: 0.5}});
            return (
              <div key={b} style={{transform: `scale(${p})`, opacity: Math.min(1, p * 2), border: `2px solid ${d.ink}`, borderRadius: 999, padding: sq ? '5px 14px' : '6px 16px', fontFamily: inter, fontWeight: 600, fontSize: sq ? 19 : 22, color: d.ink, whiteSpace: 'nowrap', background: d.bg}}>
                {b}
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

        {L.nodes.map((n, i) => (
          <Node key={i} n={n} i={i} base={beat(3 + i * 0.35)} sq={sq} />
        ))}

        {/* gate */}
        <div style={{position: 'absolute', left: L.gate.x - 70, top: L.gate.y - 60, width: 140, height: 120, transform: `scale(${gatePop})`, opacity: Math.min(1, gatePop * 2)}}>
          <div style={{position: 'absolute', left: 0, top: 0, width: 12, height: 120, borderRadius: 6, background: d.ink}} />
          <div style={{position: 'absolute', right: 0, top: 0, width: 12, height: 120, borderRadius: 6, background: d.ink}} />
          {[-1, 1].map((s) => (
            <div
              key={s}
              style={{
                position: 'absolute',
                top: 18,
                height: 84,
                width: 58 * (1 - open * 0.85),
                left: s < 0 ? 12 : undefined,
                right: s > 0 ? 12 : undefined,
                border: `3px solid ${d.ink}`,
                borderRadius: 6,
                boxSizing: 'border-box',
                opacity: 1 - open * 0.6,
              }}
            />
          ))}
          <div style={{position: 'absolute', left: 70 - 22, top: 60 - 26, transform: `translateY(${-open * 50}px)`, opacity: 1 - open * 0.3}}>
            <Padlock size={44} color={d.ink} open={open} />
          </div>
        </div>
        <div style={{position: 'absolute', left: L.chip.x, top: L.chip.y, transform: `translate(-50%, -50%) scale(${gatePop})`, fontFamily: mono, fontSize: sq ? 22 : 24, color: d.ink, background: d.surface, border: `2px solid ${d.line}`, borderRadius: 10, padding: '8px 16px', whiteSpace: 'pre'}}>
          {chipText.slice(0, chipTyped)}
          <span style={{opacity: frame >= beat(15) && frame < beat(15) + 40 && Math.floor(frame / 8) % 2 === 0 ? 1 : 0}}>▍</span>
        </div>

        {/* external pool */}
        <div
          style={{
            position: 'absolute',
            left: L.ext.x - L.ext.w / 2,
            top: L.ext.y - L.ext.h / 2,
            width: L.ext.w,
            height: L.ext.h,
            border: `2.5px dashed ${d.ink}`,
            borderRadius: 12,
            boxSizing: 'border-box',
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            justifyContent: 'center',
            transform: `scale(${extPop})`,
            opacity: Math.min(1, extPop * 2) * (0.45 + 0.55 * open),
          }}
        >
          <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 26 : 28, color: d.ink, letterSpacing: '-0.02em'}}>external provider</div>
          <div style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 18 : 20, color: d.muted, marginTop: 4}}>opt-in · named in policy</div>
        </div>

        {/* 422 stamp */}
        {frame >= deniedAt ? (
          <div
            style={{
              position: 'absolute',
              left: L.gate.x + (sq ? -250 : -40),
              top: L.gate.y - (sq ? 175 : 150),
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
      </AbsoluteFill>

      <AbsoluteFill style={{justifyContent: 'flex-start', alignItems: 'center', paddingTop: sq ? 80 : 70, opacity: 1 - zoomT * 4}}>
        <Card text="Bring your own *metal*." from={0} to={beat(4)} width={L.textW} max={sq ? 88 : 104} tone="dark" />
        <Card text="Quotas. Audit. Trust report." from={beat(4)} to={beat(7)} width={L.textW} max={sq ? 88 : 104} tone="dark" stagger={beat(0.66)} />
        <Card text="Policy decides where work goes." from={beat(7)} to={beat(10)} width={L.textW} max={sq ? 88 : 104} tone="dark" />
        <Card text="External providers?" from={beat(10)} to={beat(12.5)} width={L.textW} max={sq ? 88 : 104} tone="dark" />
        <Card text="*Fail* closed." from={beat(12.5)} to={beat(15)} width={L.textW} max={sq ? 88 : 104} tone="dark" />
        <Card text="Open only if policy allows." from={beat(15)} to={beat(20.4)} width={L.textW} max={sq ? 88 : 104} tone="dark" exit />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
