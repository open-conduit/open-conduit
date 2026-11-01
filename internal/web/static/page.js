// An extension's page (page.html): the conversation stays at its newest
// message while the operator is reading there, whoever wrote it, and
// leaves them where they are once they scroll up. The message box grows
// with what is typed; Enter sends and Shift+Enter starts a new line.
// Without this the forms still post, one line at a time.
"use strict";

(() => {
  const NEAR = 80;
  const page = document.documentElement;
  const atEnd = () => window.innerHeight + window.scrollY >= page.scrollHeight - NEAR;
  const toEnd = () => window.scrollTo(0, page.scrollHeight);
  let pinned = true;
  window.addEventListener("scroll", () => { pinned = atEnd(); }, { passive: true });
  document.addEventListener("live:redrawn", () => { if (pinned) toEnd(); });
  toEnd();

  const grow = (t) => {
    t.style.height = "auto";
    t.style.height = t.scrollHeight + "px";
  };
  const box = (el) => el && el.matches && el.matches("textarea[data-grow]");
  document.addEventListener("input", (e) => { if (box(e.target)) grow(e.target); });
  document.addEventListener("keydown", (e) => {
    if (!box(e.target) || e.key !== "Enter" || e.shiftKey || e.isComposing) return;
    e.preventDefault();
    if (e.target.value.trim() !== "") e.target.form.requestSubmit();
  });
  document.querySelectorAll("textarea[data-grow]").forEach(grow);
  // The message box answers the open question, or messages when there is
  // none: only what it sends is redrawn, so the box takes its placeholder
  // from the redrawn button, and hides when there is nothing to send.
  document.addEventListener("live:redrawn", () => {
    const t = document.querySelector("textarea[data-composer]");
    if (!t) return;
    const b = t.form.querySelector("[data-live=page-send] button");
    t.form.hidden = !b;
    if (!b) return;
    t.placeholder = b.dataset.placeholder;
    t.setAttribute("aria-label", b.dataset.label);
  });
  // Sending from the bottom keeps the reply in view.
  document.addEventListener("submit", () => { pinned = true; });
})();
