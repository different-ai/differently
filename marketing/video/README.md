# Intro video

A 40-second launch video for Differently, built with [Remotion](https://www.remotion.dev/) and a soundtrack synthesised from code. The beat-by-beat plan and the claims we checked are in [STORYBOARD.md](STORYBOARD.md).

## Build

You need Node 20+, ffmpeg, and Python 3 with `numpy` and `scipy`.

```bash
npm install
PYTHON=python3 audio/build.sh        # writes public/audio/soundtrack.wav at -14 LUFS
npm run render                       # out/differently-intro-16x9.mp4 (1920×1080, 60 fps)
npm run render:square                # out/differently-intro-1x1.mp4 (1080×1080, 60 fps)
npm run studio                       # live preview
```

## How it fits together

- `src/scenes.json`: start beat and length of each scene on a 110 BPM grid. The picture and the score both read it, so cuts and hits stay in sync.
- `src/scenes/*`: one component per act. Each one lays itself out for 16:9 or 1:1 from `useVideoConfig()`.
- `src/crowd.json`: timing of the opening swarm, shared with the score for the same reason.
- `src/kinetic.tsx`: masked word-by-word headlines. They fit to width and cut on the beat.
- `src/theme.ts`: the site's tokens (monochrome, Inter, JetBrains Mono).
- `audio/score.py`: the music bed and every sound effect, placed on picture cues. `audio/synth.py` holds the small synth toolkit.

Generated audio (`public/audio/`) and renders (`out/`) are not committed.
