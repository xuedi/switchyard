"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
const cpu = m => m < 1000 ? m + "m CPU" : (m / 1000).toFixed(1) + " CPU";
const mem = b => b >= 1 << 30 ? (b / (1 << 30)).toFixed(1) + " GiB" : Math.round(b / (1 << 20)) + " MiB";
const age = iso => { if (!iso) return ""; const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  return s < 60 ? "<1 min" : s < 3600 ? Math.floor(s / 60) + " min" : s < 172800 ? Math.floor(s / 3600) + " h" : Math.floor(s / 86400) + " days"; };
const took = (a, b) => { if (!a) return ""; const s = Math.max(0, Math.round(((b ? new Date(b) : new Date()) - new Date(a)) / 1000));
  return s >= 60 ? Math.floor(s / 60) + " min " + (s % 60) + " s" : s + " s"; };
const DOT = {running: "on", starting: "busy", stopping: "busy", partly: "busy", stopped: ""};

let last = null, lastHTML = "", clickErr = "", armed = "", armTimer = 0, typed = "", paused = false;
const phone = matchMedia("(max-width: 640px)");
let open = new Set();
try { open = new Set(JSON.parse(localStorage.getItem("sy-running-open") || "[]")); } catch (e) { /* none open */ }
const saveOpen = () => { try { localStorage.setItem("sy-running-open", JSON.stringify([...open])); } catch (e) { /* kept in memory */ } };

const cpuN = m => m < 1000 ? m + "m" : (m / 1000).toFixed(1);
function usage(v, o) { return v.Metrics && (o.CPU || o.Mem) ? `${cpu(o.CPU)} · ${mem(o.Mem)}` : ""; }
const wbr = t => esc(t).replace(/:/g, ":<wbr>");
const plural = (k, one, more) => k + " " + (k === 1 ? one : more);

function nsButtons(n) {
  const dis = n.Busy ? " disabled" : "";
  let b = n.CanStart ? `<button data-click="start" data-n="${esc(n.Name)}"${dis}>Start</button>` : "";
  if (!n.CanStop) return b;
  const key = "stop:" + n.Name;
  if (armed === key && n.Production) {
    return b + `<input class="auto" data-typed="${esc(n.Name)}" placeholder="type ${esc(n.Name)}" value="${esc(typed)}" autocomplete="off">
      <button class="stop" data-click="stop" data-n="${esc(n.Name)}"${typed === n.Name && !n.Busy ? "" : " disabled"}>Stop</button>
      <button class="plain" data-cancel="1">Cancel</button>`;
  }
  return b + `<button class="stop${armed === key ? " confirm" : ""}" data-click="stop" data-n="${esc(n.Name)}"${dis}>${armed === key ? "Click again" : "Stop"}</button>`;
}

function wlButton(n, w) {
  const dis = n.Busy ? " disabled" : "";
  const key = "stop:" + n.Name + ":" + w.Obj;
  if (w.Canary) return "";
  if (w.Replicas > 0) return `<button class="stop small${armed === key ? " confirm" : ""}" data-click="stop" data-n="${esc(n.Name)}" data-w="${esc(w.Obj)}"${dis}>${armed === key ? "Click again" : "Stop"}</button>`;
  return `<button class="small" data-click="start" data-n="${esc(n.Name)}" data-w="${esc(w.Obj)}" title="back to ${w.Saved || 1} ${w.Saved > 1 ? "replicas" : "replica"}"${dis}>Start</button>`;
}

// ready is the count with its dot; the state in words only on the way up or down.
function ready(o, quiet) {
  const word = o.State && o.State !== "running" && !(quiet && o.State === "stopped") ? ` <span class="st-${esc(o.State)}">${esc(o.State)}</span>` : "";
  return `<span class="dot ${DOT[o.State] || ""}"></span>${o.Ready}/${o.Replicas}${word}`;
}

function podState(p) {
  return p.Waiting ? `<span class="failed">${esc(p.Waiting)}</span>` : p.Ready ? `<span class="pd ok" title="ready"></span>` : `<span class="running">${esc(p.Phase.toLowerCase())}</span>`;
}

const containers = p => (p.Containers || []).map(c => `<span class="${c.Ready ? "ok" : "failed"}">${c.Ready ? "&#10003;" : "&#10007;"}</span> ${esc(c.Name)}${c.Restarts ? ` <span class="muted">(${c.Restarts})</span>` : ""}`).join(" ");

