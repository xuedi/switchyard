"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"]/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"})[c]);
let tested = false;
const token = {"X-Setup-Token": new URLSearchParams(location.search).get("token") || ""};

async function check() {
  const r = await fetch("setup/check", {headers: token});
  const c = await r.json();
  const lines = [];
  lines.push(c.Kubectl ? `<span class="ok">kubectl</span> ${esc(c.KubectlVersion)} <code>${esc(c.Kubectl)}</code>` : `<span class="failed">${esc(c.KubectlError)}</span>`);
  if (c.Kubectl) lines.push(c.KubeconfigError ? `<span class="failed">${esc(c.KubeconfigError)}</span>` : `<span class="ok">kubeconfig</span> ${c.Contexts.length} contexts`);
  $("checks").innerHTML = lines.join("<br>");
  $("kubeconfig").value = c.Kubeconfig || "";
  if (c.Listen) $("listen").value = c.Listen;
  $("context").innerHTML = (c.Contexts || []).map(x => `<option${x === c.Current ? " selected" : ""}>${esc(x)}</option>`).join("");
  tested = false; update();
}

function form() {
  return {Context: $("context").value, User: $("user").value,
          Password: $("password").value, Listen: $("listen").value};
}

function update() { $("go").disabled = !tested || !$("user").value || $("password").value.length < 8; }

$("context").addEventListener("change", () => { tested = false; $("testmsg").textContent = ""; update(); });
$("user").addEventListener("input", update);
$("password").addEventListener("input", update);
$("test").addEventListener("click", async () => {
  $("testmsg").textContent = "Testing...";
  const r = await fetch("setup/test", {method: "POST", headers: token, body: JSON.stringify(form())});
  const t = await r.json();
  tested = t.OK;
  $("testmsg").innerHTML = `<span class="${t.OK ? "ok" : "failed"}">${esc(t.Message)}</span>`;
  update();
});
$("go").addEventListener("click", async () => {
  $("go").disabled = true; $("err").hidden = true;
  $("go").textContent = "Setting up...";
  const r = await fetch("setup", {method: "POST", headers: token, body: JSON.stringify(form())});
  $("go").textContent = "Set up";
  if (!r.ok) { $("err").textContent = await r.text(); $("err").hidden = false; update(); return; }
  const d = await r.json();
  $("unit").textContent = d.Unit;
  $("cmds").textContent = d.Commands.join("\n");
  $("restart").textContent = d.Systemd
    ? "This already runs as the service: nothing else to do."
    : "This process serves the boxes until you stop it (Ctrl-C). Stop it before you enable the service, which listens on " + d.Listen + ".";
  $("done").hidden = false;
  $("go").hidden = true;
});
check();
