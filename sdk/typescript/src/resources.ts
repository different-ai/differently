import type { HttpClient } from "./http.js";
import type {
  ApiKey,
  AuditEvent,
  CreateApiKeyParams,
  CreatedApiKey,
  CreateProjectParams,
  ListAuditEventsParams,
  Page,
  PlacementPolicy,
  PlacementPolicyParams,
  Project,
  QuotaReport,
  Snapshot,
  UsageParams,
  UsageReport,
} from "./types.js";

const enc = encodeURIComponent;

/** A read-only collection the server returns in one response. */
export class Listing<T> {
  readonly #http: HttpClient;
  readonly #path: string;

  constructor(http: HttpClient, path: string) {
    this.#http = http;
    this.#path = path;
  }

  async list(): Promise<T[]> {
    return (await this.#http.json<Page<T>>({ method: "GET", path: this.#path })).items ?? [];
  }
}

export class Snapshots {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(): Promise<Snapshot[]> {
    return (await this.#http.json<Page<Snapshot>>({ method: "GET", path: "/v1/snapshots" })).items ?? [];
  }

  get(id: string): Promise<Snapshot> {
    return this.#http.json<Snapshot>({ method: "GET", path: `/v1/snapshots/${enc(id)}` });
  }

  async delete(id: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/snapshots/${enc(id)}` });
  }
}

export class Projects {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** `orgId` is honored for operator keys only. */
  async list(params: { orgId?: string } = {}): Promise<Project[]> {
    return (await this.#http.json<Page<Project>>({ method: "GET", path: "/v1/projects", query: params })).items ?? [];
  }

  create(params: CreateProjectParams): Promise<Project> {
    return this.#http.json<Project>({ method: "POST", path: "/v1/projects", json: params });
  }

  get(id: string): Promise<Project> {
    return this.#http.json<Project>({ method: "GET", path: `/v1/projects/${enc(id)}` });
  }

  async delete(id: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/projects/${enc(id)}` });
  }
}

export class ApiKeys {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** `orgId` is honored for operator keys only. */
  async list(params: { orgId?: string } = {}): Promise<ApiKey[]> {
    return (await this.#http.json<Page<ApiKey>>({ method: "GET", path: "/v1/api-keys", query: params })).items ?? [];
  }

  /** Creates a key. The returned `token` is shown only once. */
  create(params: CreateApiKeyParams): Promise<CreatedApiKey> {
    return this.#http.json<CreatedApiKey>({ method: "POST", path: "/v1/api-keys", json: params });
  }

  get(id: string): Promise<ApiKey> {
    return this.#http.json<ApiKey>({ method: "GET", path: `/v1/api-keys/${enc(id)}` });
  }

  /**
   * Replaces a key with a new one holding the same name, scopes and project, and returns
   * the new `token` once. The old key stays valid for `graceSeconds` (at most a week), or
   * is revoked at once.
   */
  rotate(id: string, params: { graceSeconds?: number } = {}): Promise<CreatedApiKey> {
    return this.#http.json<CreatedApiKey>({ method: "POST", path: `/v1/api-keys/${enc(id)}/rotate`, json: params });
  }

  async revoke(id: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/api-keys/${enc(id)}` });
  }
}

export class Quotas {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** Limits and current usage. Operator keys must pass `orgId`. */
  get(params: { orgId?: string; projectId?: string } = {}): Promise<QuotaReport> {
    return this.#http.json<QuotaReport>({ method: "GET", path: "/v1/quotas", query: params });
  }
}

export class Usage {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** Estimated usage over a window (default: the last 30 days, grouped by pool). */
  get(params: UsageParams = {}): Promise<UsageReport> {
    const time = (v: Date | string | undefined) => (v instanceof Date ? v.toISOString() : v);
    return this.#http.json<UsageReport>({
      method: "GET",
      path: "/v1/usage",
      query: { from: time(params.from), to: time(params.to), groupBy: params.groupBy, orgId: params.orgId },
    });
  }
}

export class AuditEvents {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** Returns one page in ascending sequence order. */
  async list(params: ListAuditEventsParams = {}): Promise<Page<AuditEvent>> {
    const page = await this.#http.json<Page<AuditEvent>>({ method: "GET", path: "/v1/audit-events", query: { ...params } });
    const items = page.items ?? [];
    return page.nextCursor ? { items, nextCursor: page.nextCursor } : { items };
  }

  /** Yields every matching event, following pagination. */
  async *iterate(params: ListAuditEventsParams = {}): AsyncGenerator<AuditEvent, void, undefined> {
    let afterSeq = params.afterSeq;
    for (;;) {
      const page = await this.list({ ...params, afterSeq });
      yield* page.items;
      if (!page.nextCursor) return;
      afterSeq = Number(page.nextCursor);
    }
  }
}

export class Policies {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** Placement policies visible to this key, including the global one. */
  async list(): Promise<PlacementPolicy[]> {
    return (await this.#http.json<Page<PlacementPolicy>>({ method: "GET", path: "/v1/policies" })).items ?? [];
  }

  /** Replaces the policy for a scope: "global", "org:<id>" or "project:<id>". */
  put(scope: string, policy: PlacementPolicyParams): Promise<PlacementPolicy> {
    return this.#http.json<PlacementPolicy>({ method: "PUT", path: `/v1/policies/${enc(scope)}`, json: policy });
  }

  async delete(scope: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/policies/${enc(scope)}` });
  }
}