// A workload with one plain pod is one row: the pod's restarts and age go on it.
function wlRows(v, n, w) {
  const pods = w.Pods || [];
  const merged = pods.length === 1 && !(pods[0].Containers || []).length ? pods[0] : null;
  const restarts = pods.reduce((t, p) => t + p.Restarts, 0);
  const metrics = o => v.Metrics ? `<td class="r num">${o.CPU || o.Mem ? cpuN(o.CPU) : ""}</td><td class="r num">${o.CPU || o.Mem ? mem(o.Mem) : ""}</td>` : "";
  let html = `<tr class="w">
    <td><strong title="${esc(w.Kind)}${merged ? ", pod " + esc(merged.Name) : ""}">${esc(w.Name)}</strong>${w.Canary ? ` <span class="badge canary">canary</span>` : ""}</td>
    <td class="num">${ready(w, true)}${merged && merged.Waiting ? ` <span class="failed">${esc(merged.Waiting)}</span>` : ""}</td>
    <td>${w.Image ? `<code>${wbr(w.Image)}</code>` : ""}${(w.Addresses || []).map(a => `<code class="addr muted">&#8594; ${wbr(a)}</code>`).join("")}</td>
    <td class="r num ${restarts ? "running" : "muted"}">${pods.length ? restarts : ""}</td>
    <td class="r num">${merged ? esc(age(merged.Started)) : ""}</td>
    ${metrics(w)}
    <td class="act">${wlButton(n, w)}</td></tr>`;
  if (merged) return html;
  for (const p of pods) {
    html += `<tr class="pod">
      <td><span class="muted">${esc(p.Name)}</span></td>
      <td>${podState(p)}</td>
      <td>${containers(p)}</td>
      <td class="r num ${p.Restarts ? "running" : "muted"}">${p.Restarts}</td>
      <td class="r num">${esc(age(p.Started))}</td>
      ${metrics(p)}<td></td></tr>`;
  }
  return html;
}

function cronRows(v, n) {
  return (n.CronJobs || []).map(c => `<tr>
    <td><strong>${esc(c.Name)}</strong></td>
    <td colspan="2"><code>${esc(c.Schedule)}</code> &nbsp;${c.Suspend ? `<span class="running">suspended${c.Held ? " by Stop, Start resumes it" : ""}</span>` : !n.Role ? `<span title="switchyard has no Role here">Stop leaves it running</span>` : `<span class="muted">scheduled</span>`}${c.Active ? ` · <span class="running">${c.Active} running</span>` : ""}</td>
    <td colspan="2" class="r num">${c.Last ? esc(age(c.Last)) + " ago" : ""}</td>
    ${v.Metrics ? "<td></td><td></td>" : ""}<td></td></tr>`).join("");
}

// lastRun links the namespace's last Start or Stop to the run page.
function lastRun(n) {
  const r = n.Run;
  return r ? `<a class="lastrun" href="run?s=${encodeURIComponent(n.Name)}" title="${esc(r.Message || "")}">last ${esc(r.Kind)}${r.Target ? " of " + esc(r.Target) : ""}</a> <span class="${esc(r.Status)}">${esc(r.Status)}</span>` : "";
}

function summary(n) {
  const parts = [plural((n.Workloads || []).length, "workload", "workloads")];
  if ((n.CronJobs || []).length) parts.push(plural(n.CronJobs.length, "CronJob", "CronJobs"));
  if (n.Run) parts.push(lastRun(n));
  return parts.join(" · ");
}

function nsCard(v, n) {
  const isOpen = open.has(n.Name);
  const head = `<div class="nshead" data-ns="${esc(n.Name)}">
      <span class="nsname"><span class="chev" aria-hidden="true">${isOpen ? "&#9662;" : "&#9656;"}</span>${esc(n.Name)}</span>
      <span class="num"><span class="dot ${DOT[n.State] || ""}"></span>${n.Ready}/${n.Replicas}</span>
      <span class="muted">${[n.State !== "running" ? `<span class="st-${esc(n.State)}">${esc(n.State)}</span>` : "", usage(v, n), summary(n)].filter(Boolean).join(" · ")}</span>
      <span class="buttons nsact">${nsButtons(n)}</span>
    </div>
    ${n.Busy ? `<div class="muted running busy">${esc(n.Busy)} is running</div>` : ""}`;
  if (!isOpen) return `<div class="card ns">${head}</div>`;
  const m = v.Metrics ? `<th class="r">CPU</th><th class="r">Memory</th>` : "";
  const cols = `<colgroup><col class="c-name"><col class="c-ready"><col class="c-image"><col class="c-restarts"><col class="c-age">${v.Metrics ? `<col class="c-cpu"><col class="c-mem">` : ""}<col class="c-act"></colgroup>`;
  const crons = (n.CronJobs || []).length ? `<table class="wl cron">${cols}
      <thead><tr><th>CronJob</th><th colspan="2">Schedule</th><th colspan="2" class="r">Last run</th>${v.Metrics ? "<th></th><th></th>" : ""}<th></th></tr></thead>
      <tbody>${cronRows(v, n)}</tbody></table>` : "";
  return `<div class="card ns open">${head}
    <table class="wl">${cols}
      <thead><tr><th>Workload</th><th>Ready</th><th>Image</th><th class="r">Restarts</th><th class="r">Age</th>${m}<th></th></tr></thead>
      <tbody>${(n.Workloads || []).map(w => wlRows(v, n, w)).join("")}</tbody>
    </table>
    ${crons}
  </div>`;
}

// The phone: a tile per namespace, the workloads as a list, a dot per pod.
function podDots(w) {
  return (w.Pods || []).map(p => `<span class="pd ${p.Waiting ? "bad" : p.Ready ? "ok" : "wait"}" title="${esc(p.Name)}${p.Restarts ? " · " + p.Restarts + " restarts" : ""}"></span>`).join("");
}

function nsTile(v, n) {
  const isOpen = open.has(n.Name);
  let html = `<div class="card ns tile${isOpen ? " open" : ""}">
    <div class="tilehead" data-ns="${esc(n.Name)}">
      <span class="dot ${DOT[n.State] || ""}"></span>
      <span class="nsname">${esc(n.Name)}</span>
      <span class="chev" aria-hidden="true">${isOpen ? "&#9662;" : "&#9656;"}</span>
      <span class="buttons nsact">${nsButtons(n)}</span>
      <span class="sub">${n.Ready}/${n.Replicas} ready${n.State !== "running" ? ` · <span class="st-${esc(n.State)}">${esc(n.State)}</span>` : ""}${usage(v, n) ? " · " + usage(v, n) : ""}</span>
    </div>
    ${n.Busy ? `<div class="muted running busy">${esc(n.Busy)} is running</div>` : ""}`;
  if (!isOpen) return html + `</div>`;
  html += `<ul class="wlist">` + (n.Workloads || []).map(w => {
    const waiting = (w.Pods || []).filter(p => p.Waiting).map(p => `<div class="failed">${esc(p.Waiting)}</div>`).join("");
    const restarts = (w.Pods || []).reduce((t, p) => t + p.Restarts, 0);
    const info = [w.Image ? `<code>${esc(w.Image)}</code>` : esc(w.Kind)];
    if (usage(v, w)) info.push(usage(v, w));
    if (restarts) info.push(`<span class="running">${plural(restarts, "restart", "restarts")}</span>`);
    return `<li>
      <div class="wrow"><strong>${esc(w.Name)}</strong>${w.Canary ? ` <span class="badge canary">canary</span>` : ""}
        <span class="pods">${podDots(w)}</span><span class="num">${w.Ready}/${w.Replicas}</span>${wlButton(n, w)}</div>
      <div class="muted">${info.join(" · ")}</div>
      ${(w.Addresses || []).length ? `<div class="muted">${w.Addresses.map(a => `<code>${esc(a)}</code>`).join(" ")}</div>` : ""}
      ${waiting}</li>`;
  }).join("") + `</ul>`;
  html += (n.CronJobs || []).map(c => `<div class="cronline"><span class="muted">CronJob</span> <strong>${esc(c.Name)}</strong> <code>${esc(c.Schedule)}</code>
    <span class="muted">${c.Suspend ? `· <span class="running">suspended</span>` : ""}${c.Active ? ` · ${c.Active} running` : ""}${c.Last ? ` · last ${esc(age(c.Last))} ago` : ""}</span></div>`).join("");
  return html + (n.Run ? `<div class="cronline">${lastRun(n)}</div>` : "") + `</div>`;
}

function render(v) {
  $("ver").textContent = "switchyard " + v.Version;
  $("offline").hidden = v.Online;
  if (!v.Online) { $("cluster").textContent = v.Cluster || "The cluster"; $("list").innerHTML = ""; lastHTML = ""; return; }
  $("sub").textContent = "on " + v.Cluster + " · " + v.Time;
  const err = clickErr || v.Error || "";
  $("err").textContent = err; $("err").hidden = !err;
  $("enable").hidden = v.Enabled;
  const names = (v.Namespaces || []).map(n => n.Name);
  $("all").hidden = !names.length;
  $("all").textContent = names.length && names.every(n => open.has(n)) ? "Collapse all" : "Expand all";
  const card = phone.matches ? nsTile : nsCard;
  const html = (v.Namespaces || []).map(n => card(v, n)).join("") ||
    (v.Enabled ? `<div class="card muted">No workloads outside the cluster's own namespaces.</div>` : "");
  if (html === lastHTML) return;
  const focused = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.typed : undefined;
  $("list").innerHTML = html; lastHTML = html;
  if (focused) {
    const el = $("list").querySelector(`input[data-typed="${CSS.escape(focused)}"]`);
    if (el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); }
  }
}

