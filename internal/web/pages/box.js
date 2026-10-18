"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
const two = n => String(n).padStart(2, "0");
const when = iso => { if (!iso) return ""; const d = new Date(iso);
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`; };
const took = (a, b) => { if (!a) return ""; const s = Math.max(0, Math.round(((b ? new Date(b) : new Date()) - new Date(a)) / 1000));
  return s >= 60 ? Math.floor(s / 60) + " min " + (s % 60) + " s" : s + " s"; };
const box = new URLSearchParams(location.search).get("b") || "";

const STEPS = {
  canary: [["migrate", "migrate"], ["one", "1 pod"], ["half", "half"], ["switch", "switch"], ["after", "after"], ["done", "done"]],
  quick: [["migrate", "migrate"], ["switch", "switch"], ["after", "after"], ["done", "done"]],
  rollback: [["switch", "switch"], ["after", "after"], ["done", "done"]],
};
const NAMES = {canary: "Canary deploy", quick: "Deploy", rollback: "Rollback"};

function steps(r, list) {
  const at = list.findIndex(s => s[0] === r.Step);
  return list.map(([key, label], i) => {
    let c = "";
    if (r.Status === "ok") c = "done";
    else if (i < at) c = "done";
    else if (i === at) c = r.Status === "running" ? "now" : "bad";
    return `<span class="${c}">${esc(label)}</span>`;
  }).join("");
}

let paused = false, clickBusy = false, clickErr = "", armed = "", armTimer = 0, lastLog = "", lastRows = "", lastLinks = "", last = null;

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
  if (!v.Online) { $("cluster").textContent = v.Cluster || "The cluster"; lastLog = ""; return; }
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
  $("canary").innerHTML = v.Canary ? `Canary <code>${esc(v.Canary.Tag)}</code>: ${v.Canary.Ready} of ${v.Canary.Replicas} pods ready, about ${v.Canary.Share} % of the traffic` : "";

  const busy = v.Busy || clickBusy || stopped;
  const rows = v.Images || [];
  const html = rows.length ? rows.map(x => {
    let act = "";
    if (x.Kept && !x.Live) {
      const risky = x.Back > 1 || dropsBetween(rows, x).length > 0;
      act = button("rollback:" + x.Tag, x.Newer ? "Go to" : risky ? `Roll back ${x.Back}` : "Roll back", "stop", busy);
    } else if (x.Newer && !x.Live) {
      act = button("canary:" + x.Tag, "Canary", "", busy) + button("quick:" + x.Tag, "Deploy", "", busy);
    }
    const badges = (x.Live ? `<span class="badge live">live</span>` : "") + (x.Canary ? `<span class="badge canary">canary</span>` : "")
      + (x.Kept && !x.Live ? `<span class="badge">kept</span>` : "") + (x.Newer && !x.Kept ? `<span class="badge">new</span>` : "")
      + (x.Drops ? `<span class="badge">dropped</span>` : "") + (x.Missing ? `<span class="badge">not in the registry</span>` : "");
    const meta = (x.Meta || []).map(m => `<span class="meta"><span class="muted">${esc(m.Key)}</span> <code>${esc(m.Value)}</code></span>`).join(" ");
    const went = x.At ? `<div class="muted">live ${esc(when(x.At))} by ${esc(x.By)}</div>` : "";
    return `<tr>
      <td><code class="${x.Live ? "live" : ""}">${esc(x.Tag)}</code> ${badges}${went}</td>
      <td>${meta || (x.Version ? `<code>${esc(x.Version)}</code>` : "")}<div class="muted">${esc(x.Subject)}</div></td>
      <td class="num hide-s">${esc(when(x.Created))}</td>
      <td class="act">${act}</td></tr>`;
  }).join("") : `<tr><td colspan="4" class="muted">No tags read yet.</td></tr>`;
  if (html !== lastRows) { $("tags").innerHTML = html; lastRows = html; }

  const target = armed.startsWith("rollback:") && rows.find(x => "rollback:" + x.Tag === armed);
  const drops = target ? dropsBetween(rows, target) : [];
  const warn = !target ? "" : drops.length
    ? `${drops.map(y => y.Tag).join(", ")} dropped columns or tables on purpose, and the database is not rolled back: ${target.Tag} probably still uses them and will break. Click again to go ahead.`
    : target.Back > 1 ? `${target.Tag} is ${target.Back} tags back. The database is not rolled back: if a migration since then dropped something ${target.Tag} still uses, it breaks. Click again to go ahead.` : "";
  $("warn").hidden = !warn; $("warn").textContent = warn;

  const r = v.Run;
  $("runcard").hidden = !r;
  if (r) {
    $("runtitle").textContent = (v.Busy ? "" : "Last: ") + (NAMES[r.Kind] || r.Kind) + (r.Allow ? " (anyway)" : "");
    $("status").textContent = r.Status; $("status").className = "status " + r.Status;
    $("what").textContent = `${r.From || "?"} → ${r.To}`;
    $("when").textContent = when(r.Started) + " · " + took(r.Started, r.Finished);
    $("steps").innerHTML = steps(r, STEPS[r.Kind] || []);
    $("message").textContent = r.Message || "";
    $("abortrow").hidden = !(v.Busy && v.Abortable);
    $("abort").textContent = armed === "abort" ? "Click again to abort" : "Abort";
    $("abort").classList.toggle("confirm", armed === "abort");
    const text = (r.Log || []).join("\n") || "(no log)";
    if (text !== lastLog) {
      const pre = $("log"), atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
      pre.textContent = text; lastLog = text;
      if (atEnd) pre.scrollTop = pre.scrollHeight;
    }
  }
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

async function poll() {
  if (document.hidden) { paused = true; return; }
  try {
    const r = await fetch("state?b=" + encodeURIComponent(box), {cache: "no-store"});
    if (r.status === 401) { location.href = "login"; return; }
    if (r.ok) render(last = await r.json());
  } catch (e) { /* switchyard itself unreachable for a moment: keep the last view */ }
  setTimeout(poll, 1000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && paused) { paused = false; poll(); } });

// A click arms the button, a second click within 4 s sends it. what is canary:<tag>,
// quick:<tag>, rollback:<tag>, anyway:<kind>:<tag> or abort.
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
    if (kind === "abort") url = "abort?b=" + encodeURIComponent(box);
    else if (kind === "anyway") url = `${a}?b=${encodeURIComponent(box)}&tag=${encodeURIComponent(b)}&allow=1`;
    else url = `${kind}?b=${encodeURIComponent(box)}&tag=${encodeURIComponent(a)}`;
    const r = await fetch(url, {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
  } catch (e) { clickErr = "Failed: " + e; }
  clickBusy = false; lastRows = "";
}
$("tags").addEventListener("click", e => {
  const b = e.target.closest("button[data-what]");
  if (b && !b.disabled) click(b.dataset.what);
});
$("abort").addEventListener("click", () => click("abort"));
$("anyway").addEventListener("click", () => click($("anyway").dataset.what));

poll();
