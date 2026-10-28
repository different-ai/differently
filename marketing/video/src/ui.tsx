import React from 'react';
import {interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {inter, tokens, Tone} from './theme';

export const clamp = {extrapolateLeft: 'clamp', extrapolateRight: 'clamp'} as const;

// Springs tuned once and reused so every element moves with the same "hand".
export const SNAP = {damping: 14, stiffness: 180, mass: 0.7};

export const useSpring = (delay: number, config = SNAP, durationInFrames?: number) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  return spring({frame: frame - delay, fps, config, durationInFrames});
};

/** The logo: two overlapping rounded squares. `split` 0 → apart, 1 → resting position. */
export const Mark: React.FC<{size: number; color: string; split?: number; back?: number; front?: number}> = ({
  size,
  color,
  split = 1,
  back = 1,
  front = 1,
}) => {
  const off = (1 - split) * 9;
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" style={{overflow: 'visible', display: 'block'}}>
      <rect
        x={10 + off}
        y={3 - off}
        width={19}
        height={19}
        rx={4}
        fill="none"
        stroke={color}
        strokeWidth={2.5}
        opacity={back}
        transform={`rotate(${(1 - split) * 12} ${19.5 + off} ${12.5 - off})`}
      />
      <rect
        x={3 - off}
        y={10 + off}
        width={19}
        height={19}
        rx={4}
        fill={color}
        opacity={front}
        transform={`rotate(${-(1 - split) * 12} ${12.5 - off} ${19.5 + off})`}
      />
    </svg>
  );
};

export const Wordmark: React.FC<{size: number; color: string; reveal?: number}> = ({size, color, reveal = 1}) => {
  const word = 'differently';
  return (
    <div style={{display: 'flex', fontFamily: inter, fontWeight: 600, fontSize: size, letterSpacing: '-0.045em', color}}>
      {word.split('').map((ch, i) => {
        const p = interpolate(reveal * (word.length + 4) - i, [0, 4], [0, 1], clamp);
        return (
          <span key={i} style={{opacity: p, transform: `translateY(${(1 - p) * size * 0.35}px)`, display: 'inline-block'}}>
            {ch}
          </span>
        );
      })}
    </div>
  );
};

export const Pill: React.FC<{tone: Tone; filled?: boolean; pop?: number; size?: number; children: React.ReactNode}> = ({
  tone,
  filled,
  pop = 1,
  size = 30,
  children,
}) => {
  const t = tokens[tone];
  return (
    <div
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        padding: `${size * 0.42}px ${size * 0.95}px`,
        borderRadius: 999,
        border: `2px solid ${t.ink}`,
        background: filled ? t.ink : 'transparent',
        color: filled ? t.bg : t.ink,
        fontFamily: inter,
        fontWeight: 500,
        fontSize: size,
        letterSpacing: '-0.01em',
        transform: `scale(${pop})`,
        opacity: Math.min(1, pop * 2),
        whiteSpace: 'nowrap',
      }}
    >
      {children}
    </div>
  );
};
