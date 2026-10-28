"""Small deterministic synth toolkit for the intro's music bed and sound effects.

Everything is generated from code (numpy + scipy) so the soundtrack is reproducible
and license-free. Stereo float arrays, shape (n, 2), at SR.
"""
import numpy as np
from scipy import signal

SR = 48000
rng = np.random.default_rng(20261028)


def mtof(m):
    return 440.0 * 2 ** ((m - 69) / 12)


def adsr(n, a=0.005, d=0.1, s=0.0, r=0.05, sustain_len=None):
    a_n, d_n, r_n = int(a * SR), int(d * SR), int(r * SR)
    s_n = max(0, n - a_n - d_n - r_n) if sustain_len is None else int(sustain_len * SR)
    env = np.concatenate([
        np.linspace(0, 1, max(a_n, 1), endpoint=False),
        np.linspace(1, s, max(d_n, 1), endpoint=False),
        np.full(s_n, s),
        np.linspace(s, 0, max(r_n, 1)),
    ])
    out = np.zeros(n)
    out[: min(n, len(env))] = env[:n]
    return out


def exp_env(n, attack=0.002, decay=0.2):
    t = np.arange(n) / SR
    return np.minimum(1, t / max(attack, 1e-5)) * np.exp(-t / decay)


def lowpass(x, cutoff, q=0.707):
    b, a = signal.iirfilter(2, min(cutoff, SR * 0.45) / (SR / 2), btype='low', ftype='butter')
    return signal.lfilter(b, a, x)


def highpass(x, cutoff):
    b, a = signal.butter(2, cutoff / (SR / 2), btype='high')
    return signal.lfilter(b, a, x)


def bandpass(x, lo, hi):
    b, a = signal.butter(2, [lo / (SR / 2), min(hi, SR * 0.45) / (SR / 2)], btype='band')
    return signal.lfilter(b, a, x)


def sweep_filter(x, f0, f1, kind='low', blocks=64):
    """Time-varying filter by processing in blocks with interpolated cutoff (state carried)."""
    out = np.zeros_like(x)
    n = len(x)
    edges = np.linspace(0, n, blocks + 1).astype(int)
    zi = None
    for i in range(blocks):
        f = f0 * (f1 / f0) ** (i / max(blocks - 1, 1))
        b, a = signal.butter(2, min(max(f, 20), SR * 0.45) / (SR / 2), btype=kind)
        seg = x[edges[i]:edges[i + 1]]
        if zi is None:
            zi = signal.lfilter_zi(b, a) * 0
        y, zi = signal.lfilter(b, a, seg, zi=zi)
        out[edges[i]:edges[i + 1]] = y
    return out


def pan(x, p=0.0):
    """Equal-power pan, p in [-1, 1]."""
    ang = (p + 1) * np.pi / 4
    return np.stack([x * np.cos(ang), x * np.sin(ang)], axis=1)


def reverb_ir(seconds=2.2, decay=0.6, predelay=0.012, bright=6000):
    n = int(seconds * SR)
    t = np.arange(n) / SR
    ir = np.zeros((n, 2))
    for ch in range(2):
        noise = rng.standard_normal(n)
        noise = lowpass(noise, bright)
        ir[:, ch] = noise * np.exp(-t / decay)
    pd = int(predelay * SR)
    ir = np.concatenate([np.zeros((pd, 2)), ir])
    return ir / np.sqrt(np.sum(ir ** 2, axis=0, keepdims=True))


def convolve_reverb(x, ir, wet=0.25):
    mono = x.mean(axis=1) if x.ndim == 2 else x
    y = np.stack([signal.fftconvolve(mono, ir[:, ch])[: len(mono)] for ch in range(2)], axis=1)
    dry = x if x.ndim == 2 else np.stack([x, x], axis=1)
    return dry + wet * y


def place(buf, clip, t0, gain=1.0):
    """Mix a (n,2) or (n,) clip into buf at time t0 (seconds)."""
    if clip.ndim == 1:
        clip = np.stack([clip, clip], axis=1)
    o = int(round(t0 * SR))
    if o >= len(buf):
        return
    if o < 0:
        clip = clip[-o:]
        o = 0
    e = min(len(buf), o + len(clip))
    buf[o:e] += clip[: e - o] * gain


def soft_clip(x, drive=1.0):
    return np.tanh(x * drive) / np.tanh(drive)
