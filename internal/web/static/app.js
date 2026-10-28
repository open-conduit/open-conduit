// Open Conduit web view: the shell's progressive behaviour. Every page
// works without it; this adds the theme toggle, toasts that dismiss
// themselves, dialogs and menus. Pages add their own scripts.
"use strict";

// Theme: remembered per browser (theme.js applies it before paint).
const toggle = document.querySelector("[data-theme-toggle]");
if (toggle) {
  const dark = () => document.documentElement.dataset.theme === "dark" ||
    (!document.documentElement.dataset.theme && matchMedia("(prefers-color-scheme: dark)").matches);
  const label = () => {
    toggle.textContent = dark() ? "☀" : "☾";
    toggle.setAttribute("aria-label", "Switch to " + (dark() ? "light" : "dark") + " theme");
  };
  toggle.hidden = false;
  label();
  toggle.addEventListener("click", () => {
    const next = dark() ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem("oc-theme", next); } catch (_) { /* per-browser convenience only */ }
    label();
  });
}

// Toasts leave after a while; the ✕ dismisses one at once.
document.querySelectorAll("[data-toast]").forEach((t) => {
  const x = t.querySelector("[data-dismiss]");
  if (x) x.addEventListener("click", (e) => { e.preventDefault(); t.remove(); });
  setTimeout(() => t.remove(), t.classList.contains("tone-warn") ? 12000 : 6000);
});

// Dialogs: [data-dialog="id"] opens <dialog id>, [data-close] closes the
// dialog it is in, and a click on the backdrop closes it. A link that
// opens a dialog keeps its href for when there is no script.
window.ocDialog = (id) => {
  const d = document.getElementById(id);
  if (d && !d.open) d.showModal();
  return d;
};
document.addEventListener("click", (e) => {
  const open = e.target.closest("[data-dialog]");
  if (open) {
    e.preventDefault();
    window.ocDialog(open.dataset.dialog);
    return;
  }
  const close = e.target.closest("[data-close]");
  if (close) {
    e.preventDefault();
    close.closest("dialog").close();
    return;
  }
  if (e.target instanceof HTMLDialogElement) e.target.close();
  // A click outside an open menu closes it.
  document.querySelectorAll("details.menu-wrap[open]").forEach((m) => {
    if (!m.contains(e.target)) m.open = false;
  });
});

// Menus are <details>: Escape closes the open one and returns focus, the
// arrow keys move between its items.
document.addEventListener("keydown", (e) => {
  const m = e.target.closest && e.target.closest("details.menu-wrap[open]");
  if (!m) return;
  const items = [...m.querySelectorAll("[role=menuitem]:not([aria-disabled=true])")];
  const i = items.indexOf(document.activeElement);
  if (e.key === "Escape") {
    e.preventDefault();
    e.stopPropagation();
    m.open = false;
    m.querySelector("summary").focus();
  } else if (e.key === "ArrowDown") {
    e.preventDefault();
    items[(i + 1) % items.length]?.focus();
  } else if (e.key === "ArrowUp") {
    e.preventDefault();
    items[(i - 1 + items.length) % items.length]?.focus();
  }
});
document.querySelectorAll("details.menu-wrap").forEach((m) => {
  m.addEventListener("toggle", () => {
    if (m.open) m.querySelector("[role=menuitem]:not([aria-disabled=true])")?.focus();
  });
});

// The keyboard help.
const help = document.querySelector("[data-help]");
if (help) {
  help.hidden = false;
  help.addEventListener("click", () => window.ocDialog("keys"));
}

// Confirmations: a link marked [data-action-dialog] opens its confirmation
// as a dialog, read fresh from the server, instead of the confirmation
// page it links to. If that read fails the link is followed.
document.addEventListener("click", async (e) => {
  const a = e.target.closest && e.target.closest("a[data-action-dialog]");
  if (!a || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey) return;
  e.preventDefault();
  const menu = a.closest("details.menu-wrap");
  if (menu) menu.open = false;
  try {
    const res = await fetch(a.href + "&part=1", { credentials: "same-origin" });
    if (!res.ok) throw new Error(String(res.status));
    const holder = document.createElement("div");
    holder.innerHTML = await res.text();
    const d = holder.querySelector("dialog");
    if (!d) throw new Error("no dialog");
    document.body.append(d);
    d.addEventListener("close", () => { d.remove(); a.focus(); });
    d.querySelectorAll("[data-copy]").forEach((b) => { b.hidden = false; });
    d.showModal();
    d.querySelector("[autofocus]")?.focus();
  } catch (_) {
    location.href = a.href;
  }
});

// Copy buttons: [data-copy] copies its value, shown only with script.
document.querySelectorAll("[data-copy]").forEach((b) => { b.hidden = false; });
document.addEventListener("click", (e) => {
  const b = e.target.closest && e.target.closest("[data-copy]");
  if (!b) return;
  navigator.clipboard?.writeText(b.dataset.copy);
  const was = b.textContent;
  b.textContent = "Copied";
  setTimeout(() => { b.textContent = was; }, 1400);
});
