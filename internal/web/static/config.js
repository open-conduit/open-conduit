// Configuration: a trace sample is chosen by changing the select, and a
// binding linked from the flow chart opens when it is shown.
"use strict";

document.querySelectorAll("select[data-autosubmit]").forEach((s) => {
  s.form.querySelectorAll("[data-noscript]").forEach((b) => { b.hidden = true; });
  s.addEventListener("change", () => s.form.submit());
});

function openBinding() {
  const id = decodeURIComponent(location.hash.slice(1));
  const d = id && document.getElementById(id);
  if (d instanceof HTMLDetailsElement) {
    d.open = true;
    d.scrollIntoView({ block: "center" });
  }
}
window.addEventListener("hashchange", openBinding);
openBinding();
