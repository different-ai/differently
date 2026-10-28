import React from 'react';
import {AbsoluteFill, Easing, interpolate, useCurrentFrame, useVideoConfig} from 'remotion';
import {fit} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, handoff, lifted, Mark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);
const FOOTNOTE = '#8f8f8f';

type Dest = {title: string; sub: string[]; at: number; extra?: 'stat' | 'pill'};

const DESTS: Dest[] = [
  {title: 'your microVMs', sub: ['Firecracker', 'on your own metal'], at: 1.2},
  {title: 'OpenSandbox', sub: ['for self-hosting'], at: 1.5, extra: 'stat'},
  {title: 'Freestyle · Daytona · …', sub: ['for more capacity', 'or non-sensitive work'], at: 1.8, extra: 'pill'},
];

/**
 * The mark is the router: it branches to every place work can go. The 64 ms is OpenSandbox's
 * own README figure for its Fast Sandbox runtime ("Firecracker creation: 64 ms P50 (serial)"),
 * so it carries an asterisk and a footnote, and stays smaller than the titles.
 */
export const Route: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const from = handoff(width, height);
  const to = lifted(width, height);
  const m = interpolate(frame, [beat(0.2), beat(0.8)], [0, 1], {...clamp, easing: IN_OUT});
  const pose = (k: 'x' | 'y' | 'size') => from[k] + (to[k] - from[k]) * m;

  const rise = (at: number) => interpolate(frame, [beat(at), beat(at) + 16], [0, 1], {...clamp, easing: OUT});
  const out = interpolate(frame, [beat(5.5), beat(6) - 2], [0, 1], {...clamp, easing: Easing.bezier(0.7, 0, 0.84, 0)});
  const show = (at: number): React.CSSProperties => ({opacity: rise(at) * (1 - out), transform: `translateY(${(1 - rise(at)) * 18 - out * 24}px)`});
  const value = Math.round(interpolate(frame, [beat(2.1), beat(2.9)], [0, 64], {...clamp, easing: Easing.bezier(0.2, 0.7, 0.3, 1)}));

  // 16:9: three columns. 1:1: two on the first row, the external one centred below.
  const colW = sq ? 470 : 560;
  const spots = sq
    ? [
        {x: width * 0.27, y: 400},
        {x: width * 0.73, y: 400},
        {x: width * 0.5, y: 720},
      ]
    : [
        {x: width * 0.2, y: 430},
        {x: width * 0.5, y: 430},
        {x: width * 0.8, y: 430},
      ];
  const titleSize = Math.min(...DESTS.map((dst) => fit(dst.title, colW, sq ? 54 : 64)));
  const subSize = sq ? 26 : 32;

  const mx = pose('x');
  const my = pose('y') + pose('size') / 2 + 10;
  const curve = (x: number, y: number) => {
    const ty = y - 34;
    return {c1y: my + (ty - my) * 0.55, c2y: my + (ty - my) * 0.45, ty};
  };
  const wire = (x: number, y: number) => {
    const {c1y, c2y, ty} = curve(x, y);
    return `M ${mx} ${my} C ${mx} ${c1y}, ${x} ${c2y}, ${x} ${ty}`;
  };
  const wireP = interpolate(frame, [beat(0.8), beat(1.4)], [0, 1], {...clamp, easing: OUT}) * (1 - out);
  const packet = (x: number, y: number, k: number) => {
    if (frame < beat(1.4) || out > 0) return null;
    const q = ((frame - beat(1.4)) / 42 + k / 3) % 1;
    const {c1y, c2y, ty} = curve(x, y);
    const mq = 1 - q;
    const px = mq * mq * mq * mx + 3 * mq * mq * q * mx + 3 * mq * q * q * x + q * q * q * x;
    const py = mq * mq * mq * my + 3 * mq * mq * q * c1y + 3 * mq * q * q * c2y + q * q * q * ty;
    return <circle key={k} cx={px} cy={py} r={7} fill={t.ink} />;
  };

  return (
    <AbsoluteFill style={{background: t.bg}}>
      <svg width={width} height={height} style={{position: 'absolute', inset: 0}}>
        {spots.map((p, i) => (
          <path key={i} d={wire(p.x, p.y)} stroke={t.ink} strokeWidth={2.5} fill="none" pathLength={1} strokeDasharray="1 1" strokeDashoffset={1 - wireP} strokeLinecap="round" />
        ))}
        {spots.map((p, i) => packet(p.x, p.y, i))}
      </svg>

      {DESTS.map((dst, i) => (
        <div key={dst.title} style={{position: 'absolute', left: spots[i].x - colW / 2, width: colW, top: spots[i].y, textAlign: 'center'}}>
          <div style={{...show(dst.at), fontFamily: inter, fontWeight: 600, fontSize: titleSize, color: t.ink, letterSpacing: '-0.04em', lineHeight: 1.05, whiteSpace: 'nowrap'}}>{dst.title}</div>
          <div style={{...show(dst.at + 0.15), fontFamily: inter, fontWeight: 500, fontSize: subSize, color: t.muted, marginTop: 10, letterSpacing: '-0.02em', lineHeight: 1.25}}>
            {dst.sub.map((line) => (
              <div key={line}>{line}</div>
            ))}
          </div>
          {dst.extra === 'stat' ? (
            <div style={{...show(2), marginTop: sq ? 18 : 26, fontFamily: inter, fontWeight: 600, color: t.ink, letterSpacing: '-0.03em', lineHeight: 1}}>
              <span style={{fontSize: sq ? 56 : 72, fontVariantNumeric: 'tabular-nums'}}>{value}</span>
              <span style={{fontSize: sq ? 30 : 38, marginLeft: 6}}>ms</span>
              <div style={{fontSize: sq ? 22 : 28, fontWeight: 500, marginTop: 8}}>
                P50 creation<span style={{color: FOOTNOTE}}>*</span>
              </div>
            </div>
          ) : null}
          {dst.extra === 'pill' ? (
            <div style={{...show(2.4), display: 'inline-flex', marginTop: sq ? 18 : 26, padding: sq ? '8px 18px' : '10px 22px', border: `2px solid ${t.ink}`, borderRadius: 999, fontFamily: mono, fontSize: sq ? 22 : 26, color: t.ink}}>
              external: &quot;overflow&quot;
            </div>
          ) : null}
        </div>
      ))}

      <div style={{...show(3), position: 'absolute', left: 0, right: 0, bottom: sq ? 48 : 58, textAlign: 'center', fontFamily: inter, fontWeight: 500, fontSize: sq ? 24 : 30, color: FOOTNOTE}}>
        * As reported by OpenSandbox for its Fast Sandbox runtime.
      </div>

      <div style={{position: 'absolute', left: pose('x') - pose('size') / 2, top: pose('y') - pose('size') / 2}}>
        <Mark size={pose('size')} color={t.ink} />
      </div>
    </AbsoluteFill>
  );
};
