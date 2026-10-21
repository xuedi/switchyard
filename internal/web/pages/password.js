const q = new URLSearchParams(location.search);
document.getElementById("old").hidden = q.get("failed") !== "old";
document.getElementById("locked").hidden = q.get("failed") !== "locked";
document.getElementById("new").hidden = q.get("failed") !== "new";
document.getElementById("changed").hidden = !q.has("changed");
document.getElementById("turnedoff").hidden = !q.has("login-off");
document.getElementById("turnedon").hidden = !q.has("login-on");
document.getElementById("powerset").hidden = !q.has("power");
document.getElementById("embedset").hidden = !q.has("embedded-on") && !q.has("embedded-off");

fetch("login-state", {cache: "no-store"}).then(r => r.ok ? r.json() : Promise.reject()).then(d => {
  document.getElementById("everywhere").hidden = !d.Login;
  document.getElementById("off").hidden = !d.Login;
  document.getElementById("on").hidden = d.Login;
  document.getElementById("embedon").value = d.Embedded ? "0" : "1";
  document.getElementById("embedbtn").textContent = d.Embedded ? "Turn it off" : "Turn it on";
  document.getElementById("embedstate").textContent = d.Embedded ? "It is on." : "It is off: logo and links at the top.";
}).catch(() => {});

fetch("power", {cache: "no-store"}).then(r => r.ok ? r.json() : Promise.reject()).then(p => {
  document.getElementById("pcmd").value = p.Command;
  document.getElementById("puser").value = p.User;
  document.getElementById("ppw").placeholder = p.PasswordSet ? "set, empty keeps it" : "";
}).catch(() => {});
