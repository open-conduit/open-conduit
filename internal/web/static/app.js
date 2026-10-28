// Open Conduit web view: confirmations, list refresh and live output.
"use strict";

document.addEventListener("submit", (e) => {
  const msg = e.target.dataset.confirm;
  if (msg && !window.confirm(msg)) e.preventDefault();
});

const INTERVAL = 2000;

// Refresh a [data-refresh] region from the same page, as the TUI ticks.
const region = document.querySelector("[data-refresh]");
if (region) {
  setInterval(async () => {
    if (document.hidden) return;
    try {
      const res = await fetch(location.href, { credentials: "same-origin" });
      if (!res.ok) return;
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const next = doc.querySelector("[data-refresh]");
      if (next) region.innerHTML = next.innerHTML;
    } catch (_) { /* the next tick tries again */ }
  }, INTERVAL);
}

// Follow a running run's output, staying at the end unless scrolled up.
const out = document.querySelector("#output[data-follow]");
if (out) {
  const atEnd = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 8;
  window.scrollTo(0, document.body.scrollHeight);
  const timer = setInterval(async () => {
    try {
      const res = await fetch(out.dataset.follow, { credentials: "same-origin" });
      if (!res.ok) return;
      const o = await res.json();
      const follow = atEnd();
      if (o.error) return;
      out.textContent = o.text;
      if (follow) window.scrollTo(0, document.body.scrollHeight);
      if (!o.live) {
        clearInterval(timer);
        const note = document.getElementById("following");
        if (note) {
          note.classList.add("ended");
          document.getElementById("following-text").textContent = "The run has ended";
        }
      }
    } catch (_) { /* the next tick tries again */ }
  }, INTERVAL);
}

// Keep the current turn in view on a long rail of turns.
const cur = document.querySelector(".rail [aria-current]");
if (cur) {
  const rail = cur.parentElement;
  rail.scrollLeft = Math.max(0, cur.offsetLeft + cur.offsetWidth - rail.clientWidth);
}
