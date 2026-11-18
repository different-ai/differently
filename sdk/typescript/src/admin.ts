import type { HttpClient } from "./http.js";
import type {
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
  PutStackParams,
  Secret,
  SetTemplateParams,
  Stack,
  SuspendOrganizationParams,
  SuspendOrganizationResult,
  Template,
  TrustReport,
  UpdateOrganizationParams,
  UpdatePoolParams,
  UsePoolResult,
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

  /** Registers a pool at runtime. Credentials are references (env, file, or a stored `secret`), never values. */
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

  /**
   * Sends new sandboxes to this pool and drains every other one; running sandboxes stay.
   * An external pool must be approved by the global policy, or approved here.
   */
  use(name: string, params: { approveExternal?: boolean } = {}): Promise<UsePoolResult> {
    return this.#http.json<UsePoolResult>({ method: "POST", path: `/v1/admin/pools/${enc(name)}/use`, json: params });
  }

  /** Removes an API-registered pool. Pools from the config file cannot be deleted here. */
  async delete(name: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/admin/pools/${enc(name)}` });
  }
}

/** Provider credentials stored in Differently, sealed with the server's secrets key. */
export class AdminSecrets {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(): Promise<Secret[]> {
    return (await this.#http.json<Page<Secret>>({ method: "GET", path: "/v1/admin/secrets" })).items ?? [];
  }

  /** Stores or replaces a value. Pools using it switch to the new value without a restart. */
  put(name: string, value: string): Promise<Secret> {
    return this.#http.json<Secret>({ method: "PUT", path: `/v1/admin/secrets/${enc(name)}`, json: { value } });
  }

  /** Fails while a pool uses the secret. */
  async delete(name: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/admin/secrets/${enc(name)}` });
  }
}

/** Application stacks this installation watches: their services and each one's health. */
export class AdminStacks {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  async list(): Promise<Stack[]> {
    return (await this.#http.json<Page<Stack>>({ method: "GET", path: "/v1/admin/stacks" })).items ?? [];
  }

  get(name: string): Promise<Stack> {
    return this.#http.json<Stack>({ method: "GET", path: `/v1/admin/stacks/${enc(name)}` });
  }

  /** Registers or replaces a stack. The result carries a first probe of every component. */
  put(name: string, params: PutStackParams): Promise<Stack> {
    return this.#http.json<Stack>({ method: "PUT", path: `/v1/admin/stacks/${enc(name)}`, json: params });
  }

  /** Probes the stack now instead of waiting for the next round. */
  check(name: string): Promise<Stack> {
    return this.#http.json<Stack>({ method: "POST", path: `/v1/admin/stacks/${enc(name)}/check` });
  }

  /** Stops watching the stack. Its services are not touched. */
  async delete(name: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/admin/stacks/${enc(name)}` });
  }
}

/**
 * Templates set on a running server: one name for each pool's own image or snapshot.
 * `client.templates.list()` lists them, with the config file's. A template the config
 * file declares is changed there; these calls refuse it.
 */
export class AdminTemplates {
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
  }

  /**
   * Sets what the named pools boot for a template, creating it if needed. It merges:
   * pools not named in `images` keep theirs. Applies from the next placement, with no restart.
   */
  set(name: string, params: SetTemplateParams): Promise<Template> {
    return this.#http.json<Template>({ method: "PUT", path: `/v1/admin/templates/${enc(name)}`, json: params });
  }

  /** Stops offering the template on one pool. A template's last image cannot be removed; delete the template. */
  removeImage(name: string, pool: string): Promise<Template> {
    return this.#http.json<Template>({ method: "DELETE", path: `/v1/admin/templates/${enc(name)}/images/${enc(pool)}` });
  }

  /** Removes the template. A request that names it afterwards has the name passed to the backend as given. */
  async delete(name: string): Promise<void> {
    await this.#http.json<void>({ method: "DELETE", path: `/v1/admin/templates/${enc(name)}` });
  }
}

/** Operator-only endpoints. Every call needs a key with the `admin` scope. */
export class Admin {
  readonly organizations: AdminOrganizations;
  readonly projects: AdminProjects;
  readonly pools: AdminPools;
  readonly secrets: AdminSecrets;
  readonly stacks: AdminStacks;
  readonly templates: AdminTemplates;
  readonly #http: HttpClient;

  constructor(http: HttpClient) {
    this.#http = http;
    this.organizations = new AdminOrganizations(http);
    this.projects = new AdminProjects(http);
    this.pools = new AdminPools(http);
    this.secrets = new AdminSecrets(http);
    this.stacks = new AdminStacks(http);
    this.templates = new AdminTemplates(http);
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
