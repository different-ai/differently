import React from 'react';
import {AbsoluteFill, interpolate, Sequence, staticFile, useCurrentFrame, useVideoConfig} from 'remotion';
import {Audio} from '@remotion/media';
import {Crowd, Mine} from './scenes/Problem';
import {Code, Reveal} from './scenes/Reveal';
import {Diagram} from './scenes/Diagram';
import {Outro} from './scenes/Outro';
import {tokens} from './theme';
import {beat, SCENES} from './time';
import {clamp, Mark} from './ui';

const SCENE_COMPONENTS: Record<string, React.FC> = {
  crowd: Crowd,
  mine: Mine,
  reveal: Reveal,
  code: Code,
  diagram: Diagram,
  outro: Outro,
};

/** Small mark in the corner while the dark product scenes play. */
const CornerMark: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const o = interpolate(frame, [0, 12, beat(31.6), beat(32)], [0, 1, 1, 0], clamp);
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
        <Sequence key={id} from={beat(start)} durationInFrames={beat(len)} name={id}>
          <Scene />
        </Sequence>
      );
    })}
    <Sequence from={beat(SCENES.reveal[0] + 8.4)} durationInFrames={beat(32)} name="corner-mark">
      <CornerMark />
    </Sequence>
    <Audio src={staticFile('audio/soundtrack.wav')} />
  </AbsoluteFill>
);
