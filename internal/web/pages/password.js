const q = new URLSearchParams(location.search);
document.getElementById("old").hidden = q.get("failed") !== "old";
document.getElementById("locked").hidden = q.get("failed") !== "locked";
document.getElementById("new").hidden = q.get("failed") !== "new";
document.getElementById("changed").hidden = !q.has("changed");
