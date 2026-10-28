import React from 'react';
import {Composition} from 'remotion';
import {Intro} from './Intro';
import {beat, FPS, TOTAL_BEATS} from './time';

export const Root: React.FC = () => (
  <>
    <Composition id="Intro" component={Intro} width={1920} height={1080} fps={FPS} durationInFrames={beat(TOTAL_BEATS)} />
    <Composition id="IntroSquare" component={Intro} width={1080} height={1080} fps={FPS} durationInFrames={beat(TOTAL_BEATS)} />
  </>
);
