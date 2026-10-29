// Applies the theme this browser chose before the page paints. Without a
// choice the stylesheet follows the system.
"use strict";
try {
  const t = localStorage.getItem("oc-theme");
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
} catch (_) { /* storage blocked: follow the system */ }
