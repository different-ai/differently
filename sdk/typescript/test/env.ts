import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * Runs `fn` with environment variables set (`undefined` removes one) and puts
 * the previous values back. The client reads its environment when it is
 * constructed, so constructing inside `fn` is enough.
 */
export function withEnv<T>(vars: Record<string, string | undefined>, fn: () => T): T {
  const saved = Object.keys(vars).map((name) => [name, process.env[name]] as const);
  setEnv(Object.entries(vars));
  try {
    return fn();
  } finally {
    setEnv(saved);
  }
}

function setEnv(vars: ReadonlyArray<readonly [string, string | undefined]>): void {
  for (const [name, value] of vars) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
}

/**
 * A fresh home directory, with a dev server's token file when `devToken` is
 * given. Tests never read the real one, which may hold a real token.
 */
export function tempHome(devToken?: string): string {
  const home = mkdtempSync(join(tmpdir(), "differently-sdk-"));
  if (devToken !== undefined) {
    mkdirSync(join(home, ".differently", "dev"), { recursive: true });
    writeFileSync(join(home, ".differently", "dev", "token"), devToken);
  }
  return home;
}

/** Environment that makes `home` the home directory on every platform. */
export const homeEnv = (home: string) => ({ HOME: home, USERPROFILE: home });
