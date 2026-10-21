"use strict";
// Inside a dashboard: no heading; the version's line at the bottom takes the page's own links and
// buttons (a box's site and mail, Expand all) on the left, the navigation on the right.
(() => {
  const root = document.documentElement;
  if (!root.classList.contains("embedded")) return;
  const foot = document.createElement("div");
  foot.className = "foot";
  let ver = document.getElementById("ver");
  if (!ver) { ver = document.createElement("p"); ver.id = "ver"; }
  ver.classList.add("muted");
  foot.appendChild(ver);
  for (const el of document.querySelectorAll("h1.top .links, h1.top .right:not(.navtop)")) foot.appendChild(el);
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
