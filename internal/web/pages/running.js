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
const openLogs = new Set();

function usage(v, o) { return v.Metrics && (o.CPU || o.Mem) ? `${cpu(o.CPU)} · ${mem(o.Mem)}` : ""; }

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

function workloadRow(v, n, w) {
  const dis = n.Busy ? " disabled" : "";
  const key = "stop:" + n.Name + ":" + w.Obj;
  let act = "";
  if (!w.Canary && w.Replicas > 0) act = `<button class="stop${armed === key ? " confirm" : ""}" data-click="stop" data-n="${esc(n.Name)}" data-w="${esc(w.Obj)}"${dis}>${armed === key ? "Click again" : "Stop"}</button>`;
  if (!w.Canary && w.Replicas === 0) act = `<button data-click="start" data-n="${esc(n.Name)}" data-w="${esc(w.Obj)}"${dis}>Start</button>`;
  const back = w.Replicas === 0 && w.Saved ? ` <span class="muted">(Start: ${w.Saved})</span>` : "";
  const addr = (w.Addresses || []).map(a => `<code>${esc(a)}</code>`).join(" ");
  let html = `<tr class="w"><td><strong>${esc(w.Name)}</strong> <span class="badge">${esc(w.Kind)}</span>${w.Canary ? ` <span class="badge canary">canary</span>` : ""}</td>
    <td class="num"><span class="st-${esc(w.State)}">${esc(w.State)}</span> ${w.Ready}/${w.Replicas}${back}</td>
    <td>${w.Image ? `<code class="muted">${esc(w.Image)}</code>` : ""} ${addr}</td>
    <td class="num hide-s muted">${usage(v, w)}</td>
    <td class="act">${act}</td></tr>`;
  for (const p of w.Pods || []) {
    const st = p.Waiting ? `<span class="failed">${esc(p.Waiting)}</span>` : p.Ready ? `<span class="ok">ready</span>` : `<span class="running">${esc(p.Phase.toLowerCase())}</span>`;
    const cs = (p.Containers || []).map(c => `${c.Ready ? "&#10003;" : "&#10007;"} ${esc(c.Name)}${c.Restarts ? ` (${c.Restarts} restarts)` : ""}`).join(", ");
    html += `<tr class="pod"><td colspan="5"><span class="muted">${esc(p.Name)}</span> ${st}
      <span class="muted">· ${p.Restarts} restarts · ${esc(age(p.Started))}${w.Pods.length > 1 && usage(v, p) ? " · " + usage(v, p) : ""}${cs ? " · " + cs : ""}</span></td></tr>`;
  }
  return html;
}

function nsCard(v, n) {
  const parts = [`${n.Ready}/${n.Replicas} ready`];
  if (usage(v, n)) parts.push(usage(v, n));
  const crons = (n.CronJobs || []).map(c => `<div class="muted">CronJob <strong>${esc(c.Name)}</strong> <code>${esc(c.Schedule)}</code>
    ${c.Suspend ? `· <span class="running">suspended${c.Held ? " by Stop, Start resumes it" : ""}</span>` : ""}
    ${c.Active ? `· ${c.Active} running` : ""}${c.Last ? ` · last ${esc(age(c.Last))} ago` : ""}
    ${!c.Suspend && !n.Role ? `· <span title="switchyard has no Role here">Stop leaves it running</span>` : ""}</div>`).join("");
  const r = n.Run;
  const run = r ? `<details data-log="${esc(n.Name)}"${openLogs.has(n.Name) ? " open" : ""}>
    <summary>Last ${esc(r.Kind)}${r.Target ? " of " + esc(r.Target) : ""}: <span class="${esc(r.Status)}">${esc(r.Status)}</span>
    <span class="muted">· ${esc(took(r.Started, r.Finished))} · by ${esc(r.By)}${r.Message && r.Status === "failed" ? " · " + esc(r.Message) : ""}</span></summary>
    <pre>${esc((r.Log || []).join("\n"))}</pre></details>` : "";
  return `<div class="card">
    <div class="nshead">
      <span class="nsname"><span class="dot ${DOT[n.State] || ""}"></span>${esc(n.Name)}</span>
      <span class="status st-${esc(n.State)}">${esc(n.State)}</span>
      <span class="muted num">${parts.join(" · ")}</span>
      <span class="buttons nsact">${nsButtons(n)}</span>
    </div>
    ${n.Busy ? `<div class="muted running">${esc(n.Busy)} is running</div>` : ""}
    <table class="wl">${(n.Workloads || []).map(w => workloadRow(v, n, w)).join("")}</table>
    ${crons}${run}
  </div>`;
}

function render(v) {
  $("ver").textContent = "switchyard " + v.Version;
  $("offline").hidden = v.Online;
  if (!v.Online) { $("cluster").textContent = v.Cluster || "The cluster"; $("list").innerHTML = ""; lastHTML = ""; return; }
  $("sub").textContent = "on " + v.Cluster + " · " + v.Time;
  const err = clickErr || v.Error || "";
  $("err").textContent = err; $("err").hidden = !err;
  $("enable").hidden = v.Enabled;
  const html = (v.Namespaces || []).map(n => nsCard(v, n)).join("") ||
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
$("list").addEventListener("toggle", e => {
  const d = e.target.closest("details[data-log]");
  if (!d) return;
  if (d.open) openLogs.add(d.dataset.log); else openLogs.delete(d.dataset.log);
}, true);
$("enablebtn").addEventListener("click", async () => {
  $("enablebtn").disabled = true; clickErr = "";
  try {
    const r = await fetch("running/enable", {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
  } catch (e) { clickErr = "Failed: " + e; }
  $("enablebtn").disabled = false; rerender();
});

poll();
