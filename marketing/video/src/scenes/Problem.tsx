import React from 'react';
import {AbsoluteFill, Easing, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {Card, Poster} from '../kinetic';
import {inter, mono, tokens} from '../theme';
import CROWD from '../crowd.json';
import {beat} from '../time';
import {clamp, HBlur, SNAP, useSpring} from '../ui';

const t = tokens.light;
const SMOOTH = Easing.bezier(0.65, 0, 0.35, 1);
const OUT = Easing.bezier(0.16, 1, 0.3, 1);

// Generic, made-up vendors: no real company names or logos. The first 18 end up in
// the grid; the rest only exist to crowd the opening and get flung off-screen.
// `sales` marks the ones whose "self-host" answer turns out to be a sales call.
const VENDORS = [
  {name: 'sandbox.co', call: 'sbx.run()', sales: false},
  {name: 'boxcloud', call: 'box.spawn()', sales: true},
  {name: 'vm.ly', call: 'vmly.exec()', sales: false},
  {name: 'cubed', call: 'cube.up()', sales: false},
  {name: 'runbox', call: 'rb.start()', sales: true},
  {name: 'isolato', call: 'iso.new()', sales: false},
  {name: 'podhut', call: 'hut.launch()', sales: false},
  {name: 'contain.io', call: 'c.create()', sales: true},
  {name: 'shellco', call: 'sh.open()', sales: false},
  {name: 'envy', call: 'env.boot()', sales: false},
  {name: 'jailcell', call: 'jail.lock()', sales: true},
  {name: 'cellar', call: 'cell.make()', sales: false},
  {name: 'microbx', call: 'mbx.go()', sales: false},
  {name: 'vaultvm', call: 'vault.vm()', sales: true},
  {name: 'krate', call: 'krate.ship()', sales: false},
  {name: 'hutch', call: 'hutch.run()', sales: false},
  {name: 'silo', call: 'silo.fill()', sales: false},
  {name: 'bunkr', call: 'bunkr.dig()', sales: false},
  {name: 'boxly', call: 'boxly.go()', sales: false},
  {name: 'sandbin', call: 'bin.exec()', sales: false},
  {name: 'vmbox.io', call: 'vmb.up()', sales: false},
  {name: 'runly', call: 'runly.run()', sales: false},
  {name: 'cagecloud', call: 'cage.new()', sales: false},
  {name: 'podly', call: 'podly.spin()', sales: false},
  {name: 'enclavr', call: 'enc.open()', sales: false},
  {name: 'fencd', call: 'fence.up()', sales: false},
  {name: 'walld', call: 'wall.build()', sales: false},
  {name: 'shellbx', call: 'sbx2.sh()', sales: false},
  {name: 'spawnr', call: 'spawnr.go()', sales: false},
  {name: 'instabx', call: 'insta.box()', sales: false},
  {name: 'quikvm', call: 'qvm.boot()', sales: false},
  {name: 'cellbox', call: 'cb.cell()', sales: false},
  {name: 'sandpitt', call: 'pit.dig()', sales: false},
  {name: 'hatchr', call: 'hatch.open()', sales: false},
  {name: 'vesl', call: 'vesl.sail()', sales: false},
  {name: 'capsl', call: 'capsl.seal()', sales: false},
  {name: 'pocketvm', call: 'pkt.vm()', sales: false},
  {name: 'cratr', call: 'cratr.pack()', sales: false},
  {name: 'boxfarm', call: 'farm.grow()', sales: false},
  {name: 'yetanother', call: 'ya.sandbox()', sales: false},
];

/** Small seeded PRNG (mulberry32) so the pile lands the same way on every render. */
const rand = (seed: number) => {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let q = Math.imul(a ^ (a >>> 15), 1 | a);
    q = (q + Math.imul(q ^ (q >>> 7), 61 | q)) ^ q;
    return ((q ^ (q >>> 14)) >>> 0) / 4294967296;
  };
};

