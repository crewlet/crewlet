// Hash router. A view's URL survives a refresh, fixing the "reload
// dumps me back on the dashboard" annoyance.

export function parseRoute() {
  const raw = location.hash.replace(/^#\/?/, "");
  const [path, query] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  const q = new URLSearchParams(query || "");

  if (parts.length === 0) return { name: "dashboard", params: {} };

  if (parts[0] === "agents" && parts[1]) {
    if (parts[2] === "llm" && parts[3]) {
      return {
        name: "llm",
        params: {
          id: decodeURIComponent(parts[1]),
          turn: decodeURIComponent(parts[3] || ""),
          phase: decodeURIComponent(parts[4] || ""),
          iter: decodeURIComponent(parts[5] || "0"),
        },
      };
    }
    return { name: "agent", params: { id: decodeURIComponent(parts[1]) } };
  }
  if (parts[0] === "events" && parts[1]) {
    return { name: "eventDetail", params: { id: decodeURIComponent(parts[1]) } };
  }
  // Reached from a row, never from the nav: a trace view with no trace
  // to show is a placeholder screen, and those are forbidden.
  if (parts[0] === "traces" && parts[1]) {
    return { name: "trace", params: { trace_id: decodeURIComponent(parts[1]) } };
  }

  const known = [
    "dashboard",
    "company",
    "people",
    "org",
    "audit",
    "agents",
    "events",
    "tokens",
    "tools",
    "schedules",
    "fleet",
    "config",
  ];
  const name = known.includes(parts[0]) ? parts[0] : "dashboard";
  return { name, params: Object.fromEntries(q) };
}

export function navigate(hash) {
  if (location.hash === "#" + hash) {
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  } else {
    location.hash = hash;
  }
}

export function onRouteChange(fn) {
  window.addEventListener("hashchange", fn);
  return () => window.removeEventListener("hashchange", fn);
}
