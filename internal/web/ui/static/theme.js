/* Applies the stored theme before first paint. Blocking on purpose; no other code. */
(function () {
  try {
    var t = localStorage.getItem("magnetico-theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) { /* storage unavailable: follow the system theme */ }
})();
