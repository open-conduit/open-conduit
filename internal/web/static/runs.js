// The Runs page: facets apply as they change, the bulk bar follows the
// selection and confirms in a dialog, and j/k/Enter/x/"/" move through
// the ledger. Without script the toolbar has an Apply button and the bulk
// bar's buttons post the selection directly.
"use strict";

(() => {
  const facets = document.querySelector("[data-facets]");
  if (facets) {
    facets.querySelector("[data-facets-submit]").hidden = true;
    facets.querySelectorAll("select").forEach((s) => s.addEventListener("change", () => facets.requestSubmit()));
  }
  const search = document.querySelector("[data-search]");

  // Bulk hold and release.
  const bulk = document.querySelector("[data-bulk]");
  const boxes = () => [...document.querySelectorAll('input[type=checkbox][form="bulk"]')];
  const picked = () => boxes().filter((b) => b.checked);
  const rowOf = (b) => b.closest("[data-row]");
  const label = (verb, n, of) => verb + (n > 0 && n < of ? " " + n + " of " + of : "");
  const sync = () => {
    if (!bulk) return;
    const sel = picked();
    bulk.hidden = sel.length === 0;
    const hold = sel.filter((b) => "holdable" in rowOf(b).dataset).length;
    const release = sel.filter((b) => "held" in rowOf(b).dataset).length;
    bulk.querySelector("[data-bulk-n]").textContent = sel.length;
    bulk.querySelector("[data-bulk-word]").textContent = " selected";
    const h = bulk.querySelector("[data-bulk-hold]");
    const r = bulk.querySelector("[data-bulk-release]");
    h.textContent = label("Hold", hold, sel.length);
    h.disabled = hold === 0;
    r.textContent = label("Release", release, sel.length);
    r.disabled = release === 0;
    bulk.querySelector("[data-bulk-clear]").hidden = false;
    document.querySelectorAll("[data-row]").forEach((row) => {
      row.classList.toggle("is-selected", row.querySelector('input[form="bulk"]').checked);
    });
  };
  if (bulk) {
    sync();
    document.addEventListener("change", (e) => { if (e.target.matches('input[form="bulk"]')) sync(); });
    bulk.querySelector("[data-bulk-clear]").addEventListener("click", () => {
      boxes().forEach((b) => { b.checked = false; });
      sync();
    });
    const dialog = bulk.querySelector("#bulk-dialog");
    const open = (kind) => {
      const sel = picked();
      const list = sel.filter((b) => (kind === "hold" ? "holdable" : "held") in rowOf(b).dataset);
      // Only the eligible ones are posted; the others stay selected.
      sel.forEach((b) => { b.disabled = !list.includes(b); });
      dialog.querySelector("#bulk-title").textContent = (kind === "hold" ? "Hold " : "Release ") + list.length + " pieces of work";
      const ul = dialog.querySelector("[data-bulk-list]");
      ul.replaceChildren(...list.map((b) => {
        const li = document.createElement("li");
        const mark = rowOf(b).querySelector(".now-label .mark");
        if (mark) li.append(mark.cloneNode(true), " ");
        const code = document.createElement("code");
        code.textContent = b.dataset.ref;
        li.append(code, " " + b.dataset.title);
        return li;
      }));
      const skipped = sel.length - list.length;
      const note = dialog.querySelector("[data-bulk-skipped]");
      note.hidden = skipped === 0;
      note.textContent = skipped + " selected " + (skipped === 1 ? "item is" : "items are") + " skipped: " +
        (kind === "hold" ? "running or finished work cannot be held." : "not held.");
      dialog.querySelector("[data-bulk-hold-note]").hidden = kind !== "hold";
      dialog.querySelector("[data-bulk-reason]").hidden = kind !== "hold";
      dialog.querySelector("[data-bulk-call]").textContent = list.length + " × POST /v1/work/…/" + kind;
      const go = dialog.querySelector("[data-bulk-go]");
      go.textContent = (kind === "hold" ? "Hold " : "Release ") + list.length;
      go.formAction = "/work/bulk/" + kind;
      dialog.addEventListener("close", () => boxes().forEach((b) => { b.disabled = false; }), { once: true });
      dialog.showModal();
      if (kind === "hold") dialog.querySelector("[data-bulk-reason] input").focus();
    };
    bulk.querySelector("[data-bulk-hold]").addEventListener("click", (e) => { e.preventDefault(); open("hold"); });
    bulk.querySelector("[data-bulk-release]").addEventListener("click", (e) => { e.preventDefault(); open("release"); });
  }

  // Keyboard: / searches, j and k move, Enter opens, x selects.
  const rows = () => [...document.querySelectorAll("[data-row]")];
  let cursor = -1;
  const focus = (n) => {
    const all = rows();
    if (!all.length) return;
    cursor = Math.max(0, Math.min(all.length - 1, n));
    all.forEach((r, i) => r.classList.toggle("is-cursor", i === cursor));
    all[cursor].querySelector(".row-link").focus();
  };
  document.addEventListener("focusin", (e) => {
    const row = e.target.closest && e.target.closest("[data-row]");
    if (!row) return;
    cursor = rows().indexOf(row);
    rows().forEach((r, i) => r.classList.toggle("is-cursor", i === cursor));
  });
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey || document.querySelector("dialog[open]")) return;
    const el = e.target;
    if (el.closest("input, textarea, select, [contenteditable], details[open]")) {
      if (e.key === "Escape" && el === search) search.blur();
      return;
    }
    if (e.key === "/" && search) {
      e.preventDefault();
      search.focus();
      search.select();
    } else if (e.key === "j" || e.key === "k") {
      e.preventDefault();
      focus(cursor + (e.key === "j" ? 1 : -1));
    } else if (e.key === "Enter" && cursor >= 0 && !el.closest("a, button")) {
      location.href = rows()[cursor].dataset.href;
    } else if (e.key === "x" && cursor >= 0) {
      const b = rows()[cursor].querySelector('input[form="bulk"]');
      b.checked = !b.checked;
      sync();
    }
  });
})();
