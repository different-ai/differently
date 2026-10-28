import type { HttpClient } from "./http.js";
import type {
  Agent,
  AuditVerifyResult,
  CreateOrganizationParams,
  CreateOrganizationResult,
  CreatePoolParams,
  Organization,
  Page,
  Pool,
  Project,
  Quota,
  SandboxInfo,
  SuspendOrganizationParams,
  SuspendOrganizationResult,
  TrustReport,
  UpdateOrganizationParams,
  UpdatePoolParams,
} from "./types.js";

const enc = encodeURIComponent;

export class AdminOrganizations {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(): Promise<Organization[]> {
    return (await this.#http.json<Page<Organization>>({ method: "GET", path: "/v1/admin/organizations" })).items ?? [];
  }

  create(params: CreateOrganizationParams): Promise<CreateOrganizationResult> {
    return this.#http.json<CreateOrganizationResult>({ method: "POST", path: "/v1/admin/organizations", json: params });
  }

  get(id: string): Promise<Organization> {
    return this.#http.json<Organization>({ method: "GET", path: `/v1/admin/organizations/${enc(id)}` });
  }

  update(id: string, params: UpdateOrganizationParams): Promise<Organization> {
    return this.#http.json<Organization>({ method: "PATCH", path: `/v1/admin/organizations/${enc(id)}`, json: params });
  }

  /** Blocks new work for the organization; `terminate: true` also ends its sandboxes. */
  suspend(id: string, params: SuspendOrganizationParams = {}): Promise<SuspendOrganizationResult> {
    return this.#http.json({ method: "POST", path: `/v1/admin/organizations/${enc(id)}/suspend`, json: params });
  }

  resume(id: string, params: { reason?: string } = {}): Promise<SuspendOrganizationResult> {
    return this.#http.json({ method: "POST", path: `/v1/admin/organizations/${enc(id)}/resume`, json: params });
  }
}

export class AdminProjects {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /** Replaces a project's quota. */
  setQuota(id: string, quota: Quota): Promise<Project> {
    return this.#http.json<Project>({ method: "PATCH", path: `/v1/admin/projects/${enc(id)}`, json: { quota } });
  }
}

export class AdminPools {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(): Promise<Pool[]> {
    return (await this.#http.json<Page<Pool>>({ method: "GET", path: "/v1/admin/pools" })).items ?? [];
  }

  get(name: string): Promise<Pool> {
    return this.#http.json<Pool>({ method: "GET", path: `/v1/admin/pools/${enc(name)}` });
  }

  /** Registers a pool at runtime. Credentials are server-side references, never values. */
  create(params: CreatePoolParams): Promise<Pool> {
    return this.#http.json<Pool>({ method: "POST", path: "/v1/admin/pools", json: params });
  }

  update(name: string, params: UpdatePoolParams): Promise<Pool> {
    return this.#http.json<Pool>({ method: "PATCH", path: `/v1/admin/pools/${enc(name)}`, json: params });
  }

  /** Stops new placements on the pool; running sandboxes continue. */
  drain(name: string): Promise<Pool> {
    return this.#http.json<Pool>({ method: "POST", path: `/v1/admin/pools/${enc(name)}/drain` });
  }

  undrain(name: string): Promise<Pool> {
    return this.#http.json<Pool>({ method: "POST", path: `/v1/admin/pools/${enc(name)}/undrain` });
  }

  /** Removes an API-registered pool. Pools from the config file cannot be deleted here. */
  async delete(name: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/admin/pools/${enc(name)}` });
  }
}

export class AdminAgents {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(params: { pool?: string } = {}): Promise<Agent[]> {
    return (await this.#http.json<Page<Agent>>({ method: "GET", path: "/v1/admin/agents", query: params })).items ?? [];
  }
}

/** Operator-only endpoints. Every call needs a key with the `admin` scope. */
export class Admin {
  readonly organizations: AdminOrganizations;
  readonly projects: AdminProjects;
  readonly pools: AdminPools;
  readonly agents: AdminAgents;
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
    this.organizations = new AdminOrganizations(http);
    this.projects = new AdminProjects(http);
    this.pools = new AdminPools(http);
    this.agents = new AdminAgents(http);
  }

  /** Where workloads can go, where they went, and what Differently cannot prove. */
  trustReport(): Promise<TrustReport> {
    return this.#http.json<TrustReport>({ method: "GET", path: "/v1/admin/trust-report" });
  }

  /** Verifies the hash chain of the whole audit log. */
  verifyAudit(): Promise<AuditVerifyResult> {
    return this.#http.json<AuditVerifyResult>({ method: "GET", path: "/v1/admin/audit/verify" });
  }

  /** Terminates any tenant's sandbox. */
  terminateSandbox(id: string, params: { reason?: string } = {}): Promise<SandboxInfo> {
    return this.#http.json<SandboxInfo>({ method: "POST", path: `/v1/admin/sandboxes/${enc(id)}/terminate`, json: params });
  }

  /** Runs one scheduler reconcile pass now. */
  async reconcile(): Promise<void> {
    await this.#http.json<{ ok: boolean }>({ method: "POST", path: "/v1/admin/reconcile" });
  }
}
