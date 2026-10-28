// Every cut sits on a 120 BPM grid: one beat is 30 frames at 60 fps.
export const BPM = 120;
export const FPS = 60;
export const BEAT = (FPS * 60) / BPM;
export const beat = (n: number) => Math.round(n * BEAT);

// Scene start beats; the score (audio/score.py) reads the same numbers from scenes.json.
import scenes from './scenes.json';
export const SCENES = scenes as unknown as Record<string, [number, number]>;
export const TOTAL_BEATS = Math.max(...Object.values(SCENES).map(([s, l]) => s + l));
