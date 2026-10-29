import { randomUUID } from "node:crypto";
import { DifferentlyError } from "./errors.js";
import type { HttpClient } from "./http.js";
import { readSse } from "./sse.js";
import type {
  CreateSandboxParams,
  ExecEvent,
  ExecOptions,
  ExecResult,
  FileInfo,
  ListSandboxesParams,
  Page,
  PlacementExplanation,
  PortUrl,
  SandboxInfo,
  SandboxStatus,
  Snapshot,
  UpdateSandboxParams,
} from "./types.js";

/** The server holds one wait request open for at most this long. */
const MAX_WAIT_CALL_SECONDS = 300;
const DEFAULT_WAIT_SECONDS = 300;
const TERMINAL: ReadonlySet<SandboxStatus> = new Set(["terminated", "failed"]);

export interface WaitOptions {
  /** Total time to wait before throwing a `timeout` error. Default 300. */
  timeoutSeconds?: number;
  signal?: AbortSignal;
}

export interface CreateOptions extends WaitOptions {
  /** Wait until the sandbox is running. Default true. */
  wait?: boolean;
  /** Makes retries safe. One is generated per call when omitted. */
  idempotencyKey?: string;
}

export interface ForkOptions extends WaitOptions {
  /** Wait until the fork is running. Default true. */
  wait?: boolean;
}

const sandboxPath = (id: string) => `/v1/sandboxes/${encodeURIComponent(id)}`;

/**
 * Polls the server's long-poll wait endpoint until the sandbox reaches
 * `status`. Throws `invalid_state` if it ends in another terminal state and
 * `timeout` if the deadline passes.
 */
export async function waitForStatus(
  http: HttpClient,
  id: string,
  status: SandboxStatus,
  options: WaitOptions = {},
): Promise<SandboxInfo> {
  const total = options.timeoutSeconds ?? DEFAULT_WAIT_SECONDS;
  const deadline = Date.now() + total * 1000;
  for (;;) {
    const seconds = Math.min(Math.max(Math.ceil((deadline - Date.now()) / 1000), 1), MAX_WAIT_CALL_SECONDS);
    const info = await http.json<SandboxInfo>({
      method: "GET",
      path: `${sandboxPath(id)}/wait`,
      query: { status, timeoutSeconds: seconds },
      timeoutMs: (seconds + 30) * 1000,
      signal: options.signal,
    });
    if (info.status === status) return info;
    const details = { sandboxId: id, status: info.status, ...(info.statusReason ? { statusReason: info.statusReason } : {}) };
    if (TERMINAL.has(info.status)) {
      const reason = info.statusReason ? ` (${info.statusReason})` : "";
      throw new DifferentlyError({
        code: "invalid_state",
        message: `sandbox ${id} is ${info.status}${reason}, not ${status}`,
        details,
      });
    }
    if (Date.now() >= deadline) {
      throw new DifferentlyError({
        code: "timeout",
        message: `sandbox ${id} is still ${info.status} after ${total}s waiting for ${status}`,
        details,
      });
    }
  }
}

async function settle(sandbox: Sandbox, options: { wait?: boolean } & WaitOptions): Promise<Sandbox> {
  if (options.wait === false || sandbox.status === "running") return sandbox;
  return sandbox.wait("running", options);
}

function execBody(command: string | readonly string[], options: ExecOptions) {
  return {
    ...(typeof command === "string" ? { command } : { args: [...command] }),
    env: options.env,
    cwd: options.cwd,
    stdin: options.stdin,
    timeoutSeconds: options.timeoutSeconds,
  };
}

function toExecEvent(event: string, data: string): ExecEvent {
  const payload = JSON.parse(data) as { stream?: string; data?: string; exitCode?: number; error?: string };
  const type = event === "stdout" || event === "stderr" || event === "exit" || event === "error" ? event : undefined;
  if (type === "exit" || (!type && payload.exitCode !== undefined)) return { type: "exit", exitCode: payload.exitCode ?? -1 };
  if (type === "error" || (!type && payload.error)) return { type: "error", error: payload.error ?? "unknown error" };
  return { type: type ?? (payload.stream === "stderr" ? "stderr" : "stdout"), data: payload.data ?? "" };
}

/** Files inside a running sandbox. */
export class SandboxFiles {
  readonly #http: HttpClient;
  readonly #path: string;

