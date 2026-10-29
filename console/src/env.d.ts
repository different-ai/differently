/// <reference types="astro/client" />
interface ImportMetaEnv {
  readonly PUBLIC_CONSOLE_MODE: 'static' | 'server';
  /** Optional default server URL for the static console. */
  readonly PUBLIC_DIFFERENTLY_URL?: string;
}
interface ImportMeta {
  readonly env: ImportMetaEnv;
}
