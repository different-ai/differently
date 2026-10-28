import React from 'react';
import {measureText} from '@remotion/layout-utils';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Kinetic} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, lifted, Mark, Pill, Wordmark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);

const PILLS = [
  {text: 'pre-release', filled: true},
  {text: 'open source'},
  {text: 'Apache-2.0'},
];

/**
 * The end card. The mark arrives in its lifted pose from the routing scene and settles into
 * the lockup; the honest status sits on the card itself so it survives a screenshot.
 */
export const Outro: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height, fps} = useVideoConfig();
  const sq = width === height;

  // Layout, computed so the travelling mark lands exactly in the lockup.
  const mSize = sq ? 92 : 104;
  const wSize = sq ? 88 : 104;
  const gap = sq ? 28 : 36;
  const tagSize = sq ? 68 : 76;
  const pillSize = sq ? 26 : 30;
  const urlSize = sq ? 36 : 46;
  const wordW = measureText({text: 'differently', fontFamily: inter, fontWeight: '600', fontSize: wSize, letterSpacing: '-0.045em'}).width;
  const tagH = 3 * tagSize * 1.12;
  const pillH = pillSize * 2.1;
  const urlH = urlSize * 1.25 + 10 + (sq ? 24 : 26) * 1.25;
  const g1 = sq ? 48 : 52;
  const g2 = sq ? 40 : 44;
  const g3 = sq ? 34 : 36;
  const total = mSize + g1 + tagH + g2 + pillH + g3 + urlH;
  const top = (height - total) / 2;
  const end = {x: width / 2 - (mSize + gap + wordW) / 2 + mSize / 2, y: top + mSize / 2, size: mSize};
  const start = lifted(width, height);

  const toEnd = interpolate(frame, [0, beat(0.7)], [0, 1], {...clamp, easing: IN_OUT});
  const pose = (k: 'x' | 'y' | 'size') => start[k] + (end[k] - start[k]) * toEnd;
  const word = interpolate(frame, [beat(0.4), beat(1.2)], [0, 1], {...clamp, easing: OUT});
  const url = interpolate(frame, [beat(2.7), beat(3.2)], [0, 1], {...clamp, easing: OUT});
  const tagTop = top + mSize + g1;
  const pillTop = tagTop + tagH + g2;
  const urlTop = pillTop + pillH + g3;

  return (
    <AbsoluteFill style={{background: t.bg}}>
      <div style={{position: 'absolute', left: end.x + mSize / 2 + gap, top: end.y + 2, transform: 'translateY(-50%)'}}>
        <Wordmark size={wSize} color={t.ink} reveal={word} />
      </div>
      <div style={{position: 'absolute', left: 0, right: 0, top: tagTop, display: 'flex', justifyContent: 'center'}}>
        <Kinetic text={'Your infrastructure.\nYour rules.\nOne *API*.'} at={beat(0.8)} size={tagSize} tone="light" stagger={beat(0.5) / 2} lineHeight={1.12} />
      </div>
      <div style={{position: 'absolute', left: 0, right: 0, top: pillTop, display: 'flex', justifyContent: 'center', gap: 14}}>
        {PILLS.map((p, i) => {
          const pop = spring({frame: frame - beat(2.2 + i * 0.2), fps, config: {damping: 11, stiffness: 240, mass: 0.6}});
          return (
            <Pill key={p.text} tone="light" filled={p.filled} pop={pop} size={pillSize}>
              {p.text}
            </Pill>
          );
        })}
      </div>
      <div style={{position: 'absolute', left: 0, right: 0, top: urlTop, textAlign: 'center', opacity: url, transform: `translateY(${(1 - url) * 16}px)`}}>
        <div style={{fontFamily: mono, fontSize: urlSize, color: t.ink, fontWeight: 500}}>★ github.com/different-ai/differently</div>
        <div style={{fontFamily: mono, fontSize: sq ? 24 : 26, color: t.muted, marginTop: 10}}>different-ai.github.io/differently</div>
      </div>
      <div style={{position: 'absolute', left: pose('x') - pose('size') / 2, top: pose('y') - pose('size') / 2}}>
        <Mark size={pose('size')} color={t.ink} />
      </div>
    </AbsoluteFill>
  );
};
