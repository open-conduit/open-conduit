// The Work page: turn keys, the transcript toolbar without reloads, copy
// buttons, events that open in place, and a running turn that streams in.
// The output page follows a live log. Every part works without this as
// links and forms.
"use strict";

const INTERVAL = 2000;
const typing = (e) => e.target.closest && e.target.closest("input, textarea, select, [contenteditable]");

// Copy buttons appear only where the clipboard can be written.
document.querySelectorAll("[data-copy]").forEach((b) => {
  if (!navigator.clipboard) return;
  b.hidden = false;
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(b.dataset.copy);
      const was = b.textContent;
      b.textContent = "Copied";
      setTimeout(() => { b.textContent = was; }, 1400);
    } catch (_) { /* nothing copied */ }
  });
});

// Events open in place.
document.querySelectorAll("[data-event-row]").forEach((row) => {
  row.addEventListener("click", (e) => {
    const detail = row.nextElementSibling;
    // A daemon that explains routing one event at a time: the link reads it.
    if (!detail || ("reload" in row.dataset && detail.hidden)) return;
    e.preventDefault();
    detail.hidden = !detail.hidden;
    row.setAttribute("aria-expanded", String(!detail.hidden));
  });
});

// The transcript toolbar: filters, expand, find and jump to error act on
// the page, and the URL keeps the view so a reload shows the same.
const tools = document.querySelector("[data-tx-tools]");
const conv = document.querySelector("[data-conv]");
function applyView() {
  if (!tools || !conv) return;
  const off = new Set([...tools.querySelectorAll("[data-filter]")].filter((c) => c.getAttribute("aria-pressed") !== "true").map((c) => c.dataset.filter));
  const q = (tools.querySelector("input[name=q]").value || "").trim().toLowerCase();
  conv.querySelectorAll("[data-f]").forEach((b) => {
    const text = q ? b.textContent.toLowerCase() : "";
    b.hidden = off.has(b.dataset.f) || (q !== "" && !text.includes(q));
  });
}
function saveView() {
  const u = new URL(location.href);
  const on = [...tools.querySelectorAll("[data-filter]")].filter((c) => c.getAttribute("aria-pressed") === "true").map((c) => c.dataset.filter);
  if (on.length === tools.querySelectorAll("[data-filter]").length) u.searchParams.delete("show");
  else u.searchParams.set("show", on.join(",") || "none");
  const q = tools.querySelector("input[name=q]").value.trim();
  if (q) u.searchParams.set("q", q); else u.searchParams.delete("q");
  history.replaceState(null, "", u);
}
if (tools && conv) {
  tools.querySelectorAll("[data-filter]").forEach((chip) => {
    chip.addEventListener("click", (e) => {
      e.preventDefault();
      const on = chip.getAttribute("aria-pressed") !== "true";
      chip.setAttribute("aria-pressed", String(on));
      chip.classList.toggle("is-on", on);
      applyView();
      saveView();
    });
  });
  const find = tools.querySelector("input[name=q]");
  find.addEventListener("input", () => { applyView(); saveView(); });
  tools.addEventListener("submit", (e) => { e.preventDefault(); applyView(); saveView(); });
  const expand = tools.querySelector("[data-expand]");
  if (expand) {
    expand.addEventListener("click", (e) => {
      e.preventDefault();
      const open = expand.textContent.trim() === "Expand all";
      conv.querySelectorAll("details").forEach((d) => { d.open = open; });
      expand.textContent = open ? "Collapse all" : "Expand all";
    });
  }
}

function jumpToError() {
  const el = document.getElementById("first-error") || document.querySelector(".transcript .is-error");
  if (!el) return;
  let d = el.tagName === "DETAILS" ? el : el.nextElementSibling;
  if (d && d.tagName === "DETAILS") d.open = true;
  el.scrollIntoView({ block: "center" });
  const focus = (d && d.querySelector("summary")) || el.querySelector("summary, button, a");
  if (focus) focus.focus();
}
const jump = document.querySelector("[data-jump-error]");
if (jump) jump.addEventListener("click", (e) => { e.preventDefault(); jumpToError(); });

// Keys: [ ] and j k move between turns, e jumps to the first error, Esc
// goes back to Runs.
const turns = [...document.querySelectorAll(".score-turn")];
document.addEventListener("keydown", (e) => {
  if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || document.querySelector("dialog[open], details.menu-wrap[open]")) return;
  const i = turns.findIndex((t) => t.classList.contains("is-on"));
  if ((e.key === "]" || e.key === "j") && i >= 0 && i < turns.length - 1) location.href = turns[i + 1].href;
  else if ((e.key === "[" || e.key === "k") && i > 0) location.href = turns[i - 1].href;
  else if (e.key === "e" && document.querySelector("[data-jump-error]")) jumpToError();
  else if (e.key === "Escape" && turns.length) location.href = "/";
});

// A running turn streams in: the transcript is read again, keeping open
// rows open, and follows new output only while "follow" is on.
if (conv && conv.dataset.live) {
  const atEnd = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;
  let follow = true;
  const wire = () => {
    const box = conv.querySelector("[data-follow]");
    if (!box) return;
    box.closest("label").hidden = false;
    box.checked = follow;
    box.addEventListener("change", () => { follow = box.checked; });
  };
  wire();
  let seen = conv.querySelectorAll("[data-f]").length;
  const timer = setInterval(async () => {
    if (document.hidden) return;
    try {
      const res = await fetch(conv.dataset.live, { credentials: "same-origin" });
      if (!res.ok) return;
      const open = new Set([...conv.querySelectorAll("details[open][data-key]")].map((d) => d.dataset.key));
      const end = atEnd();
      conv.innerHTML = await res.text();
      conv.querySelectorAll("details[data-key]").forEach((d) => { if (open.has(d.dataset.key)) d.open = true; });
      wire();
      applyView();
      const n = conv.querySelectorAll("[data-f]").length;
      if (n !== seen && follow && end) {
        const tail = conv.querySelector("[data-live-end]") || conv.lastElementChild;
        if (tail) tail.scrollIntoView({ block: "end" });
      }
      seen = n;
      if (res.headers.get("X-Live") !== "1") {
        clearInterval(timer);
        // The turn ended: the page shows its outcome.
        location.reload();
      }
    } catch (_) { /* the next tick tries again */ }
  }, INTERVAL);
}

// The output page follows a running run's log, staying at the end unless
// scrolled up.
const out = document.querySelector("#output[data-follow]");
if (out) {
  const atEnd = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 8;
  window.scrollTo(0, document.body.scrollHeight);
  const timer = setInterval(async () => {
    try {
      const res = await fetch(out.dataset.follow, { credentials: "same-origin" });
      if (!res.ok) return;
      const o = await res.json();
      const end = atEnd();
      if (o.error) return;
      out.textContent = o.text;
      if (end) window.scrollTo(0, document.body.scrollHeight);
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
