import { createServer, type IncomingHttpHeaders, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import type { SandboxInfo } from "../src/index.js";

export interface Recorded {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: IncomingHttpHeaders;
  body: Buffer;
  json(): any;
}

export type Handler = (req: Recorded, res: ServerResponse) => void | Promise<void>;

/**
 * An in-process HTTP server. Routes are "METHOD /path"; registering a route
 * several times queues responses, and the last one repeats.
 */
export class MockServer {
  readonly requests: Recorded[] = [];
  url = "";
  readonly #routes = new Map<string, Handler[]>();
  readonly #server: Server;

  private constructor() {
    this.#server = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        const url = new URL(req.url ?? "/", "http://mock");
        const body = Buffer.concat(chunks);
        const recorded: Recorded = {
          method: req.method ?? "GET",
          path: decodeURIComponent(url.pathname),
          query: url.searchParams,
          headers: req.headers,
          body,
          json: () => JSON.parse(body.toString("utf8")),
        };
        this.requests.push(recorded);
        const queue = this.#routes.get(`${recorded.method} ${recorded.path}`);
        const handler = queue && (queue.length > 1 ? queue.shift() : queue[0]);
        if (!handler) return sendError(res, 404, "not_found", `no mock for ${recorded.method} ${recorded.path}`);
        Promise.resolve(handler(recorded, res)).catch((err) => {
          res.destroy(err as Error);
        });
      });
    });
  }

  static async start(): Promise<MockServer> {
    const mock = new MockServer();
    await new Promise<void>((resolve) => mock.#server.listen(0, "127.0.0.1", resolve));
    mock.url = `http://127.0.0.1:${(mock.#server.address() as AddressInfo).port}`;
    return mock;
  }

  on(route: string, handler: Handler): this {
    const queue = this.#routes.get(route) ?? [];
    queue.push(handler);
    this.#routes.set(route, queue);
    return this;
  }

  reset(): void {
    this.#routes.clear();
    this.requests.length = 0;
  }

  calls(route: string): Recorded[] {
    return this.requests.filter((r) => `${r.method} ${r.path}` === route);
  }

  async close(): Promise<void> {
    this.#server.closeAllConnections();
    await new Promise<void>((resolve) => this.#server.close(() => resolve()));
  }
}

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  res.writeHead(status, { "Content-Type": "application/json", ...headers });
  res.end(JSON.stringify(body));
}

export function sendError(
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
  extra: { details?: Record<string, string>; headers?: Record<string, string> } = {},
): void {
  sendJson(res, status, { error: { code, message, details: extra.details, requestId: "req_test" } }, extra.headers);
}

export const reply =
  (status: number, body: unknown, headers?: Record<string, string>): Handler =>
  (_req, res) =>
    sendJson(res, status, body, headers);

export const delay = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

export function sandboxInfo(overrides: Partial<SandboxInfo> = {}): SandboxInfo {
  return {
    id: "sbx_1",
    orgId: "org_default",
    projectId: "prj_default",
    status: "running",
    profile: "small",
    resources: { cpu: 1, memoryMiB: 512, diskGiB: 1 },
    classification: "internal",
    priority: 0,
    attempts: 1,
    createdBy: "key:k1",
    createdAt: "2026-10-28T12:00:00Z",
    updatedAt: "2026-10-28T12:00:00Z",
    ...overrides,
  };
}
