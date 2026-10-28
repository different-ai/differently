// Differently console: a thin client over the public API. Every action uses
// the signed-in key, so the console can do nothing the key cannot.
"use strict";
const $ = (s) => document.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
let me = null;

function key() { try { return sessionStorage.getItem("dly-key") || ""; } catch { return ""; } }
async function call(method, path, body) {
  const res = await fetch(path, { method, headers: { Authorization: "Bearer " + key(), "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data.error && `${data.error.code}: ${data.error.message}`) || res.statusText);
  return data;
}
function table(cols, rows) {
  if (!rows.length) return "<p class='tag'>Nothing here yet.</p>";
  return `<div class="scroll"><table><tr>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}</tr>${rows.map((r) => `<tr>${r.map((v) => `<td>${v}</td>`).join("")}</tr>`).join("")}</table></div>`;
}
const status = (s) => `<span class="${s === "running" || s === "healthy" ? "ok" : s === "failed" || s === "unavailable" ? "bad" : "warn"}">${esc(s)}</span>`;

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
  async pools() {
    const pools = await call("GET", "/v1/admin/pools");
    return `<h1>Pools</h1>${table(["Pool", "Adapter", "Trust", "Enabled", "Draining", "Active", "Free CPU", "Health", ""], pools.items.map((p) => [esc(p.name), esc(p.adapter), esc(p.trust), p.enabled, p.draining, p.active,
        p.capacity && p.capacity.bounded ? `${p.capacity.freeCPU}/${p.capacity.totalCPU}` : "provider", status(p.health && p.health.status), `<button data-drain="${esc(p.name)}" data-on="${p.draining}">${p.draining ? "Undrain" : "Drain"}</button>`]))}`;
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
  const view = views[name] || views.overview;
  try { $("#view").innerHTML = await view(); } catch (e) { $("#view").innerHTML = `<p class="error">${esc(e.message)}</p>`; }
}

document.addEventListener("click", async (e) => {
  const del = e.target.dataset && e.target.dataset.del;
  const drain = e.target.dataset && e.target.dataset.drain;
  try {
    if (del && confirm(`Delete ${del}?`)) { await call("DELETE", "/v1/sandboxes/" + encodeURIComponent(del)); render(); }
    if (drain) { await call("POST", `/v1/admin/pools/${encodeURIComponent(drain)}/${e.target.dataset.on === "true" ? "undrain" : "drain"}`); render(); }
  } catch (err) { alert(err.message); }
});
document.addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    if (e.target.id === "exec-form") {
      const out = $("#exec-out"); out.hidden = false; out.textContent = "running…";
      const r = await call("POST", `/v1/sandboxes/${encodeURIComponent($("#exec-id").value)}/exec`, { command: $("#exec-cmd").value });
      out.textContent = `exit ${r.exitCode}\n${r.stdout}${r.stderr}`;
    }
    if (e.target.id === "org-form") { await call("POST", "/v1/admin/organizations", { name: $("#org-name").value, defaultProject: "default" }); render(); }
    if (e.target.id === "login-form") {
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
  $("#login").hidden = true; $("#nav").hidden = false;
  document.querySelectorAll(".admin").forEach((a) => (a.hidden = !me.operator));
  render();
}
boot();
