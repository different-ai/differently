// The local admin's page. It reads two things from the server the CLI
// started on this machine: what `differently discover` finds, and what the
// configured orchestrator says. It changes nothing.
"use strict";
const $ = (s) => document.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

function table(cols, rows) {
  return `<div class="scroll"><table><tr>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}</tr>${rows.map((r) => `<tr>${r.map((v) => `<td>${v}</td>`).join("")}</tr>`).join("")}</table></div>`;
}
async function get(path) {
  const res = await fetch(path);
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.json();
}
// next is a guide's address, or a command to copy.
function nextStep(next) {
  const m = /^(.*?)\s*\(?(https?:\/\/[^\s)]+)\)?\s*$/.exec(next);
  if (!m) return `<code>${esc(next)}</code>`;
  const link = `<a href="${esc(m[2])}" target="_blank" rel="noreferrer">${esc(m[2].replace(/^https?:\/\//, ""))}</a>`;
  return m[1] ? `<code>${esc(m[1])}</code> · ${link}` : link;
}

async function orchestrator() {
  const box = $("#orchestrator");
  let o;
  try { o = await get("/local/orchestrator"); } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
  if (!o.reachable) {
    box.innerHTML = `<div class="notice"><strong>None is running at <code>${esc(o.url)}</code>.</strong>
      You do not need one to see what you have. Without one, nothing routes sandboxes, enforces placement policy or quotas, or keeps an audit record.</div>
      <p class="tag">To try one on this machine: <code>differently-server --dev</code>, then “Look again”. To use another: <code>differently login</code>.</p>`;
    return;
  }
  const facts = [["Address", `<code>${esc(o.url)}</code>`], ["Version", esc(o.version)], ["Installation", esc(o.installation || "—")]];
  if (!o.authenticated) {
    box.innerHTML = `<dl class="facts">${facts.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl><div class="notice"><strong>It answers, but this CLI cannot sign in:</strong> ${esc(o.problem)}</div>`;
    return;
  }
  facts.push(["Signed in as", `<code>${esc(o.keyId)}</code> <span class="tag">${esc((o.scopes || []).join(", "))}</span>`]);
  const pools = o.pools.length
    ? table(["Pool", "Trust", "Health"], o.pools.map((p) => [`<code>${esc(p.pool)}</code>`, esc(p.trust), `<span class="${p.health === "healthy" ? "ok" : "warn"}">${esc(p.health)}</span>`]))
    : "<p class='tag'>It has no pools yet, so it has nowhere to run a sandbox.</p>";
  box.innerHTML = `<dl class="facts">${facts.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl>${pools}
    <p><a class="button" href="/dashboard/">Open its dashboard</a> <span class="tag">sandboxes, keys, pools, audit and the trust report, with this CLI's key</span></p>`;
}

async function machine() {
  let d;
  try { d = await get("/local/discover"); } catch (e) { $("#tools").innerHTML = `<p class="error">${esc(e.message)}</p>`; return; }
  $("#tools").innerHTML = table(["Tool", "Version", "Pointed at"], d.tools.map((t) => t.installed
    ? [`<code>${esc(t.name)}</code>`, esc(t.version || "—"), [esc(t.pointedAt || ""), t.note ? `<span class="warn">${esc(t.note)}</span>` : ""].filter(Boolean).join(" · ") || "—"]
    : [`<code>${esc(t.name)}</code>`, "—", "<span class='off'>not installed</span>"]));
  $("#found").innerHTML = d.found.length
    ? table(["Found", "Name", "Detail", "Seen by"], d.found.map((f) => [esc(f.kind), `<code>${esc(f.name)}</code>`, esc(f.detail || "—"), `<code>${esc(f.tool)}</code>`]))
    : "<p class='tag'>Nothing that could host sandboxes.</p>";
  $("#possible").innerHTML = `<ol class="possible">${d.possible.map((p) => `<li><strong>${esc(p.what)}</strong><div class="why">${esc(p.why)}</div><div>${nextStep(p.next)}</div></li>`).join("")}</ol>`;
  $("#commands").textContent = d.commands.join("\n");
  $("#commands-box").hidden = !d.commands.length;
}

const ACTS = { create: ["create", "+ create"], change: ["change", "~ change"], delete: ["delete", "- delete"], blocked: ["blocked", "cannot apply"], extra: ["extra", "not in the file"] };
function planTable(changes) {
  return `<div class="scroll"><table class="plan"><tr><th>What</th><th>Kind</th><th>Name</th><th>Details</th></tr>${changes.map((c) => {
    const [cls, label] = ACTS[c.action] || ["", c.action];
    const details = c.action === "extra" ? "" : `<ul>${(c.details || []).map((d) => `<li>${esc(d)}</li>`).join("")}</ul>`;
    return `<tr><td class="act ${cls}">${esc(label)}</td><td>${esc(c.kind)}</td><td><code>${esc(c.name)}</code></td><td>${details}</td></tr>`;
  }).join("")}</table></div>`;
}
function showSetup(s, note) {
  const box = $("#setup");
  const file = `<code>${esc(s.file)}</code>`;
  if (!s.exists && !s.problem) {
    box.innerHTML = `<div class="notice"><strong>No setup file here.</strong> ${file} does not exist.</div>
      <p class="tag">Create it to keep where work may run in your repository, then “Look again”. Start <code>differently console</code> in the directory that holds it, or name it with <code>-f</code>.
      <a href="https://different-ai.github.io/differently/docs/guides/setup-file/" target="_blank" rel="noreferrer">What goes in it</a></p>`;
    return;
  }
  if (s.problem) {
    box.innerHTML = `<p class="tag">${file}</p><div class="notice"><strong>It could not be compared.</strong> ${esc(s.problem)}</div>`;
    return;
  }
  const rows = s.changes.length ? planTable(s.changes) : "";
  const can = s.toApply > 0 && s.blocked === 0;
  const button = s.toApply > 0 || s.blocked > 0
    ? `<p><button id="apply" class="primary" ${can ? "" : "disabled"}>Apply ${s.toApply} change${s.toApply === 1 ? "" : "s"}</button>
       <span class="tag">${s.blocked > 0 ? "Nothing can be applied while something in the file cannot be." : "The same as <code>differently apply</code>. It never deletes: what the file does not name is left alone."}</span></p>`
    : "";
  box.innerHTML = `<p class="tag">${file}</p>${note || ""}${rows}<p>${esc(s.summary)}</p>${button}`;
  const apply = $("#apply");
  if (apply) apply.addEventListener("click", () => applySetup(s.plan));
}
async function setup() {
  try { showSetup(await get("/local/setup")); } catch (e) { $("#setup").innerHTML = `<p class="error">${esc(e.message)}</p>`; }
}
async function applySetup(plan) {
  const apply = $("#apply");
  apply.disabled = true; apply.textContent = "Applying…";
  let out;
  try {
    const res = await fetch("/local/setup/apply", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ plan }) });
    out = await res.json();
  } catch (e) { out = { applied: 0, problem: e.message }; }
  const note = out.problem
    ? `<div class="notice"><strong>${out.applied ? `Applied ${out.applied}, then stopped.` : "Not applied."}</strong> ${esc(out.problem)}</div>`
    : `<div class="notice"><strong>Applied ${out.applied} change${out.applied === 1 ? "" : "s"}.</strong></div>`;
  if (out.setup) showSetup(out.setup, note); else { await setup(); $("#setup").insertAdjacentHTML("afterbegin", note); }
  orchestrator();
}

function look() { orchestrator(); setup(); machine(); }
$("#refresh").addEventListener("click", look);
look();
