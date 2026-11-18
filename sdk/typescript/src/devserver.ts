/** The project a local `differently-server --dev` creates. */
export const DEV_PROJECT_ID = "prj_default";

/**
 * Reports whether `raw` points at this machine: localhost, a *.localhost name
 * or a loopback address (127.0.0.0/8 or ::1). The dev operator token is sent
 * nowhere else; a host that merely contains "localhost" (for example
 * localhost.example.com) is someone else's server.
 */
export function isLoopbackUrl(raw: string): boolean {
  let host: string;
  try {
    // The same parser the requests go through, so this is the host they reach.
    host = new URL(raw).hostname.toLowerCase();
  } catch {
    return false;
  }
  if (host === "localhost" || host.endsWith(".localhost")) return true;
  // URL keeps the brackets on an IPv6 host and writes it in its shortest form.
  if (host.startsWith("[")) return host === "[::1]";
  const octets = host.split(".");
  return octets.length === 4 && octets[0] === "127" && octets.every((o) => /^\d{1,3}$/.test(o) && Number(o) <= 255);
}

/**
 * The operator token a local `differently-server --dev` wrote to
 * ~/.differently/dev/token, or undefined when there is none to read.
 *
 * The built-in modules come from process.getBuiltinModule (Node.js 20.16,
 * 22.3 or newer) rather than an import, so the SDK still loads where there
 * is no file system. Without it, or on any failure, there is no token.
 */
export function readDevToken(): string | undefined {
  try {
    if (typeof process === "undefined" || typeof process.getBuiltinModule !== "function") return undefined;
    const fs = process.getBuiltinModule("node:fs");
    const os = process.getBuiltinModule("node:os");
    const path = process.getBuiltinModule("node:path");
    const token = fs.readFileSync(path.join(os.homedir(), ".differently", "dev", "token"), "utf8").trim();
    return token || undefined;
  } catch {
    return undefined;
  }
}
