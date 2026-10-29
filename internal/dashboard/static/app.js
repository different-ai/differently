// Differently console: a thin client over the public API. Every action uses
// the signed-in key, so the console can do nothing the key cannot.
"use strict";
const $ = (s) => document.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const enc = encodeURIComponent;
const when = (t) => (t ? esc(new Date(t).toLocaleString()) : "—");
const SCOPES = ["admin", "org:admin", "sandboxes:read", "sandboxes:write", "sandboxes:exec", "projects:write", "keys:write", "usage:read", "audit:read", "policies:write"];
let me = null;
// flash is shown once above the next rendered view, then dropped. It can hold
// a new token, so it is never logged or stored.
let flash = "";
// can mirrors the server's scope check: admin and org:admin imply tenant scopes.
const can = (s) => me.scopes.includes("admin") || me.scopes.includes(s) || (s !== "admin" && me.scopes.includes("org:admin"));

function key() { try { return sessionStorage.getItem("dly-key") || ""; } catch { return ""; } }
async function call(method, path, body) {
  const res = await fetch(path, { method, headers: { Authorization: "Bearer " + key(), "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error((data.error && `${data.error.code}: ${data.error.message}`) || res.statusText), { status: res.status, code: data.error && data.error.code, details: (data.error && data.error.details) || {} });
  return data;
}
function table(cols, rows) {
  if (!rows.length) return "<p class='tag'>Nothing here yet.</p>";
  return `<div class="scroll"><table><tr>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}</tr>${rows.map((r) => `<tr>${r.map((v) => `<td>${v}</td>`).join("")}</tr>`).join("")}</table></div>`;
}
const status = (s) => `<span class="${s === "running" || s === "healthy" ? "ok" : s === "failed" || s === "unavailable" ? "bad" : "warn"}">${esc(s)}</span>`;
const tokenBox = (t) => `<div class="token"><strong>Store it now; it is not shown again.</strong><div><code id="new-token">${esc(t)}</code><button data-copy>Copy</button></div></div>`;

// Provider kinds the console can add: label, fixed trust (empty: the operator
// declares it), and main config fields as [key, label, choices?, required?].
const kinds = {
  daytona: ["Daytona", "external", [["apiURL", "API URL (default https://app.daytona.io/api)"], ["target", "target region"], ["snapshot", "snapshot"], ["class", "class", ["container", "linux-vm"]]]],
  freestyle: ["Freestyle", "external", [["apiURL", "API URL (default https://api.freestyle.sh)"], ["snapshot", "snapshot"], ["egress", "egress", ["none", "public"]], ["domain", "domain (default style.dev)"]]],
  opensandbox: ["OpenSandbox", "", [["apiURL", "lifecycle API URL", null, true], ["image", "image"], ["templateId", "template id"], ["isolation", "isolation", ["container", "microvm"]], ["runtime", "runtime", ["docker", "kubernetes"]], ["lineEndings", "line endings", ["kept", "stripped"]], ["ports", "port URLs", ["signed"]], ["egress", "egress", ["none", "public", "allowlist"]], ["egressAllow", "egress allowlist (comma-separated hosts)"], ["useServerProxy", "server proxy", ["true", "false"]]]],
  remote: ["Remote plugin", "", [["endpoint", "plugin endpoint URL", null, true]]],
  vercel: ["Vercel Sandbox", "external", [["project", "project ID or name", null, true], ["team", "team ID"], ["region", "region (default iad1)"], ["image", "image"], ["architecture", "architecture", ["amd64", "arm64"]], ["ports", "ports (comma separated)"], ["maxTimeoutSeconds", "max session seconds (default 2700)"]]],
  fly: ["Fly.io Machines", "external", [["app", "Fly app name", null, true], ["region", "region"], ["image", "image"], ["cpuKind", "CPU kind", ["shared", "performance"]], ["suspend", "suspend for pause", ["true", "false"]]]],
};
function pickKind() {
  const [, trust, fields] = kinds[$("#pk-kind").value], t = $("#pk-trust");
  t.value = trust; t.disabled = !!trust;
  $("#pk-fields").innerHTML = fields.map(([k, label, choices, required]) => choices
    ? `<select data-cfg="${k}" title="${label}"><option value="">${label}: default</option>${choices.map((c) => `<option>${c}</option>`).join("")}</select>`
    : `<input data-cfg="${k}" placeholder="${label}" title="${label}"${required ? " required" : ""}>`).join("");
}

const views = {
  async overview() {
    const [sbx, caps, quota] = await Promise.all([call("GET", "/v1/sandboxes?limit=500"), call("GET", "/v1/capabilities"), me.orgId ? call("GET", "/v1/quotas").catch(() => null) : null]);
    const by = {}; sbx.items.forEach((s) => (by[s.status] = (by[s.status] || 0) + 1));
    const cards = ["running", "queued", "provisioning", "stopped", "failed"].map((s) => `<div class="card"><div class="tag">${s}</div><div class="n">${by[s] || 0}</div></div>`).join("");
    const queued = sbx.items.filter((s) => s.status === "queued");
    return `<h1>Overview</h1><div class="cards">${cards}</div>
      <h2>Pools</h2>${table(["Pool", "Adapter", "Trust", "Region", "Health", "Capabilities"], caps.items.map((p) => [esc(p.pool), esc(p.adapter), esc(p.trust), esc(p.region), status(p.health), `<code>${esc((p.capabilities || []).join(" "))}</code>`]))}
      <h2>Queue</h2>${table(["Sandbox", "Classification", "Waiting because"], queued.map((s) => [`<code>${esc(s.id)}</code>`, esc(s.classification), esc(s.statusReason)]))}
      ${quota ? `<h2>Quota</h2><pre>${esc(JSON.stringify(quota, null, 2))}</pre>` : ""}`;
  },
  async sandboxes() {
    const sbx = await call("GET", "/v1/sandboxes?limit=200");
    return `<h1>Sandboxes</h1>
      <form id="exec-form"><input id="exec-id" placeholder="sandbox id" required><input id="exec-cmd" placeholder="command, e.g. uname -a" required><button>Run</button></form><pre id="exec-out" hidden></pre>
      ${table(["ID", "Status", "Pool", "Trust", "Size", "Class", "Created", ""], sbx.items.map((s) => [
        `<code>${esc(s.id)}</code>`, status(s.status), esc(s.pool), esc(s.trust), `${s.resources.cpu} vCPU / ${s.resources.memoryMiB} MiB`, esc(s.classification),
        esc(new Date(s.createdAt).toLocaleString()), s.status === "terminated" || s.status === "failed" ? "" : `<button data-del="${esc(s.id)}">Delete</button>`]))}`;
  },
  async usage() {
    const u = await call("GET", "/v1/usage?groupBy=pool");
    return `<h1>Usage</h1><p class="tag">${esc(u.note)}</p>${table(["Pool", "Sandboxes", "Run hours", "vCPU hours", "Est. cost"], u.lines.map((l) => [esc(l.key), l.sandboxes, (l.runSeconds / 3600).toFixed(2), (l.cpuSeconds / 3600).toFixed(2), l.estimatedCost.toFixed(4)]))}`;
  },
  async audit() {
    const ev = await call("GET", "/v1/audit-events?limit=200");
    const verify = me.operator ? await call("GET", "/v1/admin/audit/verify") : null;
    return `<h1>Audit</h1>${verify ? `<p>Hash chain: ${verify.ok ? "<span class='ok'>intact</span>" : `<span class='bad'>broken at ${verify.brokenAt}: ${esc(verify.problem)}</span>`} (${verify.events} events)</p>` : ""}
      ${table(["#", "Time", "Actor", "Action", "Resource", "Details"], ev.items.slice().reverse().map((e) => [e.seq, esc(new Date(e.time).toLocaleString()), esc(e.actor), esc(e.action), `<code>${esc(e.resource)}</code>`, `<code>${esc(JSON.stringify(e.details || {}))}</code>`]))}`;
  },
  async keys() {
    const [keys, orgs] = await Promise.all([call("GET", "/v1/api-keys"), me.operator ? call("GET", "/v1/admin/organizations") : null]);
    const now = Date.now(), live = (k) => !k.revoked && !(k.expiresAt && new Date(k.expiresAt) <= now), org = {};
    if (orgs) orgs.items.forEach((o) => (org[o.id] = o.name));
    const rows = keys.items.slice().sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt)).map((k) => [
      esc(k.name) + (k.id === me.keyId ? " <span class='tag'>(this console)</span>" : ""), `<code>${esc(k.id)}</code>`, ...(orgs ? [k.orgId ? `<span title="${esc(k.orgId)}">${esc(org[k.orgId] || k.orgId)}</span>` : "operator"] : []),
      esc(k.projectId) || (k.orgId ? "all projects" : "—"), `<code>${esc(k.scopes.join(" "))}</code>`, when(k.createdAt), k.expiresAt ? when(k.expiresAt) : "never", k.lastUsedAt ? when(k.lastUsedAt) : "never",
      k.revoked ? "<span class='bad'>revoked</span>" : live(k) ? "<span class='ok'>active</span>" : "<span class='bad'>expired</span>",
      live(k) ? `<button data-rotate="${esc(k.id)}">Rotate</button> <button data-revoke="${esc(k.id)}">Revoke</button>` : ""]);
    return `<h1>API keys</h1>
      <h2>Create a key</h2><p class="tag">A key can only hold scopes the signed-in key holds.</p>
      <form id="key-form"><input id="nk-name" placeholder="key name" required>
        ${orgs ? `<select id="nk-org" title="organization"><option value="">no organization (operator key)</option>${orgs.items.map((o) => `<option value="${esc(o.id)}">${esc(o.name)} (${esc(o.id)})</option>`).join("")}</select>` : ""}
        <input id="nk-project" placeholder="project id (optional)" value="${esc(me.projectId)}"${me.projectId ? " disabled" : ""}><input id="nk-ttl" type="number" min="1" step="1" placeholder="expires after hours (optional)">
        <div class="checks">${SCOPES.filter(can).map((s) => `<label><input type="checkbox" name="scope" value="${s}"> ${s}</label>`).join("")}</div><button>Create key</button></form>
      <h2>Keys</h2>${table(["Name", "ID", ...(orgs ? ["Org"] : []), "Project", "Scopes", "Created", "Expires", "Last used", "State", ""], rows)}`;
  },
  async pools() {
    const [pools, secrets] = await Promise.all([call("GET", "/v1/admin/pools"), call("GET", "/v1/admin/secrets")]);
    return `<h1>Pools</h1>${table(["Pool", "Adapter", "Trust", "Enabled", "Draining", "Active", "Free CPU", "Health", ""], pools.items.map((p) => [esc(p.name), esc(p.adapter), esc(p.trust), p.enabled, p.draining, p.active,
        p.capacity && p.capacity.bounded ? `${p.capacity.freeCPU}/${p.capacity.totalCPU}` : "provider", status(p.health && p.health.status),
        `<button data-drain="${esc(p.name)}" data-on="${p.draining}">${p.draining ? "Undrain" : "Drain"}</button>${p.enabled ? ` <button data-use="${esc(p.name)}">Use for new sandboxes</button>` : ""}`]))}
      <h2>Add provider</h2><p class="tag">The API key is sealed on the server as a stored credential and the pool refers to it by name; it is never shown again. Leave the key empty to reuse a stored credential.</p>
      <form id="pool-form"><select id="pk-kind" title="provider">${Object.entries(kinds).map(([k, v]) => `<option value="${k}">${v[0]}</option>`).join("")}</select>
        <input id="pk-name" placeholder="pool name" required><select id="pk-trust" title="trust" required><option value="">trust…</option><option>private</option><option>external</option></select>
        <span id="pk-fields" class="contents"></span>
        <input id="pk-secret" placeholder="secret name (default &lt;pool&gt;-key)" pattern="[a-z0-9][a-z0-9._\\-]{0,62}" title="lowercase letters, digits, '.', '_' or '-'"><input id="pk-key" type="password" placeholder="provider API key" autocomplete="new-password"><button>Add provider</button></form>
      <h2>Stored credentials</h2><p class="tag">Values are sealed with the server's secrets key and never returned. Pools that use a credential pick up a replaced value without a restart.</p>
      <form id="secret-form"><input id="sec-name" placeholder="secret name" required pattern="[a-z0-9][a-z0-9._\\-]{0,62}" title="lowercase letters, digits, '.', '_' or '-'"><input id="sec-value" type="password" placeholder="value" autocomplete="new-password" required><button>Store secret</button></form>
      ${table(["Secret", "Fingerprint", "Used by", "Updated", ""], secrets.items.map((s) => [esc(s.name), `<code>${esc(s.fingerprint)}</code>`, esc((s.usedBy || []).join(", ")) || "—", `${when(s.updatedAt)} <span class="tag">${esc(s.updatedBy)}</span>`,
        `<button data-replace="${esc(s.name)}">Replace value</button> <button data-unset="${esc(s.name)}">Delete</button>`]))}`;
  },
  async tenants() {
    const orgs = await call("GET", "/v1/admin/organizations");
    return `<h1>Tenants</h1><form id="org-form"><input id="org-name" placeholder="organization name" required><button>Create organization</button></form>
      ${table(["ID", "Name", "Suspended", "Quota"], orgs.items.map((o) => [`<code>${esc(o.id)}</code>`, esc(o.name), o.suspended ? "<span class='bad'>yes</span>" : "no", `<code>${esc(JSON.stringify(o.quota))}</code>`]))}`;
  },
  async trust() {
    const t = await call("GET", "/v1/admin/trust-report");
    return `<h1>Trust report</h1><p>Telemetry: ${esc(t.telemetry)}. Global external placement: <strong>${esc(t.globalPolicy.external)}</strong>. Audit chain: ${t.auditChain.ok ? "<span class='ok'>intact</span>" : "<span class='bad'>broken</span>"}.</p>
      <h2>Pools and trust boundaries</h2>${table(["Pool", "Adapter", "Trust", "Endpoint", "Credential", "Approved for external work"], t.pools.map((p) => [esc(p.name), esc(p.adapter), esc(p.trust), `<code>${esc(p.endpoint)}</code>`, `<code>${esc(p.credentialRef)}</code>`, p.trust === "external" ? (p.approvedForExternalPlacement ? "<span class='warn'>yes</span>" : "no") : "—"]))}
      <h2>Recent external placements</h2>${table(["Sandbox", "Org", "Pool", "Class", "Created by", "Why"], (t.recentExternalPlacements || []).map((p) => [`<code>${esc(p.sandboxId)}</code>`, esc(p.orgId), esc(p.pool), esc(p.classification), esc(p.createdBy), esc(p.reason)]))}
      <h2>What Differently cannot prove</h2><ul>${t.limitations.map((l) => `<li>${esc(l)}</li>`).join("")}</ul>`;
  },
};

async function render() {
  const name = (location.hash || "#overview").slice(1);
  const view = views[name] || views.overview, note = flash;
  flash = "";
  let html; try { html = await view(); } catch (e) { html = `<p class="error">${esc(e.message)}</p>`; }
  $("#view").innerHTML = (note ? `<div class="notice">${note}</div>` : "") + html;
  if ($("#pk-kind")) pickKind();
}

// storeSecret reads a value out of its input, clears the input and stores it.
async function storeSecret(name, input) {
  const value = input.value; input.value = "";
  const s = await call("PUT", "/v1/admin/secrets/" + enc(name), { value });
  flash = `Stored secret <code>${esc(s.name)}</code> (fingerprint <code>${esc(s.fingerprint)}</code>). Pools that use it pick up the value without a restart.`;
  render();
}
// addProvider stores the API key as a secret, then registers the pool against
// it. If the pool fails, the secret stays: nothing is deleted behind your back.
async function addProvider() {
  const kind = $("#pk-kind").value, name = $("#pk-name").value.trim(), value = $("#pk-key").value.trim(), config = {};
  $("#pk-key").value = "";
  const secret = $("#pk-secret").value.trim() || (value ? `${name}-key` : "");
  if (kinds[kind][1] && !secret) throw new Error(`${kinds[kind][0]} needs an API key, or the name of a stored secret that holds one.`);
  document.querySelectorAll("#pool-form [data-cfg]").forEach((f) => f.value.trim() && (config[f.dataset.cfg] = f.value.trim()));
  if (value) {
    // Check before storing, so a name clash never overwrites a credential a pool already uses.
    if ((await call("GET", "/v1/admin/pools")).items.some((p) => p.name === name)) throw new Error(`Pool ${name} already exists.`);
    const old = (await call("GET", "/v1/admin/secrets")).items.find((s) => s.name === secret);
    if (old && !confirm(`Secret ${secret} already exists${old.usedBy.length ? ` and is used by ${old.usedBy.join(", ")}` : ""}. Replace its value with this key?`)) return;
    await call("PUT", "/v1/admin/secrets/" + enc(secret), { value });
  }
  try {
    await call("POST", "/v1/admin/pools", { name, adapter: kind, trust: $("#pk-trust").value, config, credentials: secret ? { secret } : undefined });
  } catch (err) {
    if (!value) throw err;
    $("#pk-secret").value = secret;
    throw new Error(`The API key was stored as secret ${secret}, but the pool was not added: ${err.message}\n\nNothing was deleted. Fix the form and submit again with the key left empty to reuse ${secret}, or delete it under Stored credentials.`);
  }
  flash = `Added pool <strong>${esc(name)}</strong>${secret ? ` using secret <code>${esc(secret)}</code>` : ""}.`;
  render();
}
// usePool sends new sandboxes to one pool. An external pool the global policy
// does not approve needs an explicit approval first.
async function usePool(name) {
  const path = `/v1/admin/pools/${enc(name)}/use`;
  let r;
  try { r = await call("POST", path, {}); } catch (err) {
    if (err.code !== "invalid_state" || !err.details.pool) throw err;
    if (!confirm(`${name} runs on an external provider and the global policy does not approve it.\n\nApprove ${name} for external placement and send new sandboxes there? Private-only work still never leaves private pools.`)) return;
    r = await call("POST", path, { approveExternal: true });
  }
  flash = `New sandboxes now go to <strong>${esc(r.pool.name)}</strong>. ${r.drained.length ? `Drained: ${esc(r.drained.join(", "))}.` : "No other pool needed draining."} Running sandboxes stay where they are.${r.policy ? ` The global policy now approves ${esc(r.pool.name)} for external placement (external: ${esc(r.policy.external)}).` : ""}`;
  render();
}

document.addEventListener("click", async (e) => {
  const d = e.target.dataset || {};
  try {
    if (d.del && confirm(`Delete ${d.del}?`)) { await call("DELETE", "/v1/sandboxes/" + enc(d.del)); render(); }
    if (d.drain) { await call("POST", `/v1/admin/pools/${enc(d.drain)}/${d.on === "true" ? "undrain" : "drain"}`); render(); }
    if (d.use) await usePool(d.use);
    if (d.replace) {
      const td = e.target.parentElement;
      td.innerHTML = `<form data-replace-form="${esc(d.replace)}"><input type="password" placeholder="new value for ${esc(d.replace)}" autocomplete="new-password" required><button>Save</button><button type="button" data-refresh>Cancel</button></form>`;
      td.querySelector("input").focus();
    }
    if (d.unset && confirm(`Delete secret ${d.unset}? Its value cannot be recovered.`)) { await call("DELETE", "/v1/admin/secrets/" + enc(d.unset)); flash = `Deleted secret <code>${esc(d.unset)}</code>.`; render(); }
    if (d.rotate) e.target.parentElement.innerHTML = `Old key stops ${[["now", 0], ["in 1 hour", 3600], ["in 1 day", 86400]].map(([l, g]) => `<button data-rot="${esc(d.rotate)}" data-grace="${g}">${l}</button>`).join(" ")} <button data-refresh>Cancel</button>`;
    if (d.rot) {
      const self = d.rot === me.keyId, r = await call("POST", `/v1/api-keys/${enc(d.rot)}/rotate`, { graceSeconds: +d.grace }), old = r.replaced;
      // Rotating the console's own key would sign it out; switch to the new one.
      if (self) { try { sessionStorage.setItem("dly-key", r.token); } catch {} me.keyId = r.id; }
      flash = `Rotated <strong>${esc(r.name)}</strong>: new key <code>${esc(r.id)}</code>. The old key <code>${esc(old.id)}</code> ${old.revoked ? "is revoked" : `keeps working until ${when(old.expiresAt)}`}.${self ? " This console now signs in with the new key." : ""}${tokenBox(r.token)}`;
      render();
    }
    if (d.revoke && confirm(d.revoke === me.keyId ? "Revoke the key this console is signed in with? You will be signed out." : `Revoke key ${d.revoke}? Anything using it stops working at once.`)) {
      await call("DELETE", "/v1/api-keys/" + enc(d.revoke));
      if (d.revoke === me.keyId) return $("#logout").click();
      flash = `Revoked key <code>${esc(d.revoke)}</code>.`; render();
    }
    if ("copy" in d) { const t = $("#new-token"); try { await navigator.clipboard.writeText(t.textContent); e.target.textContent = "Copied"; } catch { getSelection().selectAllChildren(t); } }
    if ("refresh" in d) render();
  } catch (err) { alert(err.message); }
});
document.addEventListener("change", (e) => e.target.id === "pk-kind" && pickKind());
document.addEventListener("input", (e) => { if (e.target.id === "pk-name") $("#pk-secret").placeholder = `secret name (default ${e.target.value.trim() || "<pool>"}-key)`; });
document.addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    if (f.id === "exec-form") {
      const out = $("#exec-out"); out.hidden = false; out.textContent = "running…";
      const r = await call("POST", `/v1/sandboxes/${enc($("#exec-id").value)}/exec`, { command: $("#exec-cmd").value });
      out.textContent = `exit ${r.exitCode}\n${r.stdout}${r.stderr}`;
    }
    if (f.id === "org-form") { await call("POST", "/v1/admin/organizations", { name: $("#org-name").value, defaultProject: "default" }); render(); }
    if (f.id === "pool-form") await addProvider();
    if (f.id === "secret-form") await storeSecret($("#sec-name").value.trim(), $("#sec-value"));
    if (f.dataset.replaceForm) await storeSecret(f.dataset.replaceForm, f.querySelector("input"));
    if (f.id === "key-form") {
      const body = { name: $("#nk-name").value.trim(), scopes: [...f.querySelectorAll("[name=scope]:checked")].map((c) => c.value) };
      const org = $("#nk-org") ? $("#nk-org").value : "", project = $("#nk-project").value.trim(), ttl = +$("#nk-ttl").value;
      if (org) body.orgId = org;
      if (project) body.projectId = project;
      if (ttl) body.ttlHours = ttl;
      const k = await call("POST", "/v1/api-keys", body);
      flash = `Created key <strong>${esc(k.name)}</strong> (<code>${esc(k.id)}</code>).${tokenBox(k.token)}`;
      render();
    }
    if (f.id === "login-form") {
      try { sessionStorage.setItem("dly-key", $("#key").value.trim()); } catch {}
      boot();
    }
  } catch (err) { alert(err.message); }
});
$("#logout").addEventListener("click", () => { try { sessionStorage.removeItem("dly-key"); } catch {} location.reload(); });
window.addEventListener("hashchange", render);

async function boot() {
  if (!key()) { $("#login").hidden = false; return; }
  try { me = await call("GET", "/v1/me"); } catch (e) { $("#login").hidden = false; $("#login-error").textContent = e.message; return; }
  me.scopes = me.scopes || [];
  $("#login").hidden = true; $("#nav").hidden = false;
  document.querySelectorAll(".admin").forEach((a) => (a.hidden = !me.operator));
  document.querySelectorAll("[data-scope]").forEach((a) => (a.hidden = !can(a.dataset.scope)));
  render();
}
boot();
