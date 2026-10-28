import React from 'react';
import {fitText} from '@remotion/layout-utils';
import {Easing, interpolate, useCurrentFrame} from 'remotion';
import {inter, tokens, Tone} from './theme';
import {clamp} from './ui';

const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN = Easing.bezier(0.7, 0, 0.84, 0);

/** Largest font size (capped) at which every line fits the width. */
export const fit = (text: string, width: number, max: number, weight = 600) => {
  const sizes = text
    .replace(/\*/g, '')
    .split('\n')
    .map((line) => fitText({text: line, withinWidth: width, fontFamily: inter, fontWeight: weight, letterSpacing: '-0.035em'}).fontSize);
  return Math.min(max, ...sizes);
};

/**
 * Masked word-by-word headline: each word rises out of its own clipping box and
 * settles without bounce. `*word*` gets an underline that draws in after it lands.
 */
export const Kinetic: React.FC<{
  text: string;
  at: number;
  out?: number;
  size: number;
  tone: Tone;
  stagger?: number;
  align?: 'left' | 'center';
  muted?: boolean;
  weight?: number;
  lineHeight?: number;
  style?: React.CSSProperties;
}> = ({text, at, out, size, tone, stagger = 4, align = 'center', muted, weight = 600, lineHeight = 1.04, style}) => {
  const frame = useCurrentFrame();
  const color = muted ? tokens[tone].muted : tokens[tone].ink;
  let i = 0;
  return (
    <div style={{fontFamily: inter, fontWeight: weight, fontSize: size, letterSpacing: '-0.035em', lineHeight, color, textAlign: align, ...style}}>
      {text.split('\n').map((line, li) => (
        <div key={li} style={{display: 'flex', justifyContent: align === 'center' ? 'center' : 'flex-start', columnGap: size * 0.24, whiteSpace: 'nowrap'}}>
          {line.split(' ').map((raw, wi) => {
            const k = i++;
            const em = /^\*.*\*[.,!?…]?$/.test(raw);
            const word = raw.replace(/\*/g, '');
            const t0 = at + k * stagger;
            const pin = interpolate(frame, [t0, t0 + 20], [1, 0], {...clamp, easing: OUT});
            const pout = out === undefined ? 0 : interpolate(frame, [out + k * 2, out + k * 2 + 12], [0, 1], {...clamp, easing: IN});
            const ul = interpolate(frame, [t0 + 10, t0 + 28], [0, 1], {...clamp, easing: OUT});
            return (
              <span key={wi} style={{display: 'inline-block', overflow: 'hidden', padding: `0 0 ${size * 0.14}px`, margin: `0 0 ${-size * 0.14}px`, position: 'relative'}}>
                <span style={{display: 'inline-block', transform: `translateY(${(pin - pout) * 110}%)`}}>{word}</span>
                {em ? (
                  <span
                    style={{
                      position: 'absolute',
                      left: 0,
                      bottom: size * 0.06,
                      height: Math.max(4, size * 0.065),
                      width: `${ul * 100}%`,
                      background: color,
                      transform: `translateY(${-pout * size * 1.2}px)`,
                    }}
                  />
                ) : null}
              </span>
            );
          })}
        </div>
      ))}
    </div>
  );
};

/** A headline that exists only inside [from, to): rises in, then hard-cuts on the next beat. */
export const Card: React.FC<{
  text: string;
  from: number;
  to: number;
  width: number;
  max: number;
  tone: Tone;
  exit?: boolean;
  stagger?: number;
  muted?: boolean;
  style?: React.CSSProperties;
}> = ({text, from, to, width, max, tone, exit, stagger, muted, style}) => {
  const frame = useCurrentFrame();
  if (frame < from - 1 || frame >= to) return null;
  const size = fit(text, width, max);
  return <Kinetic text={text} at={from} out={exit ? to - 16 : undefined} size={size} tone={tone} stagger={stagger} muted={muted} style={style} />;
};

/** Same as Card but the text is fully set from frame 0, so the first frame works as a poster. */
export const Poster: React.FC<{text: string; to: number; width: number; max: number; tone: Tone; style?: React.CSSProperties}> = ({text, to, width, max, tone, style}) => {
  const frame = useCurrentFrame();
  if (frame >= to) return null;
  return <Kinetic text={text} at={-60} out={to - 16} size={fit(text, width, max)} tone={tone} style={style} />;
};
