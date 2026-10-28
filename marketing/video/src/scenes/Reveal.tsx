import React from 'react';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card} from '../kinetic';
import {mono, tokens} from '../theme';
import {beat} from '../time';
import {clamp, Mark, Wordmark} from '../ui';

const d = tokens.dark;
const OUT = Easing.bezier(0.16, 1, 0.3, 1);
const IN_OUT = Easing.bezier(0.65, 0, 0.35, 1);

/** "So we did it… differently." The two squares fly in and lock together on the downbeat, then the pitch. */
export const Reveal: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height, fps} = useVideoConfig();
  const sq = width === height;
  const textW = width - (sq ? 120 : 260);
  // Squares travel in from opposite corners and land exactly on beat 4.
  const fly = interpolate(frame, [beat(1.6), beat(3)], [0, 1], {...clamp, easing: Easing.bezier(0.5, 0, 0.75, 0)});
  const settle = spring({frame: frame - beat(3), fps, config: {damping: 12, stiffness: 260, mass: 0.6}});
  const split = frame < beat(3) ? 0.15 * fly : 0.15 + 0.85 * settle;
  const word = interpolate(frame, [beat(3) + 4, beat(4)], [0, 1], {...clamp, easing: OUT});
  // At beat 8 the lockup glides to the top to make room for the hero line.
  const lift = interpolate(frame, [beat(4.7), beat(5.3)], [0, 1], {...clamp, easing: IN_OUT});
  const markSize = (sq ? 130 : 150) * (1 - lift * 0.45);
  const wordSize = (sq ? 120 : 150) * (1 - lift * 0.45);
  const cy = height / 2 - lift * (height / 2 - (sq ? 230 : 230));
  const enter = frame < beat(3) ? fly : 1;
  const flash = interpolate(frame, [beat(3) - 1, beat(3), beat(3) + 8], [0, 0.14, 0], clamp);
  return (
    <AbsoluteFill style={{background: d.bg}}>
      <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center'}}>
        <Card text="So we did it…" from={0} to={beat(1.8)} width={textW} max={sq ? 140 : 170} tone="dark" exit />
      </AbsoluteFill>
      {frame >= beat(1.6) ? (
        <div
          style={{
            position: 'absolute',
            left: 0,
            right: 0,
            top: cy,
            transform: 'translateY(-50%)',
            display: 'flex',
            justifyContent: 'center',
            alignItems: 'center',
            gap: markSize * 0.28,
          }}
        >
          <div style={{transform: `translate(${(1 - enter) * -width * 0.6}px, ${(1 - enter) * height * 0.5}px) rotate(${(1 - enter) * -90}deg)`}}>
            <Mark size={markSize} color={d.ink} split={split} />
          </div>
          <div style={{width: word * wordSize * 5.1, overflow: 'visible'}}>
            <Wordmark size={wordSize} color={d.ink} reveal={word} />
          </div>
        </div>
      ) : null}
      <AbsoluteFill style={{background: d.ink, opacity: flash}} />
      <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center', paddingTop: sq ? 220 : 160}}>
        <Card text="Not another sandbox." from={beat(5.3)} to={beat(7.6)} width={textW} max={sq ? 110 : 140} tone="dark" />
        <Card text={'A sandbox *router*.'} from={beat(7.6)} to={beat(10)} width={textW} max={sq ? 110 : 140} tone="dark" stagger={6} />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

const CODE = `import { Differently } from "@differently/sdk";

const differently = new Differently({
  apiKey: process.env.DIFFERENTLY_API_KEY,
});

const sandbox = await differently.sandboxes.create({
  profile: "small",
  placement: { strategy: "private-first", external: "never" },
});`;

const KW = /\b(import|from|const|new|await)\b/g;

const Line: React.FC<{text: string; size: number; hl: number; strike?: number}> = ({text, size, hl, strike = 0}) => {
  const parts: {s: string; kw: boolean}[] = [];
  let last = 0;
  for (const m of text.matchAll(KW)) {
    if (m.index! > last) parts.push({s: text.slice(last, m.index), kw: false});
    parts.push({s: m[0], kw: true});
    last = m.index! + m[0].length;
  }
  parts.push({s: text.slice(last), kw: false});
  return (
    <div style={{position: 'relative', height: size * 1.55, display: 'flex', alignItems: 'center', whiteSpace: 'pre', width: 'fit-content'}}>
      <div
        style={{
          position: 'absolute',
          left: -14,
          width: `calc(${hl} * (100% + 28px))`,
          opacity: hl > 0.01 ? 1 : 0,
          top: 2,
          bottom: 2,
          background: d.ink,
          borderRadius: 8,
        }}
      />
      <span style={{position: 'relative', color: hl > 0.5 ? d.bg : d.ink}}>
        {parts.map((p, i) => (
          <span key={i} style={{color: hl > 0.5 ? d.bg : p.kw ? d.muted : d.ink}}>
            {p.s}
          </span>
        ))}
      </span>
      {strike > 0 ? <div style={{position: 'absolute', left: -6, top: '50%', height: 4, width: `calc(${strike * 100}% + 12px)`, background: d.ink}} /> : null}
    </div>
  );
};

/** "One API in. Never a backend." over the real SDK call. */
export const Code: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const lines = CODE.split('\n');
  const size = sq ? 23 : 31;
  const chars = Math.floor(interpolate(frame, [2, beat(1.1)], [0, CODE.length], clamp));
  let budget = chars;
  const typed = lines.map((l) => {
    const take = Math.max(0, Math.min(l.length, budget));
    budget -= l.length + 1;
    return l.slice(0, take);
  });
  const hlShape = interpolate(frame, [beat(0.7), beat(0.7) + 10], [0, 1], {...clamp, easing: OUT}) * (1 - interpolate(frame, [beat(2.5), beat(2.5) + 8], [0, 1], clamp));
  const hlPolicy = interpolate(frame, [beat(1.3), beat(1.3) + 10], [0, 1], {...clamp, easing: OUT}) * (1 - interpolate(frame, [beat(2.5), beat(2.5) + 8], [0, 1], clamp));
  // A vendor line tries to sneak in, gets struck through and removed.
  const ghostType = Math.floor(interpolate(frame, [beat(2.5), beat(2.9)], [0, 26], clamp));
  const ghostStrike = interpolate(frame, [beat(3.05), beat(3.25)], [0, 1], {...clamp, easing: OUT});
  const ghostGone = interpolate(frame, [beat(3.6), beat(4)], [0, 1], {...clamp, easing: IN_OUT});
  const shake = frame > beat(3.05) && frame < beat(3.5) ? Math.sin(frame * 2.2) * 6 : 0;
  const ghost = '  backend: "sandbox.co",';
  const textW = width - (sq ? 120 : 260);
  const camera = 1 + frame * 0.0002;
  return (
    <AbsoluteFill style={{background: d.bg}}>
      <AbsoluteFill style={{justifyContent: 'flex-start', alignItems: 'center', paddingTop: sq ? 110 : 90}}>
        <Card text="One API in." from={0} to={beat(2.5)} width={textW} max={sq ? 104 : 124} tone="dark" />
        <Card text="Never a *backend*." from={beat(2.5)} to={beat(5)} width={textW} max={sq ? 104 : 124} tone="dark" />
      </AbsoluteFill>
      <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center', paddingTop: sq ? 230 : 250, transform: `scale(${camera})`}}>
        <div
          style={{
            fontFamily: mono,
            fontSize: size,
            background: d.surface,
            border: `2px solid ${d.line}`,
            borderRadius: 16,
            padding: sq ? '26px 34px' : '34px 52px',
            width: sq ? 920 : 1260,
            boxSizing: 'border-box',
            transform: `translateX(${shake}px)`,
          }}
        >
          {typed.map((l, i) => {
            const isShape = lines[i].includes('profile');
            const isPolicy = lines[i].includes('placement');
            return (
              <React.Fragment key={i}>
                {isPolicy && ghostType > 0 ? (
                  <div style={{height: (1 - ghostGone) * size * 1.55, opacity: 1 - ghostGone, overflow: 'hidden'}}>
                    <Line text={ghost.slice(0, ghostType)} size={size} hl={0} strike={ghostStrike} />
                  </div>
                ) : null}
                <Line text={l || ' '} size={size} hl={isShape ? hlShape : isPolicy ? hlPolicy : 0} />
              </React.Fragment>
            );
          })}
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