export const Padlock: React.FC<{size: number; color: string; open?: number; style?: React.CSSProperties}> = ({size, color, open = 0, style}) => (
  <svg width={size} height={size} viewBox="0 0 24 24" style={{overflow: 'visible', ...style}}>
    <path
      d="M7 11 V7.5 a5 5 0 0 1 10 0 V11"
      fill="none"
      stroke={color}
      strokeWidth={2.4}
      strokeLinecap="round"
      transform={`translate(0 ${-open * 4}) rotate(${open * 30} 17 11)`}
    />
    <rect x={4} y={10.5} width={16} height={11.5} rx={3} fill={color} />
  </svg>
);

const Tag: React.FC<{text: string; filled?: boolean; pop: number; size: number}> = ({text, filled, pop, size}) => (
  <div
    style={{
      position: 'absolute',
      bottom: -size * 0.9,
      left: '50%',
      transform: `translateX(-50%) scale(${pop})`,
      opacity: Math.min(1, pop * 3),
      padding: `${size * 0.25}px ${size * 0.7}px`,
      borderRadius: 999,
      border: `2px solid ${t.ink}`,
      background: filled ? t.ink : t.bg,
      color: filled ? t.bg : t.ink,
      fontFamily: inter,
      fontWeight: 600,
      fontSize: size,
      whiteSpace: 'nowrap',
    }}
  >
    {text}
  </div>
);

type Pose = {x: number; y: number; rot: number; scale: number};

/**
 * Where card i lands in the opening pile. Early arrivals ring the headline;
 * the last ~40% pile straight onto it, bigger, until it's buried.
 */
const pilePose = (i: number, late: number, W: number, H: number, sq: boolean): Pose => {
  const R = rand(1000 + i * 7919);
  const hx = W * (sq ? 0.43 : 0.37);
  const hy = H * (sq ? 0.19 : 0.17);
  let x = W / 2;
  let y = H / 2;
  if (late >= 0.6) {
    x = W / 2 + (R() - 0.5) * 2 * hx;
    y = H / 2 + (R() - 0.5) * 2.6 * hy;
  } else {
    for (let k = 0; k < 40; k++) {
      x = W * (0.06 + 0.88 * R());
      y = H * (0.08 + 0.84 * R());
      const onHeadline = Math.abs(x - W / 2) < hx && Math.abs(y - H / 2) < hy;
      if (!onHeadline || late >= 0.45) break;
    }
  }
  const rot = (R() - 0.5) * 26;
  const scale = late < 0.6 ? 0.85 + R() * 0.3 : 1.05 + R() * 0.45;
  return {x, y, rot, scale};
};