  constructor(http: HttpClient, sandboxId: string) {
    this.#http = http;
    this.#path = `${sandboxPath(sandboxId)}/files`;
  }

  /** Lists a directory. Without a path the backend's default directory is listed. */
  async list(path?: string): Promise<FileInfo[]> {
    const page = await this.#http.json<Page<FileInfo>>({ method: "GET", path: this.#path, query: { path } });
    return page.items ?? [];
  }

  /** Reads a file as bytes. */
  read(path: string): Promise<Uint8Array> {
    return this.#http.bytes({ method: "GET", path: `${this.#path}/content`, query: { path }, headers: { Accept: "*/*" } });
  }

  /** Reads a UTF-8 text file. */
  async readText(path: string): Promise<string> {
    return new TextDecoder().decode(await this.read(path));
  }

  /** Creates or replaces a file. Strings are written as UTF-8. */
  async write(path: string, content: string | Uint8Array | ArrayBuffer): Promise<void> {
    const body = typeof content === "string" ? content : content instanceof Uint8Array ? content : new Uint8Array(content);
    await this.#http.json<void>({ method: "PUT", path: `${this.#path}/content`, query: { path }, body });
  }
}

/**
 * A handle to one sandbox. `info` is the latest state the SDK has seen; every
 * method that returns the sandbox refreshes it.
 */
export class Sandbox {
  info: SandboxInfo;
  readonly files: SandboxFiles;
  readonly #http: HttpClient;

  constructor(http: HttpClient, info: SandboxInfo) {
    this.#http = http;
    this.info = info;
    this.files = new SandboxFiles(http, info.id);
  }

  get id(): string {
    return this.info.id;
  }

  get status(): SandboxStatus {
    return this.info.status;
  }

  get labels(): Record<string, string> {
    return this.info.labels ?? {};
  }

  get #path(): string {
    return sandboxPath(this.info.id);
  }

  async refresh(): Promise<this> {
    return this.#set(await this.#http.json<SandboxInfo>({ method: "GET", path: this.#path }));
  }

  /** Waits until the sandbox reaches `status` (default "running"). */
  async wait(status: SandboxStatus = "running", options: WaitOptions = {}): Promise<this> {
    return this.#set(await waitForStatus(this.#http, this.id, status, options));
  }

  async update(params: UpdateSandboxParams): Promise<this> {
    return this.#set(await this.#http.json<SandboxInfo>({ method: "PATCH", path: this.#path, json: params }));
  }

  /** Requests deletion. The sandbox moves to `terminating`, or straight to `terminated` if still queued. */
  async delete(): Promise<this> {
    return this.#set(await this.#http.json<SandboxInfo>({ method: "DELETE", path: this.#path }));
  }

  /**
   * Runs a command and returns its output. A string runs through `/bin/sh -c`;
   * an array runs the program directly. A non-zero exit code is not an error.
   */
  exec(command: string | readonly string[], options: ExecOptions = {}): Promise<ExecResult> {
    const timeoutMs =
      options.timeoutSeconds === undefined
        ? undefined
        : Math.max(this.#http.timeoutMs, (options.timeoutSeconds + 30) * 1000);
    return this.#http.json<ExecResult>({
      method: "POST",
      path: `${this.#path}/exec`,
      json: execBody(command, options),
      timeoutMs,
      signal: options.signal,
    });
  }

  /**
   * Runs a command and yields output as it arrives, ending with an `exit`
   * event (or an `error` event if the backend failed mid-run). Breaking out of
   * the loop closes the connection.
   */
  async *execStream(command: string | readonly string[], options: ExecOptions = {}): AsyncGenerator<ExecEvent, void, undefined> {
    const response = await this.#http.stream({
      method: "POST",
      path: `${this.#path}/exec`,
      query: { stream: "1" },
      json: execBody(command, options),
      headers: { Accept: "text/event-stream" },
      signal: options.signal,
    });
    if (!response.body) throw new DifferentlyError({ code: "connection_error", message: "exec stream has no body" });
    for await (const message of readSse(response.body)) {
      const event = toExecEvent(message.event, message.data);
      yield event;
      if (event.type === "exit" || event.type === "error") return;
    }
    throw new DifferentlyError({ code: "connection_error", message: "exec stream ended before the command finished" });
  }

  stop(): Promise<this> {
    return this.#lifecycle("stop");
  }

  start(): Promise<this> {
    return this.#lifecycle("start");
  }

  pause(): Promise<this> {
    return this.#lifecycle("pause");
  }

  /** Counts as activity for idle auto-stop, and wakes an idled sandbox. */
  keepalive(): Promise<this> {
    return this.#lifecycle("keepalive");
  }

  resume(): Promise<this> {
    return this.#lifecycle("resume");
  }

  /** Captures a disk (default) or memory snapshot. */
  snapshot(params: { kind?: "disk" | "memory" } = {}): Promise<Snapshot> {
    return this.#http.json<Snapshot>({ method: "POST", path: `${this.#path}/snapshots`, json: params });
  }

  /** Snapshots this sandbox and starts a copy from it, by default waiting until it runs. */
  async fork(options: ForkOptions = {}): Promise<Sandbox> {
    const info = await this.#http.json<SandboxInfo>({ method: "POST", path: `${this.#path}/fork` });
    return settle(new Sandbox(this.#http, info), options);
  }

  /** Returns a reachable URL for a port the sandbox listens on. */
  exposePort(port: number): Promise<PortUrl> {
    return this.#http.json<PortUrl>({ method: "POST", path: `${this.#path}/ports`, json: { port } });
  }

  /** Explains where the sandbox was (or would be) placed and why. */
  explainPlacement(): Promise<PlacementExplanation> {
    return this.#http.json<PlacementExplanation>({ method: "GET", path: `${this.#path}/placement` });
  }

  toJSON(): SandboxInfo {
    return this.info;
  }

  async #lifecycle(action: "stop" | "start" | "pause" | "resume" | "keepalive"): Promise<this> {
    return this.#set(await this.#http.json<SandboxInfo>({ method: "POST", path: `${this.#path}/${action}` }));
  }

  #set(info: SandboxInfo): this {
    this.info = info;
    return this;
  }
}

