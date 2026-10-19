"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
let editing = "";     // the box being changed, "" for a new one
let armed = "";       // the box whose Remove waits for a second click
let details = null;
let paused = false;

async function getJSON(url) {
  const r = await fetch(url, {cache: "no-store"});
  if (r.status === 401) { location.href = "login"; throw new Error("log in"); }
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

function boxRow(v) {
  const run = Object.values(v.Runs || {}).sort((a, b) => (b.Started || "").localeCompare(a.Started || ""))[0];
  const dot = !v.Online ? "" : v.Busy ? "busy" : "on";
  const canary = v.Canary ? ` · canary <code>${esc(v.Canary.Tag)}</code> about ${v.Canary.Share} %` : "";
  const id = v.Box;
  return `<div class="box">
    <span class="name"><span class="dot ${dot}"></span><a href="box?b=${encodeURIComponent(id)}">${esc(id)}</a></span>
    <span class="grow">${v.Online ? (v.Live ? `<code>${esc(v.Live)}</code> <span class="muted">${esc(v.Subject)}</span>${canary}` : "") : `<span class="muted">offline</span>`}
      ${v.Error ? `<div class="failed muted">${esc(v.Error)}</div>` : ""}</span>
    <span class="muted">${run ? `last ${esc(run.Kind)} <span class="${esc(run.Status)}">${esc(run.Status)}</span>` : ""}</span>
    <span><button class="plain" data-edit="${esc(id)}">Edit</button><button class="plain" data-remove="${esc(id)}">${armed === id ? "Click again to remove" : "Remove"}</button></span>
  </div>`;
}

const ago = iso => { const m = Math.max(0, Math.round((Date.now() - new Date(iso)) / 60000));
  return m < 60 ? m + " min ago" : m < 2880 ? Math.round(m / 60) + " h ago" : Math.round(m / 1440) + " days ago"; };

function backupRow(v) {
  const run = (v.Runs || [])[0], newest = (v.Archives || [])[0];
  const dot = !v.Online ? "" : v.Busy ? "busy" : "on";
  return `<div class="box">
    <span class="name"><span class="dot ${dot}"></span><a href="backups?n=${encodeURIComponent(v.Namespace)}">${esc(v.Namespace)}</a></span>
    <span class="grow">${newest ? `newest <code>${esc(newest.Run)}</code>${v.NewestAt ? ` <span class="muted">${esc(ago(v.NewestAt))}</span>` : ""}` : `<span class="muted">no archive yet</span>`}
      <span class="muted">${v.Set && v.Set.schedule ? "· " + esc(v.Set.schedule) : "· by hand"}</span>
      ${v.Warning ? `<div class="failed">${esc(v.Warning)}</div>` : ""}
      ${v.Stopped ? `<div class="muted">The schedule is stopped with the namespace; Start on Running resumes it.</div>` : ""}
      ${v.Error ? `<div class="failed muted">${esc(v.Error)}</div>` : ""}</span>
    <span class="muted">${run ? `last ${esc(run.Kind)} <span class="${esc(run.Status)}">${esc(run.Status)}</span>` : ""}</span>
  </div>`;
}

let lastHTML = "";
async function poll() {
  if (document.hidden) { paused = true; return; }
  try {
    const d = await getJSON("boxes");
    $("sub").textContent = d.Cluster ? "on " + d.Cluster : "";
    $("ver").textContent = "switchyard " + d.Version;
    $("logout").hidden = !d.Login;
    const html = d.Boxes.length ? d.Boxes.map(boxRow).join("") : `<p class="muted">No box yet. A box is one Deployment: switchyard moves its image from tag to tag.</p>`;
    if (html !== lastHTML) { $("boxes").innerHTML = html; lastHTML = html; }
    $("backups").innerHTML = (d.Backups || []).map(backupRow).join("") || `<p class="muted">No namespace backed up yet. A backup set saves a namespace's databases and volumes as dated archives on a PVC.</p>`;
  } catch (e) { /* keep the last list */ }
  setTimeout(poll, 3000);
}
document.addEventListener("visibilitychange", () => { if (!document.hidden && paused) { paused = false; poll(); } });

function showErr(e) { $("formerr").textContent = String(e.message || e); $("formerr").hidden = false; }

async function loadNamespaces(pick) {
  const list = await getJSON("admin/namespaces");
  $("ns").innerHTML = `<option value="">-</option>` + list.map(n => `<option${n === pick ? " selected" : ""}>${esc(n)}</option>`).join("");
}

async function loadDeployments(pick) {
  $("dep").innerHTML = ""; details = null; $("image").textContent = ""; $("creds").hidden = true;
  if (!$("ns").value) return;
  const list = await getJSON("admin/deployments?ns=" + encodeURIComponent($("ns").value));
  $("dep").innerHTML = `<option value="">-</option>` + list.map(n => `<option${n === pick ? " selected" : ""}>${esc(n)}</option>`).join("");
}

async function loadDetails(box) {
  details = null; $("image").textContent = ""; $("creds").hidden = true;
  if (!$("dep").value) return;
  const c = box ? box.container || "" : $("container").value || "";
  details = await getJSON(`admin/details?ns=${encodeURIComponent($("ns").value)}&d=${encodeURIComponent($("dep").value)}&c=${encodeURIComponent(c)}`);
  $("image").innerHTML = `Image <code>${esc(details.Image)}</code>` + (details.PullSecrets && details.PullSecrets.length ? ` · pull secrets ${details.PullSecrets.map(esc).join(", ")}` : "");
  $("reg").textContent = details.Registry;
  $("creds").hidden = details.HasCreds;
  $("cbox").hidden = details.Containers.length < 2;
  $("container").innerHTML = details.Containers.map(x => `<option${x.Name === c ? " selected" : ""}>${esc(x.Name)}</option>`).join("");
  const follow = box ? box.followers || [] : (details.Followers || []);
  $("followers").innerHTML = (details.Followers || []).length
    ? details.Followers.map(f => `<label class="check"><input type="checkbox" value="${esc(f)}"${follow.includes(f) ? " checked" : ""}>${esc(f)}</label>`).join("")
    : "none run the same image";
}

function fill(b) {
  $("pendingsql").value = b.pendingSQL || ""; $("migrate").value = b.migrate || ""; $("cache").value = b.cache || "";
  $("site").value = b.site || ""; $("pages").value = (b.pages || []).join("\n"); $("links").value = b.links || "";
  $("pause").value = b.pause || ""; $("keep").value = b.keep || "";
  $("ruser").value = ""; $("rtoken").value = "";
}

async function openForm(id) {
  editing = id || ""; $("formerr").hidden = true; $("form").hidden = false;
  $("formtitle").textContent = id ? "Change " + id : "Add a box";
  try {
    if (id) {
      const b = await getJSON("admin/box?b=" + encodeURIComponent(id));
      fill(b); $("more").open = true;
      await loadNamespaces(b.namespace); await loadDeployments(b.deployment); await loadDetails(b);
    } else {
      fill({}); $("more").open = false;
      await loadNamespaces(""); await loadDeployments("");
    }
  } catch (e) { showErr(e); }
  $("form").scrollIntoView({behavior: "smooth"});
}

$("add").addEventListener("click", () => openForm(""));
$("addbackup").addEventListener("click", async () => {
  if (!$("bns").value) {
    try {
      const list = await getJSON("admin/namespaces");
      $("bns").innerHTML = `<option value="">- namespace -</option>` + list.map(n => `<option>${esc(n)}</option>`).join("");
      $("bns").hidden = false; $("addbackup").textContent = "Set it up";
    } catch (e) { $("listerr").textContent = String(e.message || e); $("listerr").hidden = false; }
    return;
  }
  location.href = "backups?edit=1&n=" + encodeURIComponent($("bns").value);
});
$("cancel").addEventListener("click", () => { $("form").hidden = true; });
$("ns").addEventListener("change", () => loadDeployments("").catch(showErr));
$("dep").addEventListener("change", () => loadDetails(null).catch(showErr));
$("container").addEventListener("change", () => loadDetails(null).catch(showErr));
$("boxes").addEventListener("click", async e => {
  const ed = e.target.closest("[data-edit]"), rm = e.target.closest("[data-remove]");
  if (ed) openForm(ed.dataset.edit);
  if (!rm) return;
  const id = rm.dataset.remove;
  if (armed !== id) { armed = id; lastHTML = ""; setTimeout(() => { armed = ""; lastHTML = ""; }, 4000); return; }
  armed = "";
  const r = await fetch("admin/remove?b=" + encodeURIComponent(id), {method: "POST"});
  $("listerr").hidden = r.ok;
  if (!r.ok) $("listerr").textContent = await r.text();
  lastHTML = "";
});
$("save").addEventListener("click", async () => {
  $("formerr").hidden = true;
  const num = id => parseInt($(id).value, 10) || 0;
  const box = {
    namespace: $("ns").value, deployment: $("dep").value,
    container: details && details.Containers.length > 1 ? $("container").value : "",
    followers: [...$("followers").querySelectorAll("input:checked")].map(x => x.value),
    pendingSQL: $("pendingsql").value.trim(), migrate: $("migrate").value.trim(), cache: $("cache").value.trim(),
    site: $("site").value.trim(), pages: $("pages").value.split("\n").map(x => x.trim()).filter(Boolean),
    links: $("links").value.trim(), pause: num("pause"), keep: num("keep"),
  };
  $("save").disabled = true;
  try {
    const r = await fetch("admin/box", {method: "POST", body: JSON.stringify({Box: box, Original: editing,
      RegistryUser: $("ruser").value.trim(), RegistryToken: $("rtoken").value.trim()})});
    if (!r.ok) throw new Error(await r.text());
    $("form").hidden = true; lastHTML = "";
  } catch (e) { showErr(e); }
  $("save").disabled = false;
});
poll();
