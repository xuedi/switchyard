"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
const two = n => String(n).padStart(2, "0");
const when = iso => { if (!iso) return ""; const d = new Date(iso);
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`; };
const took = (a, b) => { if (!a) return ""; const s = Math.max(0, Math.round(((b ? new Date(b) : new Date()) - new Date(a)) / 1000));
  return s >= 60 ? Math.floor(s / 60) + " min " + (s % 60) + " s" : s + " s"; };
const size = n => n >= 1e9 ? (n / 1e9).toFixed(1) + " GB" : n >= 1e6 ? (n / 1e6).toFixed(1) + " MB" : n >= 1e3 ? Math.round(n / 1e3) + " kB" : n + " B";
const params = new URLSearchParams(location.search);
const ns = params.get("n") || "";
const q = "?n=" + encodeURIComponent(ns);
const NAMES = {manual: "Backup", scheduled: "Scheduled backup", "pre-restore": "Safety backup", restore: "Restore", index: "Listing"};

let last = null, clickErr = "", armed = "", armTimer = 0, lastRows = "", lastLog = "", restoring = null, formOpen = false;

async function getJSON(url) {
  const r = await fetch(url, {cache: "no-store"});
  if (r.status === 401) { location.href = "login"; throw new Error("log in"); }
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

function render(v) {
  $("ver").textContent = "switchyard " + v.Version;
  document.title = "Backups of " + ns + " - switchyard";
  $("title").textContent = "Backups of " + ns;
  const noSet = !v.Set || !v.Set.target;
  $("offline").hidden = v.Online || noSet;
  $("online").hidden = !v.Online || noSet;
  if (noSet) { if (!formOpen) openForm(); return; }
  if (!v.Online) { $("cluster").textContent = v.Cluster || "The cluster"; return; }
  $("sub").textContent = "on " + v.Cluster + " · " + v.Time;
  const err = clickErr || v.Error || "";
  $("err").textContent = err; $("err").hidden = !err;
  const s = v.Set;
  $("summary").innerHTML = `${(v.Archives || []).length} runs, ${size(v.Size || 0)} on <code>${esc(s.target)}</code>` +
    (s.schedule ? ` · scheduled <code>${esc(s.schedule)}</code> ${esc(s.timeZone || "UTC")}` : " · by hand only") +
    (s.cold ? " · cold" : "") + (v.IndexAt ? ` · listed ${esc(when(v.IndexAt))}` : "");
  $("now").disabled = $("refresh").disabled = v.Busy;
  $("now").textContent = armed === "now" ? "Click again" : "Back up now";
  $("now").classList.toggle("confirm", armed === "now");

  const rows = (v.Archives || []).map(a => {
    const badge = a.Kind === "pre-restore" ? `<span class="badge canary">pre-restore</span>` : a.Kind === "scheduled" ? `<span class="badge">scheduled</span>` : "";
    const state = a.Complete ? "" : `<span class="badge">incomplete</span>`;
    const items = a.Items.length ? a.Items.map(esc).join(", ") : `<span class="muted">${a.Files.length} files of another layout</span>`;
    return `<tr><td class="num"><code>${esc(a.Run)}</code></td><td>${items}</td><td class="num hide-s">${size(a.Size)}</td>
      <td>${badge}${state}</td><td class="act"><button class="stop" data-restore="${esc(a.Run)}"${v.Busy ? " disabled" : ""}>Restore</button></td></tr>`;
  }).join("") || `<tr><td colspan="5" class="muted">No archives yet${v.IndexAt ? "" : " (or not listed yet: Refresh)"}.</td></tr>`;
  if (rows !== lastRows) { $("archives").innerHTML = rows; lastRows = rows; }

  $("runs").innerHTML = (v.Runs || []).map(r => `<tr><td>${esc(NAMES[r.Kind] || r.Kind)} ${r.TS ? `<code>${esc(r.TS)}</code>` : ""}</td>
    <td class="${esc(r.Status)}">${esc(r.Status)}</td><td class="num hide-s">${esc(when(r.Started))}</td>
    <td class="num hide-s">${esc(took(r.Started, r.Finished))}</td><td class="muted">${esc(r.By)}</td></tr>`).join("")
    || `<tr><td colspan="5" class="muted">None yet.</td></tr>`;

  const r = v.Run;
  $("runcard").hidden = !r;
  if (r) {
    $("runtitle").textContent = (NAMES[r.Kind] || r.Kind) + (r.TS ? " " + r.TS : "");
    $("status").textContent = r.Status; $("status").className = "status " + r.Status;
    $("what").textContent = r.By ? "by " + r.By : "";
    $("when").textContent = when(r.Started) + " · " + took(r.Started, r.Finished);
    $("message").textContent = r.Message || "";
    const log = (r.Log || []).join("\n");
    if (log !== lastLog) {
      const el = $("log"), atEnd = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
      el.textContent = log; lastLog = log;
      if (atEnd) el.scrollTop = el.scrollHeight;
    }
  }
}

async function poll() {
  try {
    const r = await fetch("backup/state" + q, {cache: "no-store"});
    if (r.status === 401) { location.href = "login"; return; }
    if (r.ok) render(last = await r.json());
    else if (!formOpen) openForm();
  } catch (e) { /* keep the last view */ }
  setTimeout(poll, 2000);
}

async function post(url, body) {
  clickErr = "";
  try {
    const r = await fetch(url, {method: "POST", body: body ? JSON.stringify(body) : undefined});
    if (!r.ok) { clickErr = "Failed: " + (await r.text()); return false; }
    return true;
  } catch (e) { clickErr = "Failed: " + e; return false; }
  finally { if (last) render(last); }
}

$("now").addEventListener("click", () => {
  if (armed !== "now") {
    armed = "now"; clearTimeout(armTimer);
    armTimer = setTimeout(() => { armed = ""; if (last) render(last); }, 4000);
    if (last) render(last);
    return;
  }
  armed = ""; clearTimeout(armTimer);
  post("backup/run" + q);
});
$("refresh").addEventListener("click", () => post("backup/refresh" + q));

// --- restore ---

function openRestore(run) {
  const a = (last.Archives || []).find(x => x.Run === run);
  restoring = a;
  $("rtitle").textContent = "Restore " + run;
  $("rns").textContent = ns;
  $("rconfirm").value = ""; $("rpw").value = ""; $("rnosafety").checked = false;
  $("rimport").checked = a.Items.length === 0;
  renderPicks();
  $("restore").hidden = false;
  $("restore").scrollIntoView({behavior: "smooth"});
}

function renderPicks() {
  const a = restoring, set = last.Set;
  if (!$("rimport").checked) {
    $("rpicks").innerHTML = a.Items.map(n => `<label class="check"><input type="checkbox" data-pick="${esc(n)}" checked>${esc(n)}</label>`).join("")
      || `<p class="muted">This run has none of the set's items under their names: an import.</p>`;
    return;
  }
  const files = a.Files.filter(f => !f.Name.endsWith("_manifest.json"));
  $("rpicks").innerHTML = set.items.map(it => `<div class="grid">
    <div><label>${esc(it.name)} (${esc(it.kind)}) from</label><select data-imp="${esc(it.name)}"><option value="">- not restored -</option>
      ${files.map(f => `<option>${esc(f.Name)}</option>`).join("")}</select></div>
    <div>${it.kind === "volume" ? `<label>directory in the tar (empty: all of it)</label><input data-path="${esc(it.name)}" placeholder="e.g. app/data/images">` : ""}</div>
  </div>`).join("");
}
$("rimport").addEventListener("change", renderPicks);
$("rcancel").addEventListener("click", () => { $("restore").hidden = true; restoring = null; });
$("archives").addEventListener("click", e => {
  const b = e.target.closest("[data-restore]");
  if (b && !b.disabled) openRestore(b.dataset.restore);
});
$("rgo").addEventListener("click", async () => {
  const body = {Run: restoring.Run, Confirm: $("rconfirm").value.trim(), Password: $("rpw").value, NoSafety: $("rnosafety").checked};
  if ($("rimport").checked) {
    body.Import = [...document.querySelectorAll("[data-imp]")].filter(s => s.value).map(s => {
      const p = document.querySelector(`[data-path="${CSS.escape(s.dataset.imp)}"]`);
      return {item: s.dataset.imp, file: s.value, path: p ? p.value.trim() : ""};
    });
  } else {
    body.Items = [...document.querySelectorAll("[data-pick]:checked")].map(x => x.dataset.pick);
  }
  $("rgo").disabled = true;
  if (await post("backup/restore" + q, body)) { $("restore").hidden = true; restoring = null; }
  $("rpw").value = "";
  $("rgo").disabled = false;
});