export class Sandboxes {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /**
   * Creates a sandbox and, unless `wait: false`, waits until it is running.
   * Creation is retried safely under a per-call idempotency key.
   */
  async create(params: CreateSandboxParams = {}, options: CreateOptions = {}): Promise<Sandbox> {
    const info = await this.#http.json<SandboxInfo>({
      method: "POST",
      path: "/v1/sandboxes",
      json: params,
      headers: { "Idempotency-Key": options.idempotencyKey ?? randomUUID() },
      retry: true,
      signal: options.signal,
    });
    return settle(new Sandbox(this.#http, info), options);
  }

  async get(id: string): Promise<Sandbox> {
    return new Sandbox(this.#http, await this.#http.json<SandboxInfo>({ method: "GET", path: sandboxPath(id) }));
  }

  /** Returns one page. Use `iterate` to walk every page. */
  async list(params: ListSandboxesParams = {}): Promise<Page<Sandbox>> {
    const page = await this.#http.json<Page<SandboxInfo>>({
      method: "GET",
      path: "/v1/sandboxes",
      query: {
        projectId: params.projectId,
        orgId: params.orgId,
        status: Array.isArray(params.status) ? params.status.join(",") : params.status,
        label: params.labels ? Object.entries(params.labels).map(([k, v]) => `${k}=${v}`) : undefined,
        pool: params.pool,
        limit: params.limit,
        cursor: params.cursor,
      },
    });
    const items = (page.items ?? []).map((info) => new Sandbox(this.#http, info));
    return page.nextCursor ? { items, nextCursor: page.nextCursor } : { items };
  }

  /** Yields every matching sandbox, following pagination cursors. */
  async *iterate(params: ListSandboxesParams = {}): AsyncGenerator<Sandbox, void, undefined> {
    let cursor = params.cursor;
    do {
      const page = await this.list({ ...params, cursor });
      yield* page.items;
      cursor = page.nextCursor;
    } while (cursor);
  }

  async update(id: string, params: UpdateSandboxParams): Promise<Sandbox> {
    return new Sandbox(this.#http, await this.#http.json<SandboxInfo>({ method: "PATCH", path: sandboxPath(id), json: params }));
  }

  async delete(id: string): Promise<Sandbox> {
    return new Sandbox(this.#http, await this.#http.json<SandboxInfo>({ method: "DELETE", path: sandboxPath(id) }));
  }

  async wait(id: string, status: SandboxStatus = "running", options: WaitOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.#http, await waitForStatus(this.#http, id, status, options));
  }
}
