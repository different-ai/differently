"""Music bed + sound effects for the intro, locked to the same 110 BPM grid as the cuts.

Reads ../src/scenes.json for scene start beats, writes ../public/audio/soundtrack.wav
(stereo, 48 kHz, peak-safe; loudness is normalised afterwards with ffmpeg loudnorm).

Run:  python audio/score.py   (needs numpy + scipy)
"""
import json
import os
import wave

import numpy as np
from scipy import signal

from synth import (SR, adsr, bandpass, convolve_reverb, exp_env, highpass, lowpass, mtof, pan, place,
                   reverb_ir, rng, soft_clip, sweep_filter)

HERE = os.path.dirname(os.path.abspath(__file__))
SCENES = json.load(open(os.path.join(HERE, '..', 'src', 'scenes.json')))
BPM = 110
B = 60 / BPM  # seconds per beat
FPB = 3600 / BPM  # video frames per beat at 60 fps (not a whole number)


def frames(beats):
    """Same rounding as beat() in src/time.ts (JavaScript Math.round)."""
    return int(np.floor(beats * FPB + 0.5))
TOTAL = max(s + l for s, l in SCENES.values()) * B
N = int(TOTAL * SR)


def at(scene, b):
    """Absolute seconds for beat b inside a scene."""
    return (SCENES[scene][0] + b) * B


def buf():
    return np.zeros((N, 2))


# ---------------------------------------------------------------- instruments

def kick(level=1.0):
    n = int(0.4 * SR)
    t = np.arange(n) / SR
    f = 55 + 120 * np.exp(-t * 34)
    body = np.sin(2 * np.pi * np.cumsum(f) / SR) * exp_env(n, 0.001, 0.12) * 0.8
    # A short 1-3 kHz beater click so the kick still reads on phone speakers.
    beater = bandpass(rng.standard_normal(n), 1000, 3200) * exp_env(n, 0.0003, 0.012) * 0.55
    return soft_clip((body + beater) * level, 1.4)


def clap():
    n = int(0.35 * SR)
    noise = bandpass(rng.standard_normal(n), 900, 6000)
    env = np.zeros(n)
    for k, o in enumerate([0, 0.009, 0.019]):
        s = int(o * SR)
        env[s:] += exp_env(n - s, 0.0005, 0.006 if k < 2 else 0.09)
    return noise * env * 0.5


def hat(open_=False):
    n = int((0.18 if open_ else 0.05) * SR)
    x = highpass(rng.standard_normal(n), 7500)
    return x * exp_env(n, 0.0003, 0.05 if open_ else 0.012) * 0.22


def mallet(midi, length=0.45, bright=1.0):
    """FM mallet: playful, glassy pluck."""
    n = int(length * SR)
    t = np.arange(n) / SR
    f = mtof(midi)
    mod = np.sin(2 * np.pi * f * 3.5 * t) * 2.2 * bright * np.exp(-t * 18)
    x = np.sin(2 * np.pi * f * t + mod) * exp_env(n, 0.001, 0.16)
    return x * 0.32


def saw_voice(midi, length, cutoff=1800, detune=(-7, 0, 7), attack=0.01, release=0.15, sustain=1.0):
    n = int(length * SR)
    t = np.arange(n) / SR
    x = sum(signal.sawtooth(2 * np.pi * mtof(midi) * 2 ** (c / 1200) * t + rng.uniform(0, 6.28)) for c in detune) / len(detune)
    x = lowpass(x, cutoff)
    return x * adsr(n, attack, 0.2, sustain, release)


def bass(midi, length, cutoff=520):
    n = int(length * SR)
    t = np.arange(n) / SR
    x = signal.sawtooth(2 * np.pi * mtof(midi) * t) * 0.6 + np.sin(2 * np.pi * mtof(midi) * t) * 0.6
    x = lowpass(x, cutoff)
    return x * adsr(n, 0.004, 0.12, 0.7, 0.05) * 0.5


# ---------------------------------------------------------------- sound effects

