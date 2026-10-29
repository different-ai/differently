import { DifferentlyError, errorFromResponse } from "./errors.js";

export const SDK_VERSION = "0.1.0";

export type Method = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
export type Query = Record<string, string | number | boolean | string[] | undefined | null>;

export interface ApiRequest {
  method: Method;
  path: string;
  query?: Query;
  /** JSON body. */
  json?: unknown;
  /** Raw body, sent as application/octet-stream. */
  body?: Uint8Array | string;
  headers?: Record<string, string>;
  /** Overrides the default rule that only GET, PUT and DELETE are retried. */
  retry?: boolean;
  /** Overrides the client timeout. `null` disables it. */
  timeoutMs?: number | null;
  signal?: AbortSignal | undefined;
}

export interface HttpConfig {
  apiKey: string;
  baseUrl: string;
  timeoutMs: number;
  maxRetries: number;
  fetch: typeof fetch;
  headers: Record<string, string>;
}

const IDEMPOTENT: ReadonlySet<Method> = new Set(["GET", "PUT", "DELETE"]);
const MAX_RETRY_AFTER_MS = 30_000;

/** Sends authenticated requests, retrying idempotent ones on 429 and 503. */
export class HttpClient {
  readonly baseUrl: string;
  readonly timeoutMs: number;
  readonly #config: HttpConfig;

  constructor(config: HttpConfig) {
    this.#config = config;
    this.baseUrl = trimTrailingSlashes(config.baseUrl);
    this.timeoutMs = config.timeoutMs;
  }

  async json<T>(req: ApiRequest): Promise<T> {
    const { response, attempt } = await this.#send(req);
    const text = await attempt.read(() => response.text());
    return (text ? JSON.parse(text) : undefined) as T;
  }

  async bytes(req: ApiRequest): Promise<Uint8Array> {
    const { response, attempt } = await this.#send(req);
    return new Uint8Array(await attempt.read(() => response.arrayBuffer()));
  }

  /** Opens a streaming response without a client timeout; the caller reads `body`. */
  async stream(req: ApiRequest): Promise<Response> {
    const { response } = await this.#send({ ...req, timeoutMs: null });
    return response;
  }

  url(path: string, query: Query = {}): string {
    const url = new URL(this.baseUrl + path);
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null) continue;
      if (Array.isArray(value)) for (const item of value) url.searchParams.append(key, item);
      else url.searchParams.set(key, String(value));
    }
    return url.toString();
  }

  async #send(req: ApiRequest): Promise<{ response: Response; attempt: Attempt }> {
    const config = this.#config;
    const headers: Record<string, string> = {
      Accept: "application/json",
      "User-Agent": `differently-typescript/${SDK_VERSION}`,
      ...config.headers,
      ...req.headers,
      Authorization: `Bearer ${config.apiKey}`,
    };
    let body: string | Uint8Array | undefined;
    if (req.json !== undefined) {
      body = JSON.stringify(req.json);
      headers["Content-Type"] = "application/json";
    } else if (req.body !== undefined) {
      body = req.body;
      headers["Content-Type"] = "application/octet-stream";
    }
    const url = this.url(req.path, req.query);
    const retryable = req.retry ?? IDEMPOTENT.has(req.method);
    const timeoutMs = req.timeoutMs === undefined ? config.timeoutMs : req.timeoutMs;

    for (let n = 0; ; n++) {
      const attempt = new Attempt(timeoutMs, req.signal);
      const canRetry = retryable && n < config.maxRetries;
      let response: Response;
      try {
        // The API never redirects; refusing keeps the bearer token on this origin.
        response = await config.fetch(url, {
          method: req.method,
          headers,
          body: (body ?? null) as RequestInit["body"],
          signal: attempt.signal,
          redirect: "manual",
        });
      } catch (err) {
        attempt.done();
        const error = attempt.wrap(err);
        if (canRetry && error instanceof DifferentlyError && error.code === "connection_error") {
          await sleep(backoff(n), req.signal);
          continue;
        }
        throw error;
      }
      if (response.ok) return { response, attempt };
      const text = await attempt.read(() => response.text());
      const error = errorFromResponse(response.status, text, response.headers.get("x-request-id"));
      if (canRetry && shouldRetry(error)) {
        await sleep(retryAfter(response) ?? backoff(n), req.signal);
        continue;
      }
      throw error;
    }
  }
}

/**
 * 503 means a provider or the store is briefly unavailable. 429 is retried
 * for rate limiting only: `quota_exceeded` also uses 429 but will not clear
 * by waiting a few seconds.
 */
function shouldRetry(error: DifferentlyError): boolean {
  return error.status === 503 || (error.status === 429 && error.code !== "quota_exceeded");
}

function retryAfter(response: Response): number | undefined {
  const seconds = Number(response.headers.get("retry-after") ?? Number.NaN);
  return Number.isFinite(seconds) && seconds >= 0 ? Math.min(seconds * 1000, MAX_RETRY_AFTER_MS) : undefined;
}

function backoff(attempt: number): number {
  return Math.min(8_000, 250 * 2 ** attempt) * (0.5 + Math.random() / 2);
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** One try of a request: owns its timeout and links the caller's abort signal. */
class Attempt {
  readonly signal: AbortSignal | undefined;
  readonly #timeoutMs: number | null;
  readonly #userSignal: AbortSignal | undefined;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #unlink: (() => void) | undefined;
  #timedOut = false;

  constructor(timeoutMs: number | null, userSignal: AbortSignal | undefined) {
    this.#timeoutMs = timeoutMs;
    this.#userSignal = userSignal;
    if (timeoutMs === null) {
      this.signal = userSignal;
      return;
    }
    const controller = new AbortController();
    this.#timer = setTimeout(() => {
      this.#timedOut = true;
      controller.abort();
    }, timeoutMs);
    if (userSignal?.aborted) controller.abort(userSignal.reason);
    else if (userSignal) {
      const onAbort = () => controller.abort(userSignal.reason);
      userSignal.addEventListener("abort", onAbort, { once: true });
      this.#unlink = () => userSignal.removeEventListener("abort", onAbort);
    }
    this.signal = controller.signal;
  }

  done(): void {
    clearTimeout(this.#timer);
    this.#unlink?.();
  }

  async read<T>(fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (err) {
      throw this.wrap(err);
    } finally {
      this.done();
    }
  }

  /** Caller aborts pass through unchanged; everything else becomes a DifferentlyError. */
  wrap(err: unknown): unknown {
    if (err instanceof DifferentlyError) return err;
    if (this.#timedOut) {
      return new DifferentlyError({ code: "timeout", message: `request timed out after ${this.#timeoutMs}ms`, cause: err });
    }
    if (this.#userSignal?.aborted) return err;
    const reason = err instanceof Error ? (err.cause instanceof Error ? err.cause.message : err.message) : String(err);
    return new DifferentlyError({ code: "connection_error", message: `request failed: ${reason}`, cause: err });
  }
}

// A loop instead of /\/+$/, which backtracks polynomially on long runs of "/".
function trimTrailingSlashes(url: string): string {
  let end = url.length;
  while (end > 0 && url[end - 1] === "/") end--;
  return url.slice(0, end);
}
