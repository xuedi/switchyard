"use strict";
// Inside a dashboard: the links leave the top for the version's line at the bottom, the page's own
// (a box's site and mail) on the left, the navigation on the right.
(() => {
  const root = document.documentElement;
  if (!root.classList.contains("embedded")) return;
  const foot = document.createElement("div");
  foot.className = "foot";
  let ver = document.getElementById("ver");
  if (!ver) { ver = document.createElement("p"); ver.id = "ver"; }
  ver.classList.add("muted");
  foot.appendChild(ver);
  const links = document.querySelector("h1 .links");
  if (links) foot.appendChild(links);
  const nav = document.createElement("nav");
  nav.className = "footnav";
  for (const t of document.querySelectorAll(".tabbar a")) {
    const a = document.createElement("a");
    a.href = t.getAttribute("href");
    a.textContent = t.textContent;
    if (t.classList.contains("on")) a.className = "on";
    nav.appendChild(a);
  }
  if (root.classList.contains("login")) {
    const f = document.createElement("form");
    f.method = "post"; f.action = "logout";
    f.innerHTML = `<button class="plain">Log out</button>`;
    nav.appendChild(f);
  }
  foot.appendChild(nav);
  document.querySelector("main").appendChild(foot);
})();
