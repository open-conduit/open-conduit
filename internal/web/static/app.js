// Open Conduit web view: confirmations, list refresh and live output.
"use strict";

document.addEventListener("submit", (e) => {
  const msg = e.target.dataset.confirm;
  if (msg && !window.confirm(msg)) e.preventDefault();
});

const INTERVAL = 2000;

// Whether the page is scrolled to its end, so following output stays there.
const atEnd = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 8;

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

// The run's timeline: a turn's conversation loads when it is opened, and a
// running turn's streams in, keeping the tool calls that were open open and
// staying at the end unless scrolled up.
async function part(turn) {
  const res = await fetch(turn.dataset.part, { credentials: "same-origin" });
  if (!res.ok) throw new Error(await res.text());
  const conv = turn.querySelector(":scope > .conv");
  const open = new Set([...conv.querySelectorAll("details[open][data-key]")].map((d) => d.dataset.key));
  conv.innerHTML = await res.text();
  conv.querySelectorAll("details[data-key]").forEach((d) => { if (open.has(d.dataset.key)) d.open = true; });
  turn.dataset.loaded = "";
  return res.headers.get("X-Live") === "1";
}

function follow(turn) {
  const timer = setInterval(async () => {
    if (document.hidden || !turn.open) return;
    const end = atEnd();
    try {
      const live = await part(turn);
      if (end) window.scrollTo(0, document.body.scrollHeight);
      if (!live) clearInterval(timer);
    } catch (_) { /* the next tick tries again */ }
  }, INTERVAL);
}

document.querySelectorAll("details.turn").forEach((turn) => {
  turn.addEventListener("toggle", async () => {
    if (!turn.open || "loaded" in turn.dataset) return;
    try {
      if (await part(turn) && !("live" in turn.dataset)) {
        turn.dataset.live = "";
        follow(turn);
      }
    } catch (err) {
      turn.querySelector(":scope > .conv").textContent = "Could not load this turn: " + err.message;
    }
  });
  if ("live" in turn.dataset) follow(turn);
});

const openTurns = document.querySelector("[data-open-turns]");
if (openTurns) {
  openTurns.addEventListener("click", () => {
    document.querySelectorAll("details.turn").forEach((turn) => { turn.open = true; });
  });
}
