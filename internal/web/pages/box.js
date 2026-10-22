"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
const two = n => String(n).padStart(2, "0");
const when = iso => { if (!iso) return ""; const d = new Date(iso);
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`; };
const took = (a, b) => { if (!a) return ""; const s = Math.max(0, Math.round(((b ? new Date(b) : new Date()) - new Date(a)) / 1000));
  return s >= 60 ? Math.floor(s / 60) + " min " + (s % 60) + " s" : s + " s"; };
const box = new URLSearchParams(location.search).get("b") || "";

const NAMES = {canary: "Canary deploy", quick: "Deploy", rollback: "Rollback"};
const LOG = `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><polyline points="7,9 10,12 7,15"/><line x1="12" y1="15" x2="17" y2="15"/></svg>`;
const short = img => String(img || "").replace(/@sha256:([0-9a-f]{12})[0-9a-f]+$/, "@$1");
const runURL = r => "run?b=" + encodeURIComponent(box) + (r ? "&r=" + encodeURIComponent(r) : "");

let paused = false, clickBusy = false, clickErr = "", armed = "", armTimer = 0, lastRows = "", lastLinks = "", last = null;

// dropsBetween: kept tags from the live one back to just before x that dropped something on
// purpose: x's code may still need it.
const dropsBetween = (rows, x) => rows.filter(y => y.Kept && y.Drops && y.Back >= 0 && y.Back < x.Back);

function button(what, label, cls, busy) {
  const on = armed === what;
  return `<button class="${cls}${on ? " confirm" : ""}" data-what="${esc(what)}"${busy ? " disabled" : ""}>${on ? "Click again" : label}</button>`;
}

function render(v) {
  $("ver").textContent = "switchyard " + v.Version;
  document.title = v.Box + " - switchyard";
  $("title").textContent = v.Box;
  $("offline").hidden = v.Online;
  $("online").hidden = !v.Online;
  if (!v.Online) { $("cluster").textContent = v.Cluster || "The cluster"; return; }
  $("sub").textContent = "on " + v.Cluster + " · " + v.Time;
  const links = (v.Links || []).map(l => `<a href="${esc(l.URL)}" target="_blank" rel="noopener">${esc(l.Label)}</a>`).join("");
  if (links !== lastLinks) { $("links").innerHTML = links; lastLinks = links; }
  const err = clickErr || v.Error || "";
  $("err").textContent = err; $("err").hidden = !err;
  $("pause").textContent = v.Pause + " s";
  $("kept").textContent = `The last ${v.Keep} tags that went live are kept:`;

  $("live").textContent = v.Live || "-";
  $("livesub").textContent = v.Subject || "";
  const stopped = !!v.Live && v.Replicas === 0;
  $("pods").innerHTML = stopped ? `stopped: <a href="running">start it on Running</a>` : `${v.Ready} of ${v.Replicas} pods ready`;
  $("repo").textContent = v.Repo || "";
  const managed = (v.Managed || []).map(m => `<div>statefulset ${esc(m.Name)} <code>${m.Missing ? "not found" : esc(short(m.Image))}</code></div>`).join("");
  if ($("managed").innerHTML !== managed) $("managed").innerHTML = managed;
  $("canary").innerHTML = v.Canary ? `Canary <code>${esc(v.Canary.Tag)}</code>: ${v.Canary.Ready} of ${v.Canary.Replicas} pods ready, about ${v.Canary.Share} % of the traffic` : "";

  const busy = v.Busy || clickBusy || stopped;
  const rows = v.Images || [];
  const html = rows.length ? rows.map(x => {
    let act = "";
    if (x.Kept && !x.Live) {
      act = button("rollback:" + x.Tag, x.Newer ? "Go to" : "Roll back", "stop", busy);
    } else if (x.Newer && !x.Live) {
      act = button("canary:" + x.Tag, "Canary", "", busy) + button("quick:" + x.Tag, "Deploy", "", busy);
    }
    const badges = (x.Live ? `<span class="badge live">live</span>` : "") + (x.Canary ? `<span class="badge canary">canary</span>` : "")
      + (x.Kept && !x.Live ? `<span class="badge">kept</span>` : "") + (x.Newer && !x.Kept ? `<span class="badge">new</span>` : "")
      + (x.Drops ? `<span class="badge">dropped</span>` : "") + (x.Missing ? `<span class="badge">not in the registry</span>` : "");
    const meta = (x.Meta || []).map(m => `<span class="meta"><span class="muted">${esc(m.Key)}</span> <code>${esc(m.Value)}</code></span>`).join(" ");
    // only a deploy moves them, a rollback leaves them
    const wants = (x.Newer && !x.Kept && !x.Live ? x.Wants || [] : []).filter(w => w.Moves || w.Problem).map(w => `<div class="muted">${esc(w.Name)} <code>${esc(short(w.Image))}</code> `
      + (w.Problem ? `<span class="failed">${esc(w.Problem)}</span>` : `<span class="badge">moves</span>`) + `</div>`).join("");
    const went = x.At ? `<div class="muted">live ${esc(when(x.At))} by ${esc(x.By)}</div>` : "";
    return `<tr>
      <td><code class="${x.Live ? "live" : ""}">${esc(x.Tag)}</code> ${badges}${went}</td>
      <td>${meta || (x.Version ? `<code>${esc(x.Version)}</code>` : "")}<div class="muted">${esc(x.Subject)}</div>${wants}</td>
      <td class="num hide-s">${esc(when(x.Created))}</td>
      <td class="act">${act}${x.Run ? `<a class="logicon" href="${runURL(x.Run)}" title="the log of its last deploy">${LOG}</a>` : ""}</td></tr>`;
  }).join("") : `<tr><td colspan="4" class="muted">No tags read yet.</td></tr>`;
  if (html !== lastRows) { $("tags").innerHTML = html; lastRows = html; }

  const target = armed.startsWith("rollback:") && rows.find(x => "rollback:" + x.Tag === armed);
  const drops = target ? dropsBetween(rows, target) : [];
  const warn = !target ? "" : drops.length
    ? `${drops.map(y => y.Tag).join(", ")} dropped columns or tables on purpose, and the database is not rolled back: ${target.Tag} probably still uses them and will break. Click again to go ahead.`
    : target.Back > 1 ? `${target.Tag} is ${target.Back} tags back. The database is not rolled back: if a migration since then dropped something ${target.Tag} still uses, it breaks. Click again to go ahead.` : "";
  $("warn").hidden = !warn; $("warn").textContent = warn;

  const r = v.Run;
  const now = v.Busy && r ? `<span class="status running">${esc(NAMES[r.Kind] || r.Kind)}</span> to <code>${esc(r.To)}</code>
    <span class="muted">· ${esc(r.Step)} · ${esc(took(r.Started))}</span><a class="right" href="${runURL("")}">Show the progress</a>` : "";
  $("busy").hidden = !now;
  if ($("busy").dataset.html !== now) { $("busy").innerHTML = now; $("busy").dataset.html = now; }
  const b = !v.Busy && r && r.Status === "failed" && r.Blocked && r.Blocked.length && r.Kind !== "rollback" ? r : null;
  $("blocked").hidden = !b;
  if (b) {
    const list = b.Blocked.map(x => `<li><code>${esc(x)}</code></li>`).join("");
    if ($("blockedlist").innerHTML !== list) $("blockedlist").innerHTML = list;
    const what = "anyway:" + b.Kind + ":" + b.To;
    $("anyway").disabled = busy;
    $("anyway").textContent = armed === what ? "Click again: let them through" : `Deploy ${b.To} anyway`;
    $("anyway").classList.toggle("confirm", armed === what);
    $("anyway").dataset.what = what;
  }
}

// The last view of this page, shown at once while the first poll runs; its buttons wait for it.
const viewKey = "sy-view:" + location.pathname.split("/").pop() + location.search;
function cachedView() { try { const v = JSON.parse(localStorage.getItem(viewKey)); if (v) document.body.classList.add("cached"); return v; } catch (e) { return null; } }
function keepView(v) { document.body.classList.remove("cached"); try { localStorage.setItem(viewKey, JSON.stringify(v)); } catch (e) { /* only this tab's memory */ } }

async function poll() {
  if (document.hidden) { paused = true; return; }
  try {
    const r = await fetch("state?b=" + encodeURIComponent(box), {cache: "no-store"});
    if (r.status === 401) { location.href = "login"; return; }
    if (r.ok) { render(last = await r.json()); keepView(last); }
  } catch (e) { /* switchyard itself unreachable for a moment: keep the last view */ }
  setTimeout(poll, 1000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && paused) { paused = false; poll(); } });

// A click arms the button, a second click within 4 s sends it and goes to the run page. what is
// canary:<tag>, quick:<tag>, rollback:<tag> or anyway:<kind>:<tag>.
async function click(what) {
  if (armed !== what) {
    armed = what; clearTimeout(armTimer);
    armTimer = setTimeout(() => { armed = ""; lastRows = ""; if (last) render(last); }, 4000);
    lastRows = ""; if (last) render(last);
    return;
  }
  armed = ""; clearTimeout(armTimer);
  clickBusy = true; clickErr = ""; lastRows = "";
  if (last) render(last);
  try {
    let [kind, a, b] = what.split(":");
    let url;
    if (kind === "anyway") url = `${a}?b=${encodeURIComponent(box)}&tag=${encodeURIComponent(b)}&allow=1`;
    else url = `${kind}?b=${encodeURIComponent(box)}&tag=${encodeURIComponent(a)}`;
    const r = await fetch(url, {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
    else { location.href = runURL(""); return; }
  } catch (e) { clickErr = "Failed: " + e; }
  clickBusy = false; lastRows = "";
}
$("tags").addEventListener("click", e => {
  const b = e.target.closest("button[data-what]");
  if (b && !b.disabled) click(b.dataset.what);
});
$("anyway").addEventListener("click", () => click($("anyway").dataset.what));

{ const v = cachedView(); if (v) render(v); }
poll();
