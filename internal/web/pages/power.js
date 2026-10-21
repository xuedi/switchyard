"use strict";
// Start server on the offline card, when a power-on command is set.
(() => {
  const $ = id => document.getElementById(id);
  const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
  let last = null, armed = false, armTimer, busy = false, lastHTML = "";

  function render() {
    const p = last;
    let html = "";
    if (p && p.Set && !p.Online) {
      const wait = busy || p.Running;
      html = `<div class="buttons center gap-t"><button id="poweron"${wait ? " disabled" : ""}${armed ? ` class="confirm"` : ""}>${wait ? "Starting..." : armed ? "Click again to start" : "Start server"}</button></div>`
        + (p.Since ? `<p class="muted">Started at ${esc(p.Since)}. Booting takes a few minutes.</p>` : "")
        + (p.Error ? `<div class="err gap-t">${esc(p.Error)}</div>` : "")
        + (p.Out ? `<pre class="gap-t">${esc(p.Out)}</pre>` : "");
    }
    if (html !== lastHTML) { $("power").innerHTML = html; lastHTML = html; }
  }

  async function load() {
    try {
      const r = await fetch("power", {cache: "no-store"});
      if (r.ok) { last = await r.json(); render(); }
    } catch (e) { /* keep the last */ }
  }

  async function poll() {
    if (!$("offline").hidden && !document.hidden && !busy) await load();
    setTimeout(poll, 2000);
  }

  document.addEventListener("click", async e => {
    if (e.target.id !== "poweron") return;
    if (!armed) {
      armed = true; clearTimeout(armTimer);
      armTimer = setTimeout(() => { armed = false; render(); }, 4000);
      render(); return;
    }
    armed = false; clearTimeout(armTimer); busy = true; render();
    try { await fetch("power/on", {method: "POST"}); } catch (e) { /* the state says */ }
    busy = false; await load();
  });
  poll();
})();
