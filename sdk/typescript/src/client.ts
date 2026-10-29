import { Admin } from "./admin.js";
import { DifferentlyError } from "./errors.js";
import { HttpClient } from "./http.js";
import { ApiKeys, AuditEvents, Listing, Policies, Projects, Quotas, Snapshots, Usage } from "./resources.js";
import { Sandboxes } from "./sandbox.js";
import type { Classification, PoolCapabilities, Principal, Profile, Template, VersionInfo } from "./types.js";

export const DEFAULT_BASE_URL = "http://localhost:8080";

export interface DifferentlyOptions {
  /** A `dly_...` key. Defaults to the DIFFERENTLY_API_KEY environment variable. */
  apiKey?: string;
  /** Defaults to DIFFERENTLY_URL, then http://localhost:8080. */
  baseUrl?: string;
  /** Per-request timeout in milliseconds. Default 120000. Long polls and streams size their own. */
  timeoutMs?: number;
  /** Retries for idempotent requests on 429 (rate limited), 503 and connection errors. Default 3. */
  maxRetries?: number;
  /** Extra headers sent with every request. */
  headers?: Record<string, string>;
  /** Custom fetch implementation, for proxies or instrumentation. */
  fetch?: typeof fetch;
}

/** Client for the Differently /v1 API. */
export class Differently {
  readonly baseUrl: string;
  readonly sandboxes: Sandboxes;
  readonly snapshots: Snapshots;
  readonly projects: Projects;
  readonly apiKeys: ApiKeys;
  readonly quotas: Quotas;
  readonly usage: Usage;
  readonly auditEvents: AuditEvents;
  readonly policies: Policies;
  /** Pools as tenants see them: trust, region, health and supported features. */
  readonly capabilities: Listing<PoolCapabilities>;
  readonly profiles: Listing<Profile>;
  readonly templates: Listing<Template>;
  readonly classifications: Listing<Classification>;
  /** Operator-only endpoints. */
  readonly admin: Admin;
  readonly #http: HttpClient;

  constructor(options: DifferentlyOptions = {}) {
    const env = typeof process === "undefined" ? {} : process.env;
    const apiKey = options.apiKey || env.DIFFERENTLY_API_KEY;
    if (!apiKey) {
      throw new DifferentlyError({
        code: "unauthenticated",
        message: "missing API key: pass { apiKey } or set DIFFERENTLY_API_KEY",
      });
    }
    const http = new HttpClient({
      apiKey,
      baseUrl: options.baseUrl || env.DIFFERENTLY_URL || DEFAULT_BASE_URL,
      timeoutMs: options.timeoutMs ?? 120_000,
      maxRetries: options.maxRetries ?? 3,
      headers: options.headers ?? {},
      fetch: options.fetch ?? globalThis.fetch.bind(globalThis),
    });
    this.#http = http;
    this.baseUrl = http.baseUrl;
    this.sandboxes = new Sandboxes(http);
    this.snapshots = new Snapshots(http);
    this.projects = new Projects(http);
    this.apiKeys = new ApiKeys(http);
    this.quotas = new Quotas(http);
    this.usage = new Usage(http);
    this.auditEvents = new AuditEvents(http);
    this.policies = new Policies(http);
    this.capabilities = new Listing(http, "/v1/capabilities");
    this.profiles = new Listing(http, "/v1/profiles");
    this.templates = new Listing(http, "/v1/templates");
    this.classifications = new Listing(http, "/v1/classifications");
    this.admin = new Admin(http);
  }

  /** Describes the calling key: organization, project, scopes and operator status. */
  me(): Promise<Principal> {
    return this.#http.json<Principal>({ method: "GET", path: "/v1/me" });
  }

  /** Server version and installation ID. */
  version(): Promise<VersionInfo> {
    return this.#http.json<VersionInfo>({ method: "GET", path: "/version" });
  }
}