/** Everyone ships a walled sandbox cloud, each with its own SDK, and self-hosting means a sales call. */
export const Crowd: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height, fps} = useVideoConfig();
  const sq = width === height;
  const cols = sq ? 4 : 6;
  const rows = 3;
  const n = cols * rows;
  const cw = sq ? 255 : 302;
  const ch = sq ? 205 : 228;
  const bw = cw - 36;
  const bh = ch - 74;
  const top = sq ? 400 : 352;
  const left = width / 2 - (cols * cw) / 2;
  const N = sq ? CROWD.cards.square : CROWD.cards.wide;
  const cards = VENDORS.slice(0, n).concat(VENDORS.slice(18, 18 + N - n));
  const rank = (i: number) => (i * CROWD.rankStride) % N;
  const arrive = (i: number) => beat(CROWD.firstBeat) + Math.round(beat(CROWD.spanBeats) * Math.pow(rank(i) / N, CROWD.power));
  const grid = (i: number) => ({x: left + cw * ((i % cols) + 0.5), y: top + ch * (Math.floor(i / cols) + 0.5)});
  const snapAt = beat(CROWD.snapBeat);

  // Each landing kicks the camera a little; the impacts add up as the pile grows.
  let impact = 0;
  for (let i = 0; i < N; i++) {
    const d = frame - (arrive(i) + 7);
    if (d >= 0 && frame < snapAt) impact += Math.exp(-d / 5);
  }
  const shakeX = Math.sin(frame * 2.7) * Math.min(impact, 3) * 2.4;
  const shakeY = Math.cos(frame * 3.1) * Math.min(impact, 3) * 1.6;
  const push = 1 + 0.07 * interpolate(frame, [0, snapAt], [0, 1], {...clamp, easing: Easing.bezier(0.5, 0, 0.75, 0)});
  const unpush = interpolate(frame, [snapAt, snapAt + 16], [0, 1], {...clamp, easing: OUT});

  // "Their SDK." punches into one grid card, then back out.
  const S = CROWD.snapBeat; // everything after the snap is timed from it
  const focusB = grid(sq ? 2 : 3);
  const zB = interpolate(frame, [beat(S + 2), beat(S + 2) + 10], [0, 1], {...clamp, easing: OUT}) * (1 - interpolate(frame, [beat(S + 4), beat(S + 4) + 12], [0, 1], {...clamp, easing: OUT}));
  const zoom = (push + (1 - push) * unpush) * (1 + 0.55 * zB) + interpolate(frame, [snapAt, beat(S + 10)], [0, 0.04], clamp);
  const cx = width / 2;
  const cy = top + (rows * ch) / 2;
  const fx = focusB.x * zB + cx * (1 - zB);
  const fy = focusB.y * zB + (frame < snapAt ? height / 2 : cy) * (1 - zB);
  const tx = (frame < snapAt ? 0 : width / 2 - fx) + shakeX;
  const ty = (frame < snapAt ? 0 : cy + 60 * zB - fy) + shakeY;

  // Plain spring() rather than hooks: cards enter and leave the render, so hook counts would vary.
  const sp = (delay: number, config = SNAP) => spring({frame: frame - delay, fps, config});
  const hosted = (i: number) => sp(snapAt + 12 + ((i * 5) % n));
  const lock = (i: number) => sp(beat(S + 4) + ((i % cols) + Math.floor(i / cols)) * 3);
  const sales = (i: number) => sp(beat(S + 8) + ((i * 3) % 7) * 4, {damping: 9, stiffness: 220, mass: 0.6});
  const sdkFlash = (i: number) => interpolate(frame, [beat(S + 2) + (i % n) * 1.2, beat(S + 2) + (i % n) * 1.2 + 6], [0, 1], clamp) * (frame < beat(S + 4) ? 1 : 0);

  // Out on a whip pan: everything slides left with a horizontal motion blur; Mine whips in from the right.
  const whip = interpolate(frame, [beat(S + 9.4), beat(S + 10)], [0, 1], {...clamp, easing: Easing.bezier(0.6, 0, 0.9, 0.4)});
  const textW = width - (sq ? 120 : 260);
  const order = cards.map((_, i) => i).sort((a, b) => rank(a) - rank(b)); // later arrivals on top

  return (
    <AbsoluteFill style={{background: t.bg, overflow: 'hidden'}}>
      <HBlur id="whip-out" amount={whip * 90} />
      <AbsoluteFill style={{transform: `translateX(${-whip * width * 1.15}px)`, filter: whip > 0.01 ? 'url(#whip-out)' : 'none'}}>
      <AbsoluteFill style={{transform: `translate(${tx}px, ${ty}px) scale(${zoom})`, transformOrigin: `${fx}px ${fy}px`}}>
        {frame < snapAt ? (
          <AbsoluteFill style={{justifyContent: 'center', alignItems: 'center'}}>
            <Poster text={"Everyone's building\na sandbox company."} to={snapAt + 1} width={width - (sq ? 100 : 220)} max={sq ? 112 : 150} tone="light" />
          </AbsoluteFill>
        ) : null}
        {order.map((i) => {
          const v = cards[i];
          const inGrid = i < n;
          const late = rank(i) / N;
          const pile = pilePose(i, late, width, height, sq);
          // The first few drop in one by one from above; then they pour in from every edge.
          const dropper = rank(i) < CROWD.droppers;
          const fly = spring({frame: frame - arrive(i), fps, config: dropper ? {damping: 10, stiffness: 160, mass: 0.9} : {damping: 15, stiffness: 190, mass: 0.7}});
          if (fly <= 0.001) return null;
          const dx = pile.x - width / 2 || 1;
          const dy = pile.y - height / 2 || 1;
          const dl = Math.hypot(dx, dy);
          const far = Math.max(width, height) * 0.8;
          let x = dropper ? pile.x : pile.x + (dx / dl) * far * (1 - fly);
          let y = dropper ? pile.y - (pile.y + bh) * (1 - fly) : pile.y + (dy / dl) * far * (1 - fly);
          let rot = pile.rot * (1 + (1 - fly) * 2.5);
          let scale = pile.scale;
          let opacity = 1;
          // On the "Their cloud." beat the grid cards snap into place; the rest are flung away.
          const snap = spring({frame: frame - snapAt - (inGrid ? ((i % cols) + Math.floor(i / cols)) * 1.2 : 0), fps, config: {damping: 17, stiffness: 170, mass: 0.8}});
          if (inGrid) {
            const g = grid(i);
            x = x + (g.x - x) * snap;
            y = y + (g.y - y) * snap;
            rot = rot * (1 - snap);
            scale = scale + (1 - scale) * snap;
          } else {
            x += (dx / dl) * far * 1.2 * snap;
            y += (dy / dl) * far * 1.2 * snap;
            rot += snap * 40 * Math.sign(pile.rot || 1);
            opacity = 1 - snap;
            if (opacity <= 0.01) return null;
          }
          const L = inGrid ? lock(i) : 0;
          const S = inGrid && v.sales ? sales(i) : 0;
          const H = inGrid ? hosted(i) : 0;
          const flash = inGrid ? sdkFlash(i) : 0;
          return (
            <div
              key={v.name}
              style={{
                position: 'absolute',
                left: x - bw / 2,
                top: y - bh / 2,
                width: bw,
                height: bh,
                transform: `rotate(${rot}deg) scale(${scale})`,
                opacity,
                borderRadius: 16,
                border: `${2 + 3 * L}px solid ${t.ink}`,
                background: t.bg,
                boxShadow: frame < snapAt + 20 ? '0 10px 30px rgba(10,10,10,0.10)' : 'none',
                boxSizing: 'border-box',
                display: 'flex',
                flexDirection: 'column',
                alignItems: 'center',
                justifyContent: 'center',
                gap: 6,
              }}
            >
              <div style={{fontFamily: inter, fontWeight: 600, fontSize: sq ? 28 : 32, letterSpacing: '-0.025em', color: t.ink}}>{v.name}</div>
              <div
                style={{
                  fontFamily: mono,
                  fontSize: sq ? 18 : 21,
                  color: flash > 0.5 ? t.bg : t.muted,
                  background: flash > 0.5 ? t.ink : 'transparent',
                  padding: '2px 8px',
                  borderRadius: 6,
                }}
              >
                {v.call}
              </div>
              {inGrid ? (
                <>
                  <Padlock size={24} color={t.ink} style={{position: 'absolute', top: -14, right: -10, transform: `scale(${L})`}} />
                  {S > 0.01 ? <Tag text="contact sales" filled pop={S} size={sq ? 18 : 20} /> : <Tag text="hosted only" pop={H} size={sq ? 18 : 20} />}
                </>
              ) : null}
            </div>
          );
        })}
      </AbsoluteFill>
      <AbsoluteFill style={{justifyContent: 'flex-start', alignItems: 'center', paddingTop: sq ? 90 : 70}}>
        <div style={{background: t.bg, borderRadius: 24, padding: '0 24px'}}>
          <Card text="Their cloud." from={snapAt} to={beat(S + 2)} width={textW} max={sq ? 150 : 200} tone="light" />
          <Card text="Their SDK." from={beat(S + 2)} to={beat(S + 4)} width={textW} max={sq ? 150 : 200} tone="light" />
          <Card text="Their *rules*." from={beat(S + 4)} to={beat(S + 6)} width={textW} max={sq ? 150 : 200} tone="light" />
          <Card text="Self-hosting?" from={beat(S + 6)} to={beat(S + 8)} width={textW} max={sq ? 150 : 200} tone="light" />
          <Card text="“Contact sales.”" from={beat(S + 8)} to={beat(S + 10)} width={textW} max={sq ? 150 : 200} tone="light" />
        </div>
      </AbsoluteFill>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

const PARTS = [
  {name: 'Firecracker', x: 0.6, y: 0.4, at: 4},
  {name: 'Kubernetes', x: 0.84, y: 0.5, at: 5},
  {name: 'Postgres', x: 0.62, y: 0.7, at: 6},
  {name: 'glue.sh', x: 0.86, y: 0.8, at: 7},
  {name: 'more-glue.yaml', x: 0.68, y: 0.9, at: 8},
];

/** Your own servers, and the pile of parts you'd have to glue together yourself. */
export const Mine: React.FC = () => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const sq = width === height;
  const youPop = useSpring(2, SNAP);
  const rackPop = useSpring(8, SNAP);
  const clusterPop = useSpring(14, SNAP);
  const shake = interpolate(frame, [beat(9), beat(9.8), beat(10.3)], [0, 1, 1.4], clamp);
  const sx = Math.sin(frame * 1.7) * 7 * shake;
  const sy = Math.cos(frame * 2.3) * 4 * shake;
  const knot = interpolate(frame, [beat(8.6), beat(10.2)], [0, 1], {...clamp, easing: SMOOTH});
  // In on the whip from Crowd; out by sucking everything into the knot, then a black iris
  // opens from that point into the reveal.
  const whipIn = 1 - interpolate(frame, [0, beat(0.6)], [0, 1], {...clamp, easing: Easing.bezier(0.1, 0.6, 0.4, 1)});
  const kx = width * 0.66;
  const ky = height * 0.62;
  const implode = interpolate(frame, [beat(10.1), beat(10.75)], [0, 1], {...clamp, easing: Easing.bezier(0.7, 0, 0.84, 0)});
  const iris = interpolate(frame, [beat(10.45), beat(11) - 1], [0, 1], {...clamp, easing: Easing.bezier(0.5, 0, 0.75, 0)});
  const textW = width - (sq ? 120 : 260);
  const baseX = width * (sq ? 0.16 : 0.13);
  const baseY = height * (sq ? 0.5 : 0.5);
  const G = sq ? 1.15 : 1.4; // group scale for you + rack + cluster
  const gx = (x: number) => baseX + (x - baseX) * G;
  const gy = (y: number) => baseY + (y - baseY) * G;
  return (
    <AbsoluteFill style={{background: t.bg, overflow: 'hidden'}}>
      <HBlur id="whip-in" amount={whipIn * 90} />
      <AbsoluteFill style={{transform: `translateX(${whipIn * width * 1.15}px)`, filter: whipIn > 0.01 ? 'url(#whip-in)' : 'none'}}>
      <AbsoluteFill style={{transform: `scale(${1 - 0.97 * implode}) rotate(${implode * 30}deg)`, transformOrigin: `${kx}px ${ky}px`}}>
      <AbsoluteFill style={{transform: `translate(${sx}px, ${sy}px) scale(${1 + frame * 0.00008})`}}>
        <div style={{position: 'absolute', inset: 0, transform: `scale(${G})`, transformOrigin: `${baseX}px ${baseY}px`}}>
        {/* you */}
        <div style={{position: 'absolute', left: baseX - 70, top: baseY + 40, transform: `scale(${youPop})`, transformOrigin: 'bottom center'}}>
          <svg width={110} height={160} viewBox="0 0 110 160">
            <circle cx={55} cy={34} r={28} fill={t.ink} />
            <rect x={12} y={72} width={86} height={88} rx={38} fill={t.ink} />
          </svg>
        </div>
        {/* rack */}
        <div style={{position: 'absolute', left: baseX + 70, top: baseY - 40, transform: `scale(${rackPop})`}}>
          {[0, 1, 2].map((k) => (
            <div key={k} style={{width: 220, height: 50, border: `3px solid ${t.ink}`, borderRadius: 10, marginBottom: 12, display: 'flex', alignItems: 'center', padding: '0 16px', boxSizing: 'border-box', gap: 10, background: t.bg}}>
              {[0, 1, 2].map((d) => (
                <div key={d} style={{width: 9, height: 9, borderRadius: 9, background: t.ink, opacity: (Math.floor(frame / 9) + d + k) % 3 === 0 ? 1 : 0.3}} />
              ))}
              <div style={{flex: 1}} />
              <div style={{width: 64, height: 4, borderRadius: 2, background: t.line}} />
            </div>
          ))}
          <div style={{fontFamily: inter, fontWeight: 500, fontSize: 24, color: t.muted, textAlign: 'center'}}>your servers</div>
        </div>
        {/* cluster */}
        <div style={{position: 'absolute', left: baseX + 110, top: baseY + 200, transform: `scale(${clusterPop})`, display: 'flex', alignItems: 'center', gap: 16}}>
          <svg width={90} height={90} viewBox="-45 -45 90 90" style={{transform: `rotate(${frame * 0.4}deg)`}}>
            {Array.from({length: 7}).map((_, k) => {
              const a = (k / 7) * Math.PI * 2;
              return <line key={k} x1={0} y1={0} x2={Math.sin(a) * 36} y2={-Math.cos(a) * 36} stroke={t.ink} strokeWidth={3} opacity={0.5} />;
            })}
            <polygon
              points={Array.from({length: 7})
                .map((_, k) => `${Math.sin((k / 7) * Math.PI * 2) * 38},${-Math.cos((k / 7) * Math.PI * 2) * 38}`)
                .join(' ')}
              fill="none"
              stroke={t.ink}
              strokeWidth={4.5}
              strokeLinejoin="round"
            />
            <circle r={8} fill={t.ink} />
          </svg>
          <div style={{fontFamily: inter, fontWeight: 500, fontSize: 24, color: t.muted}}>your cluster</div>
        </div>
        </div>
        <Glue sq={sq} knot={knot} anchors={[[gx(baseX + 300), gy(baseY + 40)], [gx(baseX + 210), gy(baseY + 245)]]} />
      </AbsoluteFill>
      </AbsoluteFill>
      <AbsoluteFill style={{justifyContent: 'flex-start', alignItems: 'center', paddingTop: sq ? 90 : 70, opacity: 1 - implode}}>
        <Card text={'I just want sandboxes\non *my* *own* servers.'} from={0} to={beat(4)} width={textW} max={sq ? 96 : 116} tone="light" />
        <Card text="Firecracker." from={beat(4)} to={beat(5)} width={textW} max={sq ? 150 : 190} tone="light" stagger={0} />
        <Card text="Kubernetes." from={beat(5)} to={beat(6)} width={textW} max={sq ? 150 : 190} tone="light" stagger={0} />
        <Card text="Postgres." from={beat(6)} to={beat(7)} width={textW} max={sq ? 150 : 190} tone="light" stagger={0} />
        <Card text="Glue." from={beat(7)} to={beat(8)} width={textW} max={sq ? 150 : 190} tone="light" stagger={0} />
        <Card text="More glue." from={beat(8)} to={beat(9)} width={textW} max={sq ? 150 : 190} tone="light" stagger={3} />
        <Card text="So. Much. *Glue*." from={beat(9)} to={beat(11)} width={textW} max={sq ? 150 : 190} tone="light" stagger={beat(0.4)} />
      </AbsoluteFill>
      </AbsoluteFill>
      {iris > 0 ? <div style={{position: 'absolute', left: kx - iris * width * 1.3, top: ky - iris * width * 1.3, width: iris * width * 2.6, height: iris * width * 2.6, borderRadius: '50%', background: tokens.dark.bg}} /> : null}
    </AbsoluteFill>
  );
};

