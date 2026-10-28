import React from 'react';
import {AbsoluteFill, Easing, interpolate, useCurrentFrame, useVideoConfig} from 'remotion';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, handoff, lifted, Mark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);

/**
 * OpenSandbox's own creation-latency claim, shown as theirs. Source: the OpenSandbox README
 * ("Firecracker creation: 64 ms P50 (serial)" for the Fast Sandbox runtime). Differently has
 * not measured it; our adapter is only verified on OpenSandbox's Docker runtime.
 */
export const P50: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const from = handoff(width, height);
  const to = lifted(width, height);
  const m = interpolate(frame, [beat(0.25), beat(0.9)], [0, 1], {...clamp, easing: IN_OUT});
  const pose = (k: 'x' | 'y' | 'size') => from[k] + (to[k] - from[k]) * m;

  const rise = (at: number) => interpolate(frame, [beat(at), beat(at) + 16], [0, 1], {...clamp, easing: OUT});
  const value = Math.round(interpolate(frame, [beat(1), beat(2)], [0, 64], {...clamp, easing: Easing.bezier(0.2, 0.7, 0.3, 1)}));
  const out = interpolate(frame, [beat(5.5), beat(6) - 2], [0, 1], {...clamp, easing: Easing.bezier(0.7, 0, 0.84, 0)});
  const line = (at: number): React.CSSProperties => ({opacity: rise(at) * (1 - out), transform: `translateY(${(1 - rise(at)) * 18 - out * 24}px)`});

  return (
    <AbsoluteFill style={{background: t.bg}}>
      <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center', paddingTop: sq ? 120 : 110, textAlign: 'center'}}>
        <div style={{...line(0.7), fontFamily: inter, fontWeight: 500, fontSize: sq ? 36 : 42, color: t.muted, letterSpacing: '-0.02em'}}>OpenSandbox reports</div>
        <div style={{...line(0.95), fontFamily: inter, fontWeight: 600, fontSize: sq ? 210 : 250, color: t.ink, letterSpacing: '-0.05em', lineHeight: 1, fontVariantNumeric: 'tabular-nums', margin: sq ? '10px 0 6px' : '12px 0 8px'}}>
          {value}
          <span style={{fontSize: sq ? 90 : 110, letterSpacing: '-0.03em', marginLeft: 12}}>ms</span>
        </div>
        <div style={{...line(1.3), fontFamily: inter, fontWeight: 600, fontSize: sq ? 42 : 50, color: t.ink, letterSpacing: '-0.03em'}}>P50 sandbox creation</div>
        <div style={{...line(2.0), fontFamily: mono, fontSize: sq ? 20 : 24, color: t.muted, marginTop: sq ? 22 : 26}}>Fast Sandbox runtime · Firecracker · serial · source: OpenSandbox README</div>
        <div style={{...line(2.6), fontFamily: inter, fontWeight: 500, fontSize: sq ? 28 : 32, color: t.ink, marginTop: sq ? 18 : 20}}>Their number. We haven’t benchmarked it yet.</div>
      </AbsoluteFill>
      <div style={{position: 'absolute', left: pose('x') - pose('size') / 2, top: pose('y') - pose('size') / 2}}>
        <Mark size={pose('size')} color={t.ink} />
      </div>
    </AbsoluteFill>
  );
};