async function poll() {
  if (document.hidden) { paused = true; return; }
  try {
    const r = await fetch("running/state", {cache: "no-store"});
    if (r.status === 401) { location.href = "login"; return; }
    if (r.ok) render(last = await r.json());
  } catch (e) { /* keep the last view */ }
  setTimeout(poll, 2000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && paused) { paused = false; poll(); } });

function rerender() { lastHTML = ""; if (last) render(last); }

function arm(key, expire) {
  armed = key; typed = ""; clearTimeout(armTimer);
  if (expire) armTimer = setTimeout(() => { armed = ""; rerender(); }, 4000);
  rerender();
}

async function send(kind, n, w) {
  clickErr = "";
  let url = `running/${kind}?n=${encodeURIComponent(n)}`;
  if (w) url += "&w=" + encodeURIComponent(w);
  if (kind === "stop" && !w && typed) url += "&typed=" + encodeURIComponent(typed);
  armed = ""; typed = ""; clearTimeout(armTimer);
  try {
    const r = await fetch(url, {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
  } catch (e) { clickErr = "Failed: " + e; }
  rerender();
}

$("list").addEventListener("click", e => {
  if (e.target.closest("[data-cancel]")) { armed = ""; typed = ""; rerender(); return; }
  const b = e.target.closest("button[data-click]");
  const head = e.target.closest("[data-ns]");
  if (!b && head && !e.target.closest("input, a")) {
    const n = head.dataset.ns;
    if (open.has(n)) open.delete(n); else open.add(n);
    saveOpen(); rerender(); return;
  }
  if (!b || b.disabled) return;
  const kind = b.dataset.click, n = b.dataset.n, w = b.dataset.w || "";
  if (kind === "start") return send(kind, n, w);
  const key = "stop:" + n + (w ? ":" + w : "");
  const prod = !w && last && (last.Namespaces || []).some(x => x.Name === n && x.Production);
  if (armed !== key) return arm(key, !prod);
  send(kind, n, w);
});
$("list").addEventListener("input", e => {
  if (e.target.dataset.typed === undefined) return;
  typed = e.target.value.trim();
  const btn = e.target.parentElement.querySelector("button[data-click=stop]");
  if (btn) btn.disabled = typed !== e.target.dataset.typed;
});
$("list").addEventListener("keydown", e => {
  if (e.target.dataset.typed !== undefined && e.key === "Enter" && typed === e.target.dataset.typed) send("stop", typed, "");
});
$("all").addEventListener("click", () => {
  const names = ((last && last.Namespaces) || []).map(n => n.Name);
  open = names.every(n => open.has(n)) ? new Set() : new Set(names);
  saveOpen(); rerender();
});
phone.addEventListener("change", rerender);
$("enablebtn").addEventListener("click", async () => {
  $("enablebtn").disabled = true; clickErr = "";
  try {
    const r = await fetch("running/enable", {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
  } catch (e) { clickErr = "Failed: " + e; }
  $("enablebtn").disabled = false; rerender();
});

poll();
