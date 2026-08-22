// Hash router. A view's URL survives a refresh, and — since every filter
// and lens is a query parameter — a filtered screen can be handed to
// somebody else as a link.
//
// It is also the Back button, and Back is where this went wrong three
// separate ways. A URL that is merely correct is not enough: what makes
// Back work is which entries exist in the session stack, and that is a
// decision every navigation makes whether or not anybody noticed making
// it. The rules, in one place because they are one subject:
//
//   - A MOVED route replaces. It used to push, which made Back a trap:
//     `#/events` pushed `#/activity`, so Back landed on `#/events`, which
//     redirected forward again — for ever. Measured in a browser: from a
//     redirected route, Back could not escape at all.
//   - A SECTION pushes. Lenses, tabs — the things a reader thinks of as
//     screens. These used to replace, so Back from the fourth lens of a
//     room left the room entirely and landed wherever you came in.
//   - A FILTER replaces. Somebody who ticked four category pills and
//     pressed Back means "leave this screen", not "untick one pill".
//
// The line between the last two is whether the reader would call it a
// different screen, and it is the only judgement call here.

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
  const [path, search] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  const q = new URLSearchParams(search || "");

  // The query belongs to every route, not just the ones addressed
  // entirely by it. It used to be merged only on the last branch below,
  // so a seat route arrived with its path parsed and its `?tab=` thrown
  // away — which the seat view worked around by re-reading
  // `location.hash` itself. That workaround held only because nothing
  // else needed the value; the moment the shell started handing a
  // section change to a live view, `params` with no `tab` in it read as
  // "go back to the default tab" on every Back.
  const query = Object.fromEntries(q);

  if (parts.length === 0) return { name: "mission", params: { ...query } };

  // A seat is addressed by handle — the canonical identity everywhere
  // else in the system — but resolves by runtime id too, so the links
  // minted before the move still land.
  if (parts[0] === "seats" && parts[1]) {
    const key = decodeURIComponent(parts[1]);
    if (parts[2] === "llm" && parts[3]) {
      return {
        name: "llm",
        params: {
          ...query,
          key,
          turn: decodeURIComponent(parts[3] || ""),
          phase: decodeURIComponent(parts[4] || ""),
          iter: decodeURIComponent(parts[5] || "0"),
        },
      };
    }
    return { name: "seat", params: { ...query, key } };
  }
  if (parts[0] === "agents" && parts[1]) {
    const rest = parts.slice(2).map(encodeURIComponent).join("/");
    return {
      redirect: `/seats/${encodeURIComponent(decodeURIComponent(parts[1]))}${rest ? "/" + rest : ""}`,
    };
  }

  if (parts[0] === "events" && parts[1]) {
    return {
      name: "eventDetail",
      params: { ...query, id: decodeURIComponent(parts[1]) },
    };
  }
  // Reached from a row, never from the nav: a trace view with no trace
  // to show is a placeholder screen, and those are forbidden.
  if (parts[0] === "traces" && parts[1]) {
    return {
      name: "trace",
      params: { ...query, trace_id: decodeURIComponent(parts[1]) },
    };
  }

  const moved = MOVED[parts[0]];
  if (moved) {
    const target = moved(q);
    const search = target.query ? target.query.toString() : "";
    return { redirect: target.path + (search ? `?${search}` : "") };
  }

  const name = KNOWN.includes(parts[0]) ? parts[0] : "mission";
  return { name, params: query };
}

/**
 * Go somewhere.
 *
 * `replace` overwrites the current entry instead of pushing one, which is
 * what a redirect needs: the entry naming a route that no longer exists
 * must not stay in the stack, or Back returns to it and it redirects
 * forward again.
 */
export function navigate(hash, { replace = false } = {}) {
  const next = "#" + hash;
  if (location.hash === next) {
    // Same URL, so nothing fires on its own — but the caller asked to go
    // here, and a nav click on the room you are already in should still
    // re-mount it.
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    return;
  }
  if (replace) {
    // Carry the entry's scroll key across, so returning to this entry
    // still restores where the reader was.
    history.replaceState(history.state, "", next);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    return;
  }
  location.hash = hash;
}

function href(name, params) {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === "" || value === null || value === undefined) continue;
    search.set(key, String(value));
  }
  const query = search.toString();
  return `#/${name}${query ? `?${query}` : ""}`;
}

/**
 * Move to a different SECTION of the current room — a lens, a tab.
 *
 * Pushes, because the reader thinks of these as screens and expects Back
 * to walk out through them one at a time. They all replaced once, which
 * is why Back from a room's fourth lens skipped every lens and left the
 * room.
 */
