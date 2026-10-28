import React from 'react';
import {AbsoluteFill, interpolate, Sequence, staticFile, useCurrentFrame, useVideoConfig} from 'remotion';
import {Audio} from '@remotion/media';
import {Crowd, Mine} from './scenes/Problem';
import {Code, Reveal} from './scenes/Reveal';
import {Diagram} from './scenes/Diagram';
import {Outro} from './scenes/Outro';
import {P50} from './scenes/P50';
import {tokens} from './theme';
import {beat, SCENES} from './time';
import {clamp, Mark} from './ui';

const SCENE_COMPONENTS: Record<string, React.FC> = {
  crowd: Crowd,
  mine: Mine,
  reveal: Reveal,
  code: Code,
  diagram: Diagram,
  p50: P50,
  outro: Outro,
};

// The corner mark shows from the moment the reveal lockup lifts until the diagram's hand-off.
const CORNER_FROM = SCENES.reveal[0] + 5.3;
const CORNER_BEATS = SCENES.diagram[0] + 13 - CORNER_FROM;

/** Small mark in the corner while the dark product scenes play. */
const CornerMark: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const o = interpolate(frame, [0, 12, beat(CORNER_BEATS - 0.4), beat(CORNER_BEATS)], [0, 1, 1, 0], clamp);
  return (
    <div style={{position: 'absolute', top: sq ? 40 : 44, left: sq ? 40 : 52, opacity: o * 0.9}}>
      <Mark size={sq ? 34 : 38} color={tokens.dark.ink} />
    </div>
  );
};

export const Intro: React.FC = () => (
  <AbsoluteFill style={{background: '#ffffff'}}>
    {Object.entries(SCENES).map(([id, [start, len]]) => {
      const Scene = SCENE_COMPONENTS[id];
      return (
        // End at the next boundary's rounded frame so scenes never leave a one-frame gap.
        <Sequence key={id} from={beat(start)} durationInFrames={beat(start + len) - beat(start)} name={id}>
          <Scene />
        </Sequence>
      );
    })}
    <Sequence from={beat(CORNER_FROM)} durationInFrames={beat(CORNER_FROM + CORNER_BEATS) - beat(CORNER_FROM)} name="corner-mark">
      <CornerMark />
    </Sequence>
    <Audio src={staticFile('audio/soundtrack.wav')} />
  </AbsoluteFill>
);
