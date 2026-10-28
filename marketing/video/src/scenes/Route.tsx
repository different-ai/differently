import React from 'react';
import {AbsoluteFill, Easing, interpolate, useCurrentFrame, useVideoConfig} from 'remotion';
import {fit} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, handoff, lifted, Mark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);
const FOOTNOTE = '#a3a3a3';

/**
 * The router: the mark sits on top and branches to where work can go. Self-hosting on
 * OpenSandbox on one side, hosted providers for extra capacity or non-sensitive work on the
 * other. The 64 ms is OpenSandbox's own README figure for its Fast Sandbox runtime
 * ("Firecracker creation: 64 ms P50 (serial)"), so it carries an asterisk and a footnote.
 */
export const Route: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const from = handoff(width, height);
  const to = lifted(width, height);
  const m = interpolate(frame, [beat(0.25), beat(0.9)], [0, 1], {...clamp, easing: IN_OUT});
  const pose = (k: 'x' | 'y' | 'size') => from[k] + (to[k] - from[k]) * m;

  const rise = (at: number) => interpolate(frame, [beat(at), beat(at) + 16], [0, 1], {...clamp, easing: OUT});
  const out = interpolate(frame, [beat(6.5), beat(7) - 2], [0, 1], {...clamp, easing: Easing.bezier(0.7, 0, 0.84, 0)});
  const show = (at: number): React.CSSProperties => ({opacity: rise(at) * (1 - out), transform: `translateY(${(1 - rise(at)) * 18 - out * 24}px)`});
  const value = Math.round(interpolate(frame, [beat(2.3), beat(3.1)], [0, 64], {...clamp, easing: Easing.bezier(0.2, 0.7, 0.3, 1)}));

  const colW = sq ? 460 : 760;
  const cols = [width * (sq ? 0.27 : 0.29), width * (sq ? 0.73 : 0.71)];
  const top = sq ? 470 : 440;
  const titleL = 'OpenSandbox';
  const titleR = 'Freestyle · Daytona · …';
  const titleSize = Math.min(fit(titleL, colW, sq ? 56 : 80), fit(titleR, colW, sq ? 56 : 80));

  // Two wires branch from the mark down to each destination, with a packet riding each.
  const mx = pose('x');
  const my = pose('y') + pose('size') / 2 + 10;
  const wire = (x: number) => `M ${mx} ${my} C ${mx} ${(my + top - 30) / 2 + 20}, ${x} ${(my + top - 30) / 2 - 20}, ${x} ${top - 30}`;
  const wireP = interpolate(frame, [beat(0.9), beat(1.5)], [0, 1], {...clamp, easing: OUT}) * (1 - out);
  const packet = (x: number, k: number) => {
    const ph = ((frame - beat(1.5)) / 40 + k * 0.5) % 1;
    if (frame < beat(1.5) || out > 0) return null;
    const q = ph;
    const c1y = (my + top - 30) / 2 + 20;
    const c2y = (my + top - 30) / 2 - 20;
    const mq = 1 - q;
    const px = mq * mq * mq * mx + 3 * mq * mq * q * mx + 3 * mq * q * q * x + q * q * q * x;
    const py = mq * mq * mq * my + 3 * mq * mq * q * c1y + 3 * mq * q * q * c2y + q * q * q * (top - 30);
    return <circle key={`${x}-${k}`} cx={px} cy={py} r={7} fill={t.ink} />;
  };

  return (
    <AbsoluteFill style={{background: t.bg}}>
      <svg width={width} height={height} style={{position: 'absolute', inset: 0}}>
        {cols.map((x) => (
          <path key={x} d={wire(x)} stroke={t.ink} strokeWidth={2.5} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - wireP} strokeLinecap="round" />
        ))}
        {cols.map((x, i) => packet(x, i))}
      </svg>

      {/* self-hosting */}
      <div style={{position: 'absolute', left: cols[0] - colW / 2, width: colW, top, textAlign: 'center'}}>
        <div style={{...show(1.3), fontFamily: inter, fontWeight: 600, fontSize: titleSize, color: t.ink, letterSpacing: '-0.04em', lineHeight: 1.05}}>{titleL}</div>
        <div style={{...show(1.5), fontFamily: inter, fontWeight: 500, fontSize: sq ? 28 : 38, color: t.muted, marginTop: 10, letterSpacing: '-0.02em'}}>for self-hosting</div>
        <div style={{...show(2.2), marginTop: sq ? 34 : 44, fontFamily: inter, fontWeight: 600, color: t.ink, letterSpacing: '-0.04em', lineHeight: 1, fontVariantNumeric: 'tabular-nums'}}>
          <span style={{fontSize: sq ? 100 : 150}}>{value}</span>
          <span style={{fontSize: sq ? 46 : 64, marginLeft: 8}}>ms</span>
        </div>
        <div style={{...show(2.4), fontFamily: inter, fontWeight: 500, fontSize: sq ? 24 : 32, color: t.ink, marginTop: 10}}>
          P50 sandbox creation<span style={{color: FOOTNOTE}}>*</span>
        </div>
      </div>

      {/* burst */}
      <div style={{position: 'absolute', left: cols[1] - colW / 2, width: colW, top, textAlign: 'center'}}>
        <div style={{...show(1.9), fontFamily: inter, fontWeight: 600, fontSize: titleSize, color: t.ink, letterSpacing: '-0.04em', lineHeight: 1.05, whiteSpace: 'nowrap'}}>{titleR}</div>
        <div style={{...show(2.1), fontFamily: inter, fontWeight: 500, fontSize: sq ? 28 : 38, color: t.muted, marginTop: 10, letterSpacing: '-0.02em', lineHeight: 1.25}}>
          for more capacity
          <br />
          or non-sensitive work
        </div>
        <div style={{...show(2.8), display: 'inline-flex', marginTop: sq ? 30 : 40, padding: sq ? '8px 18px' : '10px 22px', border: `2px solid ${t.ink}`, borderRadius: 999, fontFamily: mono, fontSize: sq ? 20 : 26, color: t.ink}}>
          external: &quot;overflow&quot;
        </div>
      </div>

      <div style={{...show(3.2), position: 'absolute', left: 0, right: 0, bottom: sq ? 50 : 60, textAlign: 'center', fontFamily: inter, fontWeight: 500, fontSize: sq ? 20 : 24, color: FOOTNOTE}}>
        * As reported by OpenSandbox for its Fast Sandbox runtime.
      </div>

      <div style={{position: 'absolute', left: pose('x') - pose('size') / 2, top: pose('y') - pose('size') / 2}}>
        <Mark size={pose('size')} color={t.ink} />
      </div>
    </AbsoluteFill>
  );
};
