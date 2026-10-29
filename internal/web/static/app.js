// Open Conduit web view: the shell's progressive behaviour. Every page
// works without it; this adds the theme toggle, toasts that dismiss
// themselves, dialogs and menus. Pages add their own scripts.
"use strict";

(() => {
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

  // ---- Keyboard and the jump palette ----
  // Pages mark what the keys act on: [data-rows] holds [data-row] rows, each
  // with an [data-open] link and an optional checkbox; [data-turn="prev"] and
  // [data-turn="next"] link to the neighbouring turns; [data-error] marks
  // failures in a transcript; [data-search] is the page's search field;
  // [data-back] is where Escape returns to.

  const typing = (el) => el && el.closest && el.closest("input, textarea, select, [contenteditable]");

  const palette = document.getElementById("palette");
  const jump = document.querySelector("[data-palette]");
  let paletteItems = null;
  let paletteAt = 0;

  async function openPalette() {
    if (!palette) return;
    window.ocDialog("palette");
    const input = palette.querySelector(".palette-input");
    input.value = "";
    input.focus();
    if (!paletteItems) {
      try {
        const res = await fetch("/palette.json", { credentials: "same-origin" });
        paletteItems = res.ok ? await res.json() : [];
      } catch (_) {
        paletteItems = [];
      }
    }
    drawPalette();
  }

  function paletteMatches() {
    const q = palette.querySelector(".palette-input").value.trim().toLowerCase();
    const all = paletteItems || [];
    return (q ? all.filter((x) => (x.label + " " + x.hint + " " + (x.find || "")).toLowerCase().includes(q)) : all).slice(0, 12);
  }

  function drawPalette() {
    const list = palette.querySelector(".palette-list");
    const input = palette.querySelector(".palette-input");
    const items = paletteMatches();
    paletteAt = Math.min(paletteAt, Math.max(items.length - 1, 0));
    list.replaceChildren();
    items.forEach((x, n) => {
      const li = document.createElement("li");
      li.id = "pal-" + n;
      li.setAttribute("role", "option");
      li.setAttribute("aria-selected", n === paletteAt ? "true" : "false");
      if (n === paletteAt) li.className = "is-on";
      if (x.mark) {
        li.insertAdjacentHTML("beforeend", x.mark);
      } else {
        const dot = document.createElement("span");
        dot.className = "pal-dot";
        li.append(dot);
      }
      const label = document.createElement("span");
      label.className = "pal-label";
      label.textContent = x.label;
      const hint = document.createElement("span");
      hint.className = "pal-hint";
      hint.textContent = x.hint;
      li.append(label, hint);
      li.addEventListener("mouseenter", () => { paletteAt = n; drawPalette(); });
      li.addEventListener("click", () => { location.href = x.href; });
      list.append(li);
    });
    if (items.length === 0) {
      const li = document.createElement("li");
      li.className = "pal-empty";
      li.textContent = "Nothing matches “" + input.value + "”";
      list.append(li);
    }
    input.setAttribute("aria-activedescendant", items.length ? "pal-" + paletteAt : "");
  }

  if (palette && jump) {
    jump.hidden = false;
    jump.addEventListener("click", openPalette);
    const input = palette.querySelector(".palette-input");
    input.addEventListener("input", () => { paletteAt = 0; drawPalette(); });
    input.addEventListener("keydown", (e) => {
      const items = paletteMatches();
      if (e.key === "ArrowDown") { e.preventDefault(); paletteAt = Math.min(paletteAt + 1, items.length - 1); drawPalette(); }
      if (e.key === "ArrowUp") { e.preventDefault(); paletteAt = Math.max(paletteAt - 1, 0); drawPalette(); }
      if (e.key === "Enter" && items[paletteAt]) { e.preventDefault(); location.href = items[paletteAt].href; }
    });
  }

  // The focused row of a [data-rows] list.
  let rowAt = -1;
  function rows() { return [...document.querySelectorAll("[data-rows] [data-row]")]; }
  function focusRow(i) {
    const rs = rows();
    if (!rs.length) return;
    rowAt = Math.max(0, Math.min(i, rs.length - 1));
    rs.forEach((r, n) => r.classList.toggle("is-focus", n === rowAt));
    rs[rowAt].scrollIntoView({ block: "nearest" });
    (rs[rowAt].querySelector("[data-open]") || rs[rowAt]).focus({ preventScroll: true });
  }

  let chord = false;
  let chordTimer;
  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      openPalette();
      return;
    }
    if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey) return;
    if (document.querySelector("dialog[open]") || document.querySelector("details.menu-wrap[open]")) return;
    if (chord) {
      chord = false;
      const dest = { r: "/", a: "/attention", c: "/config", s: "/setup" }[e.key];
      if (dest) { e.preventDefault(); location.href = dest; }
      return;
    }
    const click = (sel) => { const el = document.querySelector(sel); if (el) { e.preventDefault(); el.click(); } };
    switch (e.key) {
      case "g":
        chord = true;
        clearTimeout(chordTimer);
        chordTimer = setTimeout(() => { chord = false; }, 900);
        break;
      case "?":
        e.preventDefault();
        window.ocDialog("keys");
        break;
      case "/": {
        const f = document.querySelector("[data-search]");
        if (f) { e.preventDefault(); f.focus(); f.select(); }
        break;
      }
      case "j":
        if (rows().length) { e.preventDefault(); focusRow(rowAt + 1); } else click('[data-turn="next"]');
        break;
      case "k":
        if (rows().length) { e.preventDefault(); focusRow(rowAt - 1); } else click('[data-turn="prev"]');
        break;
      case "]":
        click('[data-turn="next"]');
        break;
      case "[":
        click('[data-turn="prev"]');
        break;
      case "Enter": {
        const r = rows()[rowAt];
        const a = r && r.querySelector("[data-open]");
        if (a && !e.target.closest("a, button")) { e.preventDefault(); a.click(); }
        break;
      }
      case "x": {
        const r = rows()[rowAt];
        const box = r && r.querySelector("input[type=checkbox]");
        if (box) { e.preventDefault(); box.click(); }
        break;
      }
      case "e": {
        const err = document.querySelector("[data-error]");
        if (err) {
          e.preventDefault();
          const d = err.closest("details");
          if (d) d.open = true;
          err.scrollIntoView({ block: "center" });
          err.classList.add("is-flash");
          setTimeout(() => err.classList.remove("is-flash"), 1200);
        }
        break;
      }
      case "Escape": {
        const back = document.querySelector("[data-back]");
        if (back) { e.preventDefault(); location.href = back.getAttribute("href"); }
        break;
      }
    }
  });

  // ---- Live regions ----
  // A [data-live] region is redrawn from the same URL every few seconds while
  // the tab is visible, unless something in it has focus, a dialog or menu
  // is open, a checkbox in it is ticked or the operator picked a radio or
  // select option in it: a refresh never takes what the operator is doing
  // away.
  const live = [...document.querySelectorAll("[data-live]")];
  // A choice the operator changed in a region keeps it from being redrawn.
  document.addEventListener("change", (e) => {
    if (!e.target.matches || !e.target.matches("input[type=radio], select")) return;
    const r = e.target.closest("[data-live]");
    if (r) r.dataset.changed = "1";
  });
  if (live.length) {
    setInterval(async () => {
      if (document.hidden || document.querySelector("dialog[open], details.menu-wrap[open]")) return;
      if (live.some((r) => r.contains(document.activeElement) && typing(document.activeElement))) return;
      if (live.some((r) => r.querySelector("input[type=checkbox]:checked") || r.dataset.changed)) return;
      try {
        const res = await fetch(location.href, { credentials: "same-origin" });
        if (!res.ok) return;
        const doc = new DOMParser().parseFromString(await res.text(), "text/html");
        live.forEach((r) => {
          const next = doc.querySelector('[data-live="' + r.dataset.live + '"]');
          if (!next) return;
          const open = new Set([...r.querySelectorAll("details[open][data-key]")].map((d) => d.dataset.key));
          r.innerHTML = next.innerHTML;
          r.querySelectorAll("details[data-key]").forEach((d) => { if (open.has(d.dataset.key)) d.open = true; });
          if (rowAt >= 0 && r.matches("[data-rows], :has([data-rows])")) focusRow(rowAt);
        });
        const title = doc.querySelector("title");
        if (title) document.title = title.textContent;
      } catch (_) { /* the next tick tries again */ }
    }, 5000);
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
})();