def pop(midi=84):
    n = int(0.14 * SR)
    t = np.arange(n) / SR
    f = mtof(midi) * (1 + 0.9 * np.exp(-t * 60))
    return np.sin(2 * np.pi * np.cumsum(f) / SR) * exp_env(n, 0.0008, 0.04) * 0.5


def tick():
    n = int(0.04 * SR)
    return bandpass(rng.standard_normal(n), 2500, 9000) * exp_env(n, 0.0002, 0.006) * 0.35


def whoosh(length=0.55, up=True):
    n = int(length * SR)
    x = rng.standard_normal(n)
    x = sweep_filter(x, 400 if up else 5000, 5000 if up else 400, 'low', 48)
    env = np.sin(np.pi * np.linspace(0, 1, n)) ** 2
    return x * env * 0.5


def squelch(midi=50):
    n = int(0.32 * SR)
    t = np.arange(n) / SR
    f = mtof(midi) * (1.8 - 0.9 * t / t[-1]) * (1 + 0.08 * np.sin(2 * np.pi * 22 * t))
    x = signal.sawtooth(2 * np.pi * np.cumsum(f) / SR, 0.3)
    x = sweep_filter(x, 2400, 300, 'low', 24)
    return x * exp_env(n, 0.004, 0.12) * 0.55


def boom():
    n = int(2.4 * SR)
    t = np.arange(n) / SR
    f = 36 + 90 * np.exp(-t * 7)
    sub = np.sin(2 * np.pi * np.cumsum(f) / SR) * exp_env(n, 0.002, 0.7)
    air = lowpass(rng.standard_normal(n), 1800) * exp_env(n, 0.001, 0.25) * 0.3
    return soft_clip(sub + air, 1.2) * 0.9


def keyclick(seed):
    n = int(0.03 * SR)
    r = np.random.default_rng(seed)
    x = bandpass(r.standard_normal(n), 1800 + r.uniform(0, 1500), 8000)
    return x * exp_env(n, 0.0002, 0.005) * r.uniform(0.12, 0.22)


def deny():
    """Two short descending tones and a thud: the 422."""
    out = np.zeros(int(0.5 * SR))
    for k, m in enumerate([55, 49]):
        v = saw_voice(m, 0.16, cutoff=1400, detune=(0,), attack=0.002, release=0.05)
        place2(out, v * 0.45, k * 0.12)
    n = int(0.3 * SR)
    t = np.arange(n) / SR
    thud = np.sin(2 * np.pi * (70 + 80 * np.exp(-t * 25)) * t) * exp_env(n, 0.001, 0.08)
    place2(out, thud * 0.8, 0)
    return out


def lock_click():
    n = int(0.12 * SR)
    a = highpass(rng.standard_normal(n), 2000) * exp_env(n, 0.0002, 0.004) * 0.5
    b = np.zeros(n)
    o = int(0.035 * SR)
    b[o:] = highpass(rng.standard_normal(n - o), 1200) * exp_env(n - o, 0.0002, 0.009) * 0.6
    return a + b


def chime():
    out = np.zeros(int(1.2 * SR))
    place2(out, mallet(88, 1.0, 0.6) * 1.1, 0)
    place2(out, mallet(95, 1.0, 0.6) * 0.9, 0.09)
    return out


def riser(length):
    n = int(length * SR)
    t = np.arange(n) / SR
    noise = sweep_filter(rng.standard_normal(n), 300, 12000, 'low', 64) * (t / t[-1]) ** 2 * 0.45
    tone = np.sin(2 * np.pi * np.cumsum(200 * 2 ** (3 * t / t[-1])) / SR) * (t / t[-1]) ** 3 * 0.18
    return noise + tone


def rattle(length=1.2):
    out = np.zeros(int(length * SR))
    k = 0
    tt = 0.0
    while tt < length - 0.05:
        place2(out, tick() * 1.6, tt)
        tt += 0.045 + 0.02 * np.sin(k)
        k += 1
    return out * np.linspace(0.4, 1, len(out))


def reverse_swell(length=1.6):
    x = boom()[: int(length * SR)] * 0.6 + lowpass(rng.standard_normal(int(length * SR)), 3000) * exp_env(int(length * SR), 0.001, 0.4) * 0.3
    return x[::-1].copy()