// --- the set ---

let candidates = [];
const FIELDS = {
  mariadb: [["service", "Service"], ["statefulSet", "StatefulSet"], ["database", "Database"], ["user", "User"], ["secret", "Secret"],
            ["passwordKey", "its password key"], ["rootUser", "Restore user (root)"], ["rootKey", "its password key"]],
  volume: [["subPath", "Sub-path (empty: all of it)"], ["exclude", "Excludes, space separated"]],
};
FIELDS.postgres = FIELDS.mariadb.map(([k, l]) => [k, k === "rootUser" ? "Restore user (postgres)" : l]);

function itemRow(it, i) {
  const fields = FIELDS[it.kind].map(([k, label]) => {
    const val = k === "exclude" ? (it.exclude || []).join(" ") : (it[k] || "");
    return `<div><label>${esc(label)}</label><input data-i="${i}" data-f="${k}" value="${esc(val)}"></div>`;
  }).join("");
  const what = it.kind === "volume" ? `volume <code>${esc(it.pvc)}</code>` : `${esc(it.kind)} <code>${esc(it.statefulSet || it.service)}</code>`;
  return `<div class="card item"><label class="check"><input type="checkbox" data-i="${i}" data-f="on"${it.on ? " checked" : ""}>${what}</label>
    <div class="grid"><div><label>Name in the files</label><input data-i="${i}" data-f="name" value="${esc(it.name)}"></div><div></div>${fields}</div></div>`;
}