export function pushParams(name, params) {
  const next = href(name, params);
  if (location.hash === next) return;
  // Assigned in its `#...` form, which is what `location.hash` reads
  // back as. Assigning the bare path works too and is how `navigate`
  // does it, but then what you write and what you read differ, and the
  // comparison above is against what you read.
  location.hash = next;
}

/**
 * Change a FILTER on the current screen.
 *
 * Replaces: somebody who ticked four category pills and then pressed
 * Back means "leave this screen", not "untick one pill". The entry's
 * state is carried over rather than nulled, so the scroll key survives.
 */
export function replaceParams(name, params) {
  const next = href(name, params);
  if (location.hash === next) return;
  history.replaceState(history.state, "", next);
}

// ---------------------------------------------------------------------
// Scroll memory
// ---------------------------------------------------------------------
// Getting the stack right still leaves Back landing at the top of a list
// the reader had scrolled halfway down, because the shell reset the
// scroll on every mount — including the ones the reader arrived at by
// going back. A position is a property of a history ENTRY, so it is
// keyed by one.
//
// Entries carry that key in `history.state`. An entry we have never seen
// has none, which is exactly the test for "this is somewhere new": new
// means top of the page, and only a key we have already filed means
// restore.

let seq = 0;
const positions = new Map();
let currentKey = null;

/**
 * Take the route change: file the outgoing scroll, adopt the incoming
 * entry, and answer where the page should be left.
 *
 * Called before anything renders. The scroll read here is still the
 * outgoing screen's — a hash that matches no element id scrolls nothing
 * on its own, and `scrollRestoration` is set to manual below so the
 * browser does not race us for it.
 */
export function takeRoute() {
  if (currentKey !== null) positions.set(currentKey, window.scrollY || 0);

  const state = history.state;
  const known = state && state.k;
  if (known) {
    currentKey = state.k;
    return positions.get(currentKey) || 0;
  }
  currentKey = `r${++seq}`;
  history.replaceState({ ...(state || {}), k: currentKey }, "");
  return 0;
}

/**
 * Put the page back where it was, once the view has drawn.
 *
 * A restored position can be past the bottom of a view that is still
 * loading its rows, and the browser clamps a scroll to the height that
 * exists — so one attempt lands short and looks like it did nothing.
 * This keeps trying while the content grows, and stops the moment the
 * reader touches the page: fighting somebody for their own scrollbar is
 * worse than losing the position.
 */
export function restoreScroll(target) {
  window.scrollTo(0, target);
  if (!target) return;

  let stop = false;
  const give = () => {
    stop = true;
  };
  for (const ev of ["wheel", "touchstart", "keydown", "mousedown"]) {
    window.addEventListener(ev, give, { passive: true, once: true });
  }
  const until = Date.now() + RESTORE_WINDOW_MS;
  const tick = () => {
    if (stop || Date.now() > until) {
      for (const ev of ["wheel", "touchstart", "keydown", "mousedown"]) {
        window.removeEventListener(ev, give);
      }
      return;
    }
    if ((window.scrollY || 0) < target) window.scrollTo(0, target);
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

// How long to keep re-applying a restored position while a view's rows
// arrive. Sized to the slowest read a room makes on mount: the seat page
// waits on `agent`, which carries an LLM history, and that lands in a
// few hundred milliseconds against a warm store. Past that the reader
// has been looking at a settled page long enough that moving it under
// them is the ruder failure.
const RESTORE_WINDOW_MS = 600;

// The parameters the router reads out of the URL's PATH rather than its
// query string. Two routes naming the same screen differ only in their
// query — a different tab of one seat is the same screen, a different
// seat is not — which is what lets the shell hand a section change to
// the view instead of rebuilding it.
const PATH_PARAMS = ["key", "id", "trace_id", "turn", "phase", "iter"];

export function sameScreen(a, b) {
  if (!a || !b || a.name !== b.name) return false;
  return PATH_PARAMS.every(
    (k) => String((a.params || {})[k] || "") === String((b.params || {})[k] || ""),
  );
}

export function onRouteChange(fn) {
  // The browser also restores scroll on its own, on a timing we cannot
  // see; with the memory above, its guess only fights ours.
  if ("scrollRestoration" in history) history.scrollRestoration = "manual";
  window.addEventListener("hashchange", fn);
  return () => window.removeEventListener("hashchange", fn);
}
