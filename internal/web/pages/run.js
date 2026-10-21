"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
const two = n => String(n).padStart(2, "0");
const when = iso => { if (!iso) return ""; const d = new Date(iso);
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`; };
const took = (a, b) => { if (!a) return ""; const s = Math.max(0, Math.round(((b ? new Date(b) : new Date()) - new Date(a)) / 1000));
  return s >= 60 ? Math.floor(s / 60) + " min " + (s % 60) + " s" : s + " s"; };

const STEPS = {
  canary: [["migrate", "migrate"], ["one", "1 pod"], ["half", "half"], ["switch", "switch"], ["after", "after"], ["done", "done"]],
  quick: [["migrate", "migrate"], ["switch", "switch"], ["after", "after"], ["done", "done"]],
  rollback: [["switch", "switch"], ["after", "after"], ["done", "done"]],
};
const NAMES = {canary: "Canary deploy", quick: "Deploy", rollback: "Rollback", manual: "Backup", scheduled: "Scheduled backup",
  "pre-restore": "Safety backup", restore: "Restore", index: "Listing", start: "Start", stop: "Stop"};

// b: a box's deploy, n: a backup set's run, s: Running's last Start or Stop. Without r the page
// follows what runs: a restore's safety backup, then the restore.
const q = new URLSearchParams(location.search);
const kind = q.has("b") ? "b" : q.has("n") ? "n" : "s";
const of = q.get(kind) || "";
let fixed = q.get("r") || "";
const tab = document.querySelector(`.tabbar a[href="${kind === "s" ? "running" : "./"}"]`);
if (tab) tab.classList.add("on");

let last = null, lastLog = "", armed = "", armTimer = 0, clickErr = "", paused = false;

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

function title(v, r) {
  if (!r) return "Run of " + of;
  const name = (NAMES[r.Kind] || r.Kind) + (r.Allow ? " anyway" : "");
  if (kind === "b") return `${name} of ${v.Of} to ${r.To}`;
  if (kind === "n") return `${name}${r.TS ? " " + r.TS : ""} of ${v.Of}`;
  return `${name} of ${r.Target || v.Of}`;
}

function render(v) {
  const r = v.Run;
  $("ver").textContent = "switchyard " + v.Version;
  $("back").href = v.Back || "./";
  $("head").textContent = v.Of || of;
  $("sub").textContent = v.Cluster ? "on " + v.Cluster : "";
  document.title = title(v, r) + " - switchyard";
  $("title").textContent = title(v, r);
  const err = clickErr || (!v.Online ? (v.Cluster || "The cluster") + " is offline" : v.Error) || "";
  $("err").textContent = err; $("err").hidden = !err;
  $("runline").hidden = !r;
  if (!r) return;
  $("status").textContent = r.Status; $("status").className = "status " + r.Status;
  $("what").textContent = kind === "b" ? `${r.From || "?"} → ${r.To}` : r.By ? "by " + r.By : "";
  $("when").textContent = when(r.Started) + " · " + took(r.Started, r.Finished);
  const st = kind === "b" ? steps(r, STEPS[r.Kind] || []) : "";
  $("steps").hidden = !st;
  if ($("steps").innerHTML !== st) $("steps").innerHTML = st;
  $("message").textContent = r.Message || "";
  $("abortrow").hidden = !(kind === "b" && v.Running && v.Abortable);
  $("abort").textContent = armed === "abort" ? "Click again to abort" : "Abort";
  $("abort").classList.toggle("confirm", armed === "abort");
  const blocked = kind === "b" && !v.Running && r.Status === "failed" && (r.Blocked || []).length && r.Kind !== "rollback";
  $("blocked").hidden = !blocked;
  if (blocked) {
    const list = r.Blocked.map(x => `<li><code>${esc(x)}</code></li>`).join("");
    if ($("blockedlist").innerHTML !== list) $("blockedlist").innerHTML = list;
    $("anyway").textContent = armed === "anyway" ? "Click again: let them through" : `Deploy ${r.To} anyway`;
    $("anyway").classList.toggle("confirm", armed === "anyway");
  }
  const text = (r.Log || []).join("\n") || "(no log)";
  if (text !== lastLog) {
    const pre = $("log"), atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
    pre.textContent = text; lastLog = text;
    if (atEnd) pre.scrollTop = pre.scrollHeight;
  }
}

// A finished run that the page was asked for does not change: no more polls then.
async function poll() {
  if (document.hidden) { paused = true; return; }
  let again = true;
  try {
    const r = await fetch(`run/state?${kind}=${encodeURIComponent(of)}${fixed ? "&r=" + encodeURIComponent(fixed) : ""}`, {cache: "no-store"});
    if (r.status === 401) { location.href = "login"; return; }
    if (r.ok) {
      render(last = await r.json());
      again = !fixed || last.Running || !last.Online;
    } else clickErr = await r.text();
  } catch (e) { /* switchyard unreachable for a moment: keep the last view */ }
  if (again) setTimeout(poll, 1000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && paused) { paused = false; poll(); } });

function arm(what) {
  armed = what; clearTimeout(armTimer);
  armTimer = setTimeout(() => { armed = ""; if (last) render(last); }, 4000);
  if (last) render(last);
}

async function send(url) {
  armed = ""; clearTimeout(armTimer); clickErr = "";
  try {
    const r = await fetch(url, {method: "POST"});
    if (!r.ok) clickErr = "Failed: " + (await r.text());
  } catch (e) { clickErr = "Failed: " + e; }
  if (last) render(last);
}

$("abort").addEventListener("click", () => {
  if (armed !== "abort") return arm("abort");
  send("abort?b=" + encodeURIComponent(of));
});
$("anyway").addEventListener("click", async () => {
  if (armed !== "anyway") return arm("anyway");
  const r = last.Run;
  await send(`${r.Kind}?b=${encodeURIComponent(of)}&tag=${encodeURIComponent(r.To)}&allow=1`);
  if (!clickErr && fixed) { fixed = ""; history.replaceState(null, "", "run?b=" + encodeURIComponent(of)); poll(); }
});

poll();
