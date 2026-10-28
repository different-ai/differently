import React from 'react';
import {measureText} from '@remotion/layout-utils';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card, Kinetic} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, lifted, Mark, Pill, Wordmark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);

const PILLS = [
  {text: 'pre-release', filled: true},
  {text: 'Apache-2.0'},
  {text: 'self-hosted'},
  {text: 'TS · Python · Go'},
];

/**
 * Honest status, then the end card. The mark arrives in its lifted pose from the routing scene,
 * sits above the status line, then settles into the lockup. The last ~3 s are still.
 */
export const Outro: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height, fps} = useVideoConfig();
  const sq = width === height;
  const textW = width - (sq ? 120 : 260);

  // End-card layout, computed so the travelling mark lands exactly in the lockup.
  const mSize = sq ? 96 : 112;
  const wSize = sq ? 92 : 112;
  const gap = sq ? 30 : 38;
  const tagSize = sq ? 76 : 84;
  const urlSize = sq ? 32 : 38;
  const wordW = measureText({text: 'differently', fontFamily: inter, fontWeight: '600', fontSize: wSize, letterSpacing: '-0.045em'}).width;
  const tagH = 3 * tagSize * 1.12;
  const urlH = urlSize * 1.25 + 12 + (sq ? 25 : 28) * 1.25;
  const total = mSize + 64 + tagH + 64 + urlH;
  const top = (height - total) / 2;
  const end = {x: width / 2 - (mSize + gap + wordW) / 2 + mSize / 2, y: top + mSize / 2, size: mSize};
  const start = lifted(width, height);
  const toEnd = interpolate(frame, [beat(3.2), beat(3.9)], [0, 1], {...clamp, easing: IN_OUT});
  const pose = (k: 'x' | 'y' | 'size') => start[k] + (end[k] - start[k]) * toEnd;

  const status = frame < beat(3.4);
  const word = interpolate(frame, [beat(3.6), beat(4.4)], [0, 1], {...clamp, easing: OUT});
  const url = interpolate(frame, [beat(5.3), beat(5.8)], [0, 1], {...clamp, easing: OUT});
  const statusOut = interpolate(frame, [beat(3), beat(3.4)], [1, 0], clamp);

  return (
    <AbsoluteFill style={{background: t.bg}}>
      {status ? (
        <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center', gap: sq ? 40 : 52, paddingTop: sq ? 150 : 130}}>
          <Card text="Come build it with us." from={beat(0.15)} to={beat(3.4)} width={textW} max={sq ? 96 : 120} tone="light" exit />
          <div style={{display: 'flex', gap: 18, flexWrap: 'wrap', justifyContent: 'center', maxWidth: sq ? 860 : 1400}}>
            {PILLS.map((p, i) => {
              const pop = spring({frame: frame - beat(0.5 + i * 0.3), fps, config: {damping: 11, stiffness: 240, mass: 0.6}});
              return (
                <Pill key={p.text} tone="light" filled={p.filled} pop={pop * statusOut} size={sq ? 32 : 38}>
                  {p.text}
                </Pill>
              );
            })}
          </div>
          <div style={{fontFamily: mono, fontSize: sq ? 26 : 30, color: t.muted, opacity: interpolate(frame, [beat(1.6), beat(2)], [0, 1], clamp) * statusOut}}>
            not production-ready yet
          </div>
        </AbsoluteFill>
      ) : (
        <>
          <div style={{position: 'absolute', left: end.x + mSize / 2 + gap, top: end.y + 2, transform: 'translateY(-50%)'}}>
            <Wordmark size={wSize} color={t.ink} reveal={word} />
          </div>
          <div style={{position: 'absolute', left: 0, right: 0, top: top + mSize + 64, display: 'flex', justifyContent: 'center'}}>
            <Kinetic text={'Your infrastructure.\nYour rules.\nOne *API*.'} at={beat(4)} size={tagSize} tone="light" stagger={beat(0.5) / 2} lineHeight={1.12} />
          </div>
          <div style={{position: 'absolute', left: 0, right: 0, top: top + mSize + 64 + tagH + 64, textAlign: 'center', opacity: url, transform: `translateY(${(1 - url) * 16}px)`}}>
            <div style={{fontFamily: mono, fontSize: urlSize, color: t.ink, fontWeight: 500}}>github.com/different-ai/differently</div>
            <div style={{fontFamily: mono, fontSize: sq ? 25 : 28, color: t.muted, marginTop: 12}}>different-ai.github.io/differently</div>
          </div>
        </>
      )}
      <div style={{position: 'absolute', left: pose('x') - pose('size') / 2, top: pose('y') - pose('size') / 2}}>
        <Mark size={pose('size')} color={t.ink} />
      </div>
    </AbsoluteFill>
  );
};
