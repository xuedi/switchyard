const failed = new URLSearchParams(location.search).get("failed");
document.getElementById("failed").hidden = failed !== "1";
document.getElementById("locked").hidden = failed !== "locked";
