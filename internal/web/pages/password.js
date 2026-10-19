const q = new URLSearchParams(location.search);
document.getElementById("old").hidden = q.get("failed") !== "old";
document.getElementById("locked").hidden = q.get("failed") !== "locked";
document.getElementById("new").hidden = q.get("failed") !== "new";
document.getElementById("changed").hidden = !q.has("changed");
document.getElementById("turnedoff").hidden = !q.has("login-off");
document.getElementById("turnedon").hidden = !q.has("login-on");

fetch("login-state", {cache: "no-store"}).then(r => r.ok ? r.json() : Promise.reject()).then(d => {
  document.getElementById("everywhere").hidden = !d.Login;
  document.getElementById("off").hidden = !d.Login;
  document.getElementById("on").hidden = d.Login;
}).catch(() => {});
