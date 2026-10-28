import {loadFont as loadInter} from '@remotion/google-fonts/Inter';
import {loadFont as loadMono} from '@remotion/google-fonts/JetBrainsMono';

export const inter = loadInter('normal', {
  weights: ['400', '500', '600', '700'],
  subsets: ['latin'],
}).fontFamily;
export const mono = loadMono('normal', {weights: ['400', '500'], subsets: ['latin']}).fontFamily;

// Tokens from site/src/styles/landing.css: strictly monochrome, no accent color.
export type Tone = 'light' | 'dark';
export const tokens = {
  light: {bg: '#ffffff', surface: '#fafafa', ink: '#0a0a0a', muted: '#6b6b6b', line: '#e8e8e8'},
  dark: {bg: '#0a0a0a', surface: '#111111', ink: '#fafafa', muted: '#8f8f8f', line: '#222222'},
} as const;