def place2(out, clip, t0):
    o = int(t0 * SR)
    e = min(len(out), o + len(clip))
    out[o:e] += clip[: e - o]


# ---------------------------------------------------------------- arrangement

music = buf()
drums = buf()
fx = buf()
kicks = []  # kick times drive the sidechain

A_MIN_PENTA = [57, 60, 62, 64, 67, 69, 72, 74, 76, 79, 81, 84]

# Act 1 (crowd + mine): sparse "vendor jingle" mallets that drift further out of tune as the
# crowd and the glue pile up, over a thin beat. Nothing here is warm on purpose.
act1_end = at('reveal', 0)
glue_from = at('mine', 2.3)
for b in np.arange(0, SCENES['mine'][0] + SCENES['mine'][1], 0.5):
    tt = b * B
    if tt >= act1_end - 0.2:
        break
    step = int(round(b * 2))
    seq = [0, 4, 2, 5, 3, 6, 4, 7]
    note = A_MIN_PENTA[seq[step % 8] + (2 if (step // 16) % 2 else 0)]
    sour = min(1.0, tt / act1_end)
    cents = (np.sin(step * 2.1) * 45 + np.cos(step * 1.3) * 25) * sour  # detune grows with the mess
    if tt >= at('crowd', 0.5):
        place(music, pan(mallet(note + cents / 100, 0.3) * (1.0 if step % 2 == 0 else 0.7), (-0.5, 0.5)[step % 2]), tt, 0.7)
    if step % 2 == 1 and tt >= at('crowd', 3):
        place(drums, hat(), tt, 0.6)
    if step % 4 == 0 and tt >= at('crowd', 2):
        kicks.append(tt)
        place(drums, kick(0.6), tt)
    if step % 8 == 4 and tt >= glue_from:
        place(drums, clap(), tt, 0.45)
# A low, uneasy bass drone under act 1 (A minor), getting wobblier.
for i, root in enumerate([45, 41, 45, 40] * 4):
    tt = i * 4 * B
    if tt >= act1_end - 0.3:
        break
    length = min(4 * B, act1_end - 0.3 - tt)
    place(music, pan(bass(root - 12, length, cutoff=300 + 300 * min(1, tt / act1_end)), 0), tt, 0.7)

# Reveal: a breath, a reverse swell into the logo, then pad and bass carry the next line
# (no dip), and the full groove drops on "The layer above them." with the biggest hit.
logo_hit = at('reveal', 2.4)
drop = at('reveal', 6.2)
place(fx, pan(reverse_swell(logo_hit - at('reveal', 1.2)), 0), at('reveal', 1.2), 0.9)
dr = saw_voice(33, logo_hit - at('reveal', 0), cutoff=220, detune=(-5, 5), attack=0.4, release=0.3) * 0.22
place(music, pan(dr, 0), at('reveal', 0))

CHORDS = [[48, 55, 60, 64], [43, 50, 55, 59], [45, 52, 57, 60], [41, 48, 53, 57]]
groove_end = at('outro', 0.9)
bar = 4 * B
# Logo → drop: warm pad and pulsing bass, no drums yet.
t0 = logo_hit
while t0 < drop - 0.01:
    length = min(bar, drop - t0)
    ch = CHORDS[0]
    pad = sum(saw_voice(m + 12, length + 0.3, cutoff=1300, attack=0.05, release=0.3, sustain=0.8) for m in ch[1:]) * 0.13
    place(music, pan(pad, 0), t0)
    for k in range(int(length / (B / 2))):
        place(music, pan(bass(ch[0] - 12, B / 2 * 0.9, 420), 0), t0 + k * B / 2, 0.55)
    t0 += length
# The drop: full groove from "The layer above them." to the end card.
nbar = int(np.ceil((groove_end - drop) / bar))
for i in range(nbar):
    t0 = drop + i * bar
    ch = CHORDS[i % 4]
    gate_dip = at('diagram', 5) <= t0 < at('diagram', 7.8)
    pad = sum(saw_voice(m + 12, bar + 0.4, cutoff=1800 if not gate_dip else 800, attack=0.06, release=0.4, sustain=0.8) for m in ch[1:]) * 0.13
    place(music, pan(pad, 0), t0)
    for k in range(8):
        tt = t0 + k * B / 2
        if tt >= groove_end:
            break
        place(music, pan(bass(ch[0] - 12 + (12 if k % 4 == 3 else 0), B / 2 * 0.9, 700), 0), tt, 0.75)
    for k in range(16):
        tt = t0 + k * B / 4
        if tt >= groove_end or k % 2:
            continue
        m = ch[1:][k // 2 % 3] + 24
        place(music, pan(mallet(m, 0.3, 0.5), np.sin(k) * 0.6), tt, 0.4)
    for k in range(4):
        tt = t0 + k * B
        if tt >= groove_end:
            break
        kicks.append(tt)
        place(drums, kick(1.0), tt)
        place(drums, pan(hat(open_=True), 0.3), tt + B / 2, 0.7)
        if k % 2 == 1:
            place(drums, clap(), tt, 0.85)
        for h in range(4):
            place(drums, pan(hat(), -0.3), tt + h * B / 4, 0.5 if h % 2 else 0.25)

# End card: the final chord rings out from the lockup.
end_hit = at('outro', 0.4)
for m in [48, 55, 60, 64, 67, 72]:
    place(music, pan(saw_voice(m, 4.0, cutoff=2600, attack=0.01, release=2.6, sustain=0.5) * 0.1, (m % 5 - 2) / 3), end_hit)
place(music, pan(mallet(84, 1.2, 0.5), -0.2), end_hit + B * 1.5, 0.4)
place(music, pan(mallet(88, 1.2, 0.5), 0.2), end_hit + B * 2.25, 0.35)
place(music, pan(mallet(91, 1.6, 0.5), 0), end_hit + B * 3, 0.3)

# ---------------------------------------------------------------- sound effects on picture cues

# Crowd: every card lands with a pop, rising in pitch and piling up faster; the big late
# ones add a soft thud. Same arrival formula as src/scenes/Problem.tsx (src/crowd.json).
CROWD = json.load(open(os.path.join(HERE, '..', 'src', 'crowd.json')))
n_cards = CROWD['cards']['wide']
for i in range(n_cards):
    rank = (i * CROWD['rankStride']) % n_cards
    arrive = frames(CROWD['firstBeat']) + int(np.floor(frames(CROWD['spanBeats']) * (rank / n_cards) ** CROWD['power'] + 0.5))
    dropper = rank < CROWD['droppers']
    land = (arrive + (11 if dropper else 6)) / 60
    note = A_MIN_PENTA[min(len(A_MIN_PENTA) - 1, rank * len(A_MIN_PENTA) // n_cards)] + (0 if dropper else 12)
    place(fx, pan(pop(note), ((i * 7) % 11 - 5) / 6), land, 0.6 if dropper else 0.35 + 0.3 * rank / n_cards)
    if dropper or rank / n_cards >= 0.6:
        n = int(0.18 * SR)
        tt = np.arange(n) / SR
        thud = np.sin(2 * np.pi * (60 + 70 * np.exp(-tt * 30)) * tt) * exp_env(n, 0.001, 0.05)
        place(fx, pan(thud, 0), land, 0.6 if dropper else 0.35)
S = CROWD['snapBeat']
snap = at('crowd', S)
place(fx, pan(whoosh(0.45, up=False), 0), snap - 0.08, 0.7)
for k in range(18):
    place(fx, pan(lock_click(), ((k % 6) - 2.5) / 3), snap + 0.05 + k * 1.2 / 60 * 1.6, 0.18)
for b in [S, S + 1.5, S + 3, S + 4.5]:
    place(fx, pan(tick(), 0), at('crowd', b), 1.2)
place(fx, pan(whoosh(0.35), 0), at('crowd', S + 1.5) - 0.1, 0.5)
for i in range(18):  # padlocks ripple
    place(fx, pan(lock_click(), ((i % 6) - 2.5) / 3), at('crowd', S + 3) + ((i % 6) + i // 6) * 3 / 60, 0.25)
for i in range(5):  # "contact sales" stamps
    place(fx, pan(pop(72 - i * 2), (i - 2) / 3), at('crowd', S + 4.9) + ((i * 3) % 7) * 4 / 60, 0.6)
place(fx, pan(whoosh(0.45), 0.4), at('crowd', S + 6.35), 0.8)  # whip pan

# Mine: parts drop with squelchy glue, then everything is sucked into the knot.
for k, b in enumerate([0, 0.5, 1]):
    place(fx, pan(pop(64 + k * 5), -0.4), at('mine', 0.6) + b * 0.13, 0.5)
for k, b in enumerate([2.3, 3, 3.7, 4.4, 5.1]):
    place(fx, pan(squelch(48 + k * 2), (k - 2) / 4), at('mine', b), 0.8)
for k in range(3):
    place(fx, pan(squelch(44 - k * 3), 0), at('mine', 5.8 + k * 0.35), 0.9)
place(fx, pan(rattle(0.7), 0), at('mine', 6.4), 0.5)
place(fx, pan(reverse_swell(at('mine', 7.75) - at('mine', 7.1)), 0), at('mine', 7.1), 0.7)
n = int(0.25 * SR)
tt = np.arange(n) / SR
place(fx, pan(np.sin(2 * np.pi * (55 + 90 * np.exp(-tt * 30)) * tt) * exp_env(n, 0.001, 0.08), 0), at('mine', 7.75), 0.8)

# Reveal: the logo locks (medium hit), then the biggest hit lands on the drop line.
place(fx, pan(whoosh(0.8), 0), at('reveal', 1.6), 0.6)
place(fx, pan(boom(), 0), logo_hit, 0.6)
place(fx, pan(mallet(84, 1.0, 0.4), 0), logo_hit, 0.5)
place(fx, pan(whoosh(0.5), 0), at('reveal', 3.85), 0.35)
place(fx, pan(tick(), 0), at('reveal', 4.4), 1.0)
place(fx, pan(boom(), 0), drop, 1.0)
crash = highpass(rng.standard_normal(int(1.6 * SR)), 4000) * exp_env(int(1.6 * SR), 0.002, 0.5) * 0.35
place(fx, pan(crash, 0), drop, 0.8)

# Code: keyboard while the call types, a buzz when the vendor line is struck.
for k in range(30):
    place(fx, pan(keyclick(k), np.sin(k) * 0.3), at('code', 0) + 2 / 60 + k * (B * 0.9 - 2 / 60) / 30)
for k in range(10):
    place(fx, pan(keyclick(100 + k), 0.2), at('code', 2.5) + k * B * 0.4 / 10)
for b in [0, 2.5]:
    place(fx, pan(tick(), 0), at('code', b), 1.0)
place(fx, pan(deny()[: int(0.3 * SR)], 0), at('code', 3), 0.6)
place(fx, pan(whoosh(0.3, up=False), 0), at('code', 3.9), 0.4)

# Diagram: pools pop, sandboxes land as they fill to quota, the gate locks, a request is
# denied (camera punches in), the policy flips, the gate opens, overflow lands outside, the
# audit log types.
place(fx, pan(pop(67), -0.6), at('diagram', 0), 0.6)
place(fx, pan(pop(72), 0), at('diagram', 0.3), 0.6)
place(fx, pan(pop(64), 0), at('diagram', 0.5), 0.4)
for i in range(2):
    place(fx, pan(pop(76 + i * 3), 0.6), at('diagram', 0.55 + i * 0.25), 0.5)
for pool in range(4):
    for k in range(4 if pool < 2 else 3):
        fill = 0.9 + (k * 2 + pool) * 0.38 if pool < 2 else 8.7 + (k * 2 + (pool - 2)) * 0.35
        place(fx, pan(pop(84 + k * 2 + pool), 0.5), at('diagram', fill), 0.3)
for b in [2.5, 5, 7.5, 10.3]:
    place(fx, pan(tick(), 0), at('diagram', b), 1.0)
place(fx, pan(pop(79), 0), at('diagram', 2.5), 0.6)  # quotas badge
place(fx, pan(lock_click(), 0.3), at('diagram', 4.6), 0.9)
for b in [5.1, 7.4, 10.1, 12.2]:  # camera moves
    place(fx, pan(whoosh(0.35), 0), at('diagram', b), 0.3)
place(fx, pan(deny(), 0.2), at('diagram', 6.2), 1.0)
for k in range(8):
    place(fx, pan(keyclick(200 + k), 0.3), at('diagram', 7.5) + 14 / 60 + k * 14 / 60 / 8)
place(fx, pan(lock_click(), 0.3), at('diagram', 8.2), 1.0)
place(fx, pan(chime(), 0.3), at('diagram', 8.25), 0.8)
for k, b in enumerate([10.7, 11.1, 11.5]):
    for j in range(6):
        place(fx, pan(keyclick(300 + k * 10 + j), -0.3), at('diagram', b) + j * 0.02)
place(fx, pan(pop(82), 0), at('diagram', 10.4), 0.5)
place(fx, pan(pop(86), 0), at('diagram', 10.9), 0.5)
# Hand-off: the server box fills the screen; a soft rising air, then a low landing.
hand = at('diagram', 13)
place(fx, pan(riser(at('diagram', 15) - hand) * 0.7, 0), hand, 0.7)
place(fx, pan(whoosh(at('diagram', 15) - hand + 0.2), 0), hand, 0.5)

# Route: the mark lifts, three wires branch out, the destinations land, the number counts up.
place(fx, pan(boom(), 0), at('route', 0), 0.4)
place(fx, pan(whoosh(0.5), 0), at('route', 0.2), 0.35)
for k, (b, p) in enumerate([(1.2, -0.6), (1.5, 0), (1.8, 0.6)]):
    place(fx, pan(pop(74 + k * 4), p), at('route', b), 0.6)
for k in range(14):
    place(fx, pan(tick(), 0), at('route', 2.1) + (k / 14) ** 1.6 * (at('route', 2.9) - at('route', 2.1)), 0.5)
place(fx, pan(lock_click(), 0.6), at('route', 2.4), 0.5)

# End card
place(fx, pan(whoosh(0.5), 0), at('outro', 0), 0.4)
place(fx, pan(boom(), 0), end_hit, 0.8)
for i in range(3):
    place(fx, pan(tick(), 0), at('outro', 0.8 + i * 0.5), 1.0)
for i in range(3):
    place(fx, pan(pop(76 + i * 3), (i - 1) / 2), at('outro', 2.2 + i * 0.2), 0.5)

# ---------------------------------------------------------------- mix

def sidechain(n, hits, depth=0.55, release=0.18):
    env = np.ones(n)
    t = np.arange(int(release * 2.5 * SR)) / SR
    shape = 1 - depth * np.exp(-t / (release / 2.5))
    for h in hits:
        o = int(h * SR)
        e = min(n, o + len(shape))
        env[o:e] = np.minimum(env[o:e], shape[: e - o])
    return env

duck = sidechain(N, kicks)[:, None]
ir = reverb_ir(2.4, 0.7)
music_bus = convolve_reverb(music * duck, ir, wet=0.22)
fx_bus = convolve_reverb(fx, reverb_ir(1.2, 0.35), wet=0.12)
drum_bus = highpass(drums.T, 25).T
mix = music_bus * 0.9 + drum_bus * 0.8 + fx_bus * 0.9
# gentle master glue: tame lows, soft clip, fade the very end
mix = soft_clip(mix * 0.9, 1.3)
fade = np.ones(N)
fl = int(0.6 * SR)
fade[-fl:] = np.linspace(1, 0, fl)
mix *= fade[:, None]
mix /= max(1e-9, np.abs(mix).max()) / 0.89

out = os.path.join(HERE, '..', 'public', 'audio', 'soundtrack.raw.wav')
os.makedirs(os.path.dirname(out), exist_ok=True)
with wave.open(out, 'wb') as w:
    w.setnchannels(2)
    w.setsampwidth(2)
    w.setframerate(SR)
    w.writeframes((mix * 32767).astype('<i2').tobytes())
print(f'wrote {out}  {TOTAL:.2f}s')
