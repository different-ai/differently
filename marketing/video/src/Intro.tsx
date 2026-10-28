import React from 'react';
import {AbsoluteFill, interpolate, Sequence, staticFile, useCurrentFrame, useVideoConfig} from 'remotion';
import {Audio} from '@remotion/media';
import {Crowd, Mine} from './scenes/Problem';
import {Code, Reveal} from './scenes/Reveal';
import {Diagram} from './scenes/Diagram';
import {Outro} from './scenes/Outro';
import {Route} from './scenes/Route';
import {inter, tokens} from './theme';
import {beat, SCENES} from './time';
import {clamp, Mark} from './ui';

const SCENE_COMPONENTS: Record<string, React.FC> = {
  crowd: Crowd,
  mine: Mine,
  reveal: Reveal,
  code: Code,
  diagram: Diagram,
  route: Route,
  outro: Outro,
};

// The corner mark shows on the dark product scenes after the reveal (so it never doubles the
// lockup) and fades with the diagram's badges before the hand-off.
const CORNER_FROM = SCENES.code[0];
const CORNER_BEATS = SCENES.diagram[0] + 12.9 - CORNER_FROM;
// A small brand tag on the light opening scenes, so the first frame says who this is.
const TAG_BEATS = SCENES.mine[0] + 7.1;

/** "differently · open source" in the corner of the opening, fading as the glue implodes. */
const BrandTag: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const o = interpolate(frame, [beat(TAG_BEATS - 0.5), beat(TAG_BEATS)], [1, 0], clamp);
  return (
    <div style={{position: 'absolute', top: sq ? 40 : 44, left: sq ? 40 : 52, opacity: o, display: 'flex', alignItems: 'center', gap: 12}}>
      <Mark size={sq ? 32 : 36} color={tokens.light.ink} />
      <span style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 26 : 30, letterSpacing: '-0.04em', color: tokens.light.ink}}>differently</span>
      <span style={{fontFamily: inter, fontWeight: 500, fontSize: sq ? 20 : 22, color: tokens.light.muted}}>· open source</span>
    </div>
  );
};

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
    <Sequence from={0} durationInFrames={beat(TAG_BEATS)} name="brand-tag">
      <BrandTag />
    </Sequence>
    <Sequence from={beat(CORNER_FROM)} durationInFrames={beat(CORNER_FROM + CORNER_BEATS) - beat(CORNER_FROM)} name="corner-mark">
      <CornerMark />
    </Sequence>
    <Audio src={staticFile('audio/soundtrack.wav')} />
  </AbsoluteFill>
);