function workloads(sel, list, pick) {
  sel.innerHTML = `<option value="">- none -</option>` + list.map(w => `<option${w === pick ? " selected" : ""}>${esc(w)}</option>`).join("");
}

async function openForm() {
  formOpen = true;
  $("form").hidden = false; $("formerr").hidden = true;
  try {
    const sg = await getJSON("admin/backup" + q);
    const set = sg.Set || {};
    $("mirror").hidden = !(sg.Set && !(last && last.Set && last.Set.target));
    $("formtitle").textContent = set.target ? "The backup set of " + ns : "Back up " + ns;
    $("remove").hidden = !(last && last.Set && last.Set.target);
    $("target").innerHTML = `<option value="">-</option>` + sg.PVCs.map(p => `<option${p === set.target ? " selected" : ""}>${esc(p)}</option>`).join("");
    $("tools").value = set.tools || ""; $("schedule").value = set.schedule || ""; $("tz").value = set.timeZone || sg.TimeZone;
    $("cold").checked = !!set.cold; $("daily").value = set.keepDaily || ""; $("monthly").value = set.keepMonthly || "";
    workloads($("beforew"), sg.Workloads || [], set.before && set.before.workload); $("beforec").value = set.before ? set.before.command : "";
    workloads($("afterw"), sg.Workloads || [], set.after && set.after.workload); $("afterc").value = set.after ? set.after.command : "";
    const key = it => it.kind + ":" + (it.kind === "volume" ? it.pvc + "/" + (it.subPath || "") : it.statefulSet || it.service);
    candidates = (set.items || []).map(it => ({...it, on: true}));
    for (const it of sg.Items || []) if (!candidates.some(c => key(c) === key(it))) candidates.push({...it, on: false});
    $("items").innerHTML = candidates.length ? candidates.map(itemRow).join("") : "No database StatefulSet and no PVC found.";
  } catch (e) { $("formerr").textContent = String(e.message || e); $("formerr").hidden = false; }
  $("form").scrollIntoView({behavior: "smooth"});
}

function readForm() {
  for (const el of $("items").querySelectorAll("[data-i]")) {
    const it = candidates[+el.dataset.i], f = el.dataset.f;
    if (f === "on") it.on = el.checked;
    else if (f === "exclude") it.exclude = el.value.split(/\s+/).filter(Boolean);
    else it[f] = el.value.trim();
  }
  const num = id => parseInt($(id).value, 10) || 0;
  const hook = (w, c) => $(w).value ? {workload: $(w).value, command: $(c).value.trim()} : null;
  return {
    namespace: ns, target: $("target").value, tools: $("tools").value.trim(),
    items: candidates.filter(it => it.on).map(({on, ...it}) => it),
    schedule: $("schedule").value.trim(), timeZone: $("tz").value.trim(), cold: $("cold").checked,
    before: hook("beforew", "beforec"), after: hook("afterw", "afterc"),
    keepDaily: num("daily"), keepMonthly: num("monthly"),
  };
}

$("edit").addEventListener("click", openForm);
$("cancel").addEventListener("click", () => {
  if (last && last.Set && last.Set.target) { $("form").hidden = true; formOpen = false; } else location.href = "./";
});
$("save").addEventListener("click", async () => {
  $("formerr").hidden = true; $("save").disabled = true;
  try {
    const r = await fetch("admin/backup", {method: "POST", body: JSON.stringify(readForm())});
    if (!r.ok) throw new Error(await r.text());
    $("form").hidden = true; formOpen = false; lastRows = "";
  } catch (e) { $("formerr").textContent = String(e.message || e); $("formerr").hidden = false; }
  $("save").disabled = false;
});
let removeArmed = false;
$("remove").addEventListener("click", async () => {
  if (!removeArmed) {
    removeArmed = true; $("remove").textContent = "Click again to stop";
    setTimeout(() => { removeArmed = false; $("remove").textContent = "Stop backing up"; }, 4000);
    return;
  }
  const r = await fetch("admin/backup/remove" + q, {method: "POST"});
  if (!r.ok) { $("formerr").textContent = await r.text(); $("formerr").hidden = false; return; }
  location.href = "./";
});

if (params.has("edit")) openForm();
poll();