const Glue: React.FC<{sq: boolean; knot: number; anchors: number[][]}> = ({sq, knot, anchors}) => {
  const frame = useCurrentFrame();
  const {width, height} = useVideoConfig();
  const px = (p: (typeof PARTS)[number]) => width * (sq ? 0.12 + p.x * 0.86 : p.x);
  const py = (p: (typeof PARTS)[number]) => height * (sq ? 0.08 + p.y * 0.86 : p.y);
  return (
    <>
      <svg width={width} height={height} style={{position: 'absolute', inset: 0}}>
        {Array.from({length: 22}).map((_, i) => {
          const p = PARTS[i % PARTS.length];
          const at = beat(p.at + Math.floor(i / PARTS.length) * 0.9 + 0.1);
          const prog = interpolate(frame, [at, at + 24], [0, 1], {...clamp, easing: OUT});
          const [ax, ay] = anchors[i % 2];
          const tx = px(p) - 60;
          const ty = py(p);
          const k1 = ((i * 137) % 460) - 230;
          const k2 = ((i * 251) % 560) - 280;
          const wob = Math.sin(frame / 6 + i) * (14 + knot * 40);
          // As it knots, every curve gets pulled through the same tangle point.
          const mx = width * 0.66 + Math.sin(i * 1.3 + frame / 9) * 60 * (1 - knot * 0.5);
          const my = height * 0.62 + Math.cos(i * 1.7 + frame / 11) * 50 * (1 - knot * 0.5);
          const c1x = ax + 240 * (1 - knot) + (mx - ax) * knot;
          const c1y = ay + (k1 + wob) * (1 - knot) + (my - ay) * knot;
          const c2x = tx - 280 * (1 - knot) + (mx - tx) * knot;
          const c2y = ty + (k2 - wob) * (1 - knot) + (my - ty) * knot;
          return (
            <path
              key={i}
              d={`M ${ax} ${ay} C ${c1x} ${c1y}, ${c2x} ${c2y}, ${tx} ${ty}`}
              fill="none"
              stroke={t.ink}
              strokeWidth={2.5}
              strokeLinecap="round"
              pathLength={1}
              strokeDasharray="1 1"
              strokeDashoffset={1 - prog}
              opacity={0.8}
            />
          );
        })}
      </svg>
      {PARTS.map((p, i) => {
        const pop = useSpring(beat(p.at), {damping: 11, stiffness: 240, mass: 0.6});
        const wob = Math.sin(frame / 4 + i * 2) * 4 * knot;
        const isFile = p.name.includes('.');
        return (
          <div
            key={p.name}
            style={{
              position: 'absolute',
              left: px(p),
              top: py(p),
              transform: `translate(-50%, -50%) translateY(${(1 - pop) * -140}px) rotate(${wob + (1 - pop) * 8}deg)`,
              opacity: Math.min(1, pop * 3),
              background: isFile ? t.bg : t.ink,
              color: isFile ? t.ink : t.bg,
              border: `3px solid ${t.ink}`,
              borderRadius: 14,
              padding: sq ? '14px 22px' : '18px 30px',
              fontFamily: isFile ? mono : inter,
              fontWeight: 600,
              fontSize: sq ? 27 : 38,
              letterSpacing: isFile ? 0 : '-0.02em',
              whiteSpace: 'nowrap',
            }}
          >
            {p.name}
          </div>
        );
      })}
    </>
  );
};
