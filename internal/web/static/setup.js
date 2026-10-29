// The Setup page: the conversation stays at its newest message while the
// operator is reading there, whoever wrote it, and leaves them where they
// are once they scroll up. The message and answer boxes grow with what is
// typed; Enter sends and Shift+Enter starts a new line. Without this the
// forms still post, one line at a time.
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
  // Sending from the bottom keeps the reply in view.
  document.addEventListener("submit", () => { pinned = true; });
})();
