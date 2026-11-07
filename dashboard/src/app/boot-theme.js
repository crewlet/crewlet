// The reader's stored mode and theme, on the root before the first paint: the
// boot screen and the app that replaces it open in the same colours. A classic
// script of its own, because the engine's policy admits no inline one and the
// app's entry arrives seconds later on a slow connection. `lib/prefs.ts` holds
// these choices and validates them once it loads; this only saves the flash.
try {
  var root = document.documentElement;
  var mode = localStorage.getItem("crewlet_theme");
  if (mode === "light" || mode === "dark") root.setAttribute("data-theme", mode);
  var palette = localStorage.getItem("crewlet_palette");
  if (palette) root.setAttribute("data-palette", palette);
} catch {
  // A privacy mode or a sandbox: the system's colours, until the app says otherwise.
}
