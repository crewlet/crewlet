// Hash router. A view's URL survives a refresh, and — since every filter
// and lens is a query parameter — a filtered screen can be handed to
// somebody else as a link.

// Routes that moved when the dashboard was reorganised around questions
// rather than data types. Old links stay good: they are in bookmarks, in
// chat threads, and in the agent page's own "Events" button. A redirect
// costs one hashchange; a dead link costs the reader the thing they were
// looking for.
//
// Query strings survive the move, which is the point for `events`: the
// filtered form (`#/events?actor=CEO`) is the one that gets shared.
const MOVED = {
  dashboard: () => ({ path: "/" }),
  events: (q) => ({ path: "/activity", query: q }),
  tokens: (q) => ({ path: "/spend", query: q }),
  agents: () => ({ path: "/org", query: new URLSearchParams({ lens: "seats" }) }),
  people: () => ({ path: "/org", query: new URLSearchParams({ lens: "directory" }) }),
  company: () => ({ path: "/org", query: new URLSearchParams({ lens: "charter" }) }),
  audit: () => ({ path: "/config", query: new URLSearchParams({ lens: "history" }) }),
};

const KNOWN = [
  "mission",
  "work",
  "activity",
  "org",
  "schedules",
  "spend",
  "integrations",
  "fleet",
  "config",
  "tools",
];

/**
 * The route the location bar currently names.
 *
 * Returns `{ name, params }`, or `{ redirect }` for a route that moved —
 * the shell navigates rather than rendering, so the address bar ends up
 * showing where the reader actually is.
 */
export function parseRoute() {
  const raw = location.hash.replace(/^#\/?/, "");
  const [path, query] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  const q = new URLSearchParams(query || "");

  if (parts.length === 0) return { name: "mission", params: {} };

  // A seat is addressed by handle — the canonical identity everywhere
  // else in the system — but resolves by runtime id too, so the links
  // minted before the move still land.
  if (parts[0] === "seats" && parts[1]) {
    const key = decodeURIComponent(parts[1]);
    if (parts[2] === "llm" && parts[3]) {
      return {
        name: "llm",
        params: {
          key,
          turn: decodeURIComponent(parts[3] || ""),
          phase: decodeURIComponent(parts[4] || ""),
          iter: decodeURIComponent(parts[5] || "0"),
        },
      };
    }
    return { name: "seat", params: { key } };
  }
  if (parts[0] === "agents" && parts[1]) {
    const rest = parts.slice(2).map(encodeURIComponent).join("/");
    return {
      redirect: `/seats/${encodeURIComponent(decodeURIComponent(parts[1]))}${rest ? "/" + rest : ""}`,
    };
  }

  if (parts[0] === "events" && parts[1]) {
    return { name: "eventDetail", params: { id: decodeURIComponent(parts[1]) } };
  }
  // Reached from a row, never from the nav: a trace view with no trace
  // to show is a placeholder screen, and those are forbidden.
  if (parts[0] === "traces" && parts[1]) {
    return { name: "trace", params: { trace_id: decodeURIComponent(parts[1]) } };
  }

  const moved = MOVED[parts[0]];
  if (moved) {
    const target = moved(q);
    const search = target.query ? target.query.toString() : "";
    return { redirect: target.path + (search ? `?${search}` : "") };
  }

  const name = KNOWN.includes(parts[0]) ? parts[0] : "mission";
  return { name, params: Object.fromEntries(q) };
}

export function navigate(hash) {
  if (location.hash === "#" + hash) {
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  } else {
    location.hash = hash;
  }
}

/**
 * Replace the current entry rather than pushing a new one.
 *
 * Filter changes call this: a reader who ticked four category pills and
 * then pressed Back expects to leave the screen, not to walk back
 * through four intermediate filter states.
 */
export function replaceParams(name, params) {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === "" || value === null || value === undefined) continue;
    search.set(key, String(value));
  }
  const query = search.toString();
  const next = `#/${name}${query ? `?${query}` : ""}`;
  if (location.hash === next) return;
  history.replaceState(null, "", next);
}

export function onRouteChange(fn) {
  window.addEventListener("hashchange", fn);
  return () => window.removeEventListener("hashchange", fn);
}
