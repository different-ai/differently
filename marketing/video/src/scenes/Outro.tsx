import React from 'react';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card, Kinetic} from '../kinetic';
import {mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, Mark, Pill, Wordmark} from '../ui';

const t = tokens.light;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);

const PILLS = [
  {text: 'pre-release', filled: true},
  {text: 'Apache-2.0'},
  {text: 'self-hosted'},
  {text: 'TS · Python · Go'},
];

/** Honest status, then the end card. The last ~3 s are completely still. */
export const Outro: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height, fps} = useVideoConfig();
  const sq = width === height;
  const textW = width - (sq ? 120 : 260);
  const status = frame < beat(4);
  const split = spring({frame: frame - beat(4), fps, config: {damping: 13, stiffness: 240, mass: 0.6}});
  const word = interpolate(frame, [beat(4) + 6, beat(5.2)], [0, 1], {...clamp, easing: OUT});
  const url = interpolate(frame, [beat(7.5), beat(8.3)], [0, 1], {...clamp, easing: OUT});
  return (
    <AbsoluteFill style={{background: t.bg}}>
      {status ? (
        <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center', gap: sq ? 44 : 56}}>
          <Card text="Come build it with us." from={beat(0)} to={beat(4)} width={textW} max={sq ? 100 : 128} tone="light" exit />
          <div style={{display: 'flex', gap: 18, flexWrap: 'wrap', justifyContent: 'center', maxWidth: sq ? 860 : 1400}}>
            {PILLS.map((p, i) => {
              const pop = spring({frame: frame - beat(0.6 + i * 0.5), fps, config: {damping: 11, stiffness: 240, mass: 0.6}});
              const out = interpolate(frame, [beat(3.6), beat(4)], [1, 0], clamp);
              return (
                <Pill key={p.text} tone="light" filled={p.filled} pop={pop * out} size={sq ? 32 : 38}>
                  {p.text}
                </Pill>
              );
            })}
          </div>
          <div style={{fontFamily: mono, fontSize: sq ? 26 : 30, color: t.muted, opacity: interpolate(frame, [beat(2.4), beat(2.8), beat(3.6), beat(4)], [0, 1, 1, 0], clamp)}}>
            not production-ready yet
          </div>
        </AbsoluteFill>
      ) : (
        <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center'}}>
          <div style={{display: 'flex', alignItems: 'center', gap: sq ? 30 : 38, marginBottom: sq ? 56 : 64}}>
            <Mark size={sq ? 96 : 112} color={t.ink} split={Math.min(1, 0.2 + 0.8 * split)} />
            <Wordmark size={sq ? 92 : 112} color={t.ink} reveal={word} />
          </div>
          <Kinetic text={'Your infrastructure.\nYour rules.\nOne *API*.'} at={beat(5)} size={sq ? 76 : 84} tone="light" stagger={beat(0.75) / 2} lineHeight={1.12} />
          <div style={{marginTop: sq ? 56 : 64, textAlign: 'center', opacity: url, transform: `translateY(${(1 - url) * 16}px)`}}>
            <div style={{fontFamily: mono, fontSize: sq ? 32 : 38, color: t.ink, fontWeight: 500}}>github.com/different-ai/differently</div>
            <div style={{fontFamily: mono, fontSize: sq ? 25 : 28, color: t.muted, marginTop: 12}}>different-ai.github.io/differently</div>
          </div>
        </AbsoluteFill>
      )}
    </AbsoluteFill>
  );
};
