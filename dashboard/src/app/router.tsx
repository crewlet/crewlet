/**
 * The hash router.
 *
 * Hash routing, deliberately: the shell is served from a Go binary at `/` and
 * `/dashboard`, behind whatever path a reverse proxy chose, and a path router
 * would need a rewrite rule on every one of those deployments. It also keeps
 * every link anyone has already bookmarked — `#/seats/pm?tab=llm` — working.
 *
 * The interesting part is not parsing, it is WHAT LEAVES A HISTORY ENTRY. The
 * Back button reads the session stack, not the URL, and three rules cover
 * every move this product makes:
 *
 *   | Move                                   | Stack    | Why
 *   |----------------------------------------|----------|-------------------
 *   | a MOVED path (an old route redirecting)| replaces | the entry names a
 *   |                                        |          | route that no longer
 *   |                                        |          | exists; leaving it
 *   |                                        |          | means Back lands on
 *   |                                        |          | it, it redirects
 *   |                                        |          | forward, and you
 *   |                                        |          | arrive where you
 *   |                                        |          | started
 *   | a SECTION — a lens, a tab              | pushes   | the reader calls
 *   |                                        |          | these screens; Back
 *   |                                        |          | after three should
 *   |                                        |          | walk out through them
 *   | a FILTER — chips, sort, a search box   | replaces | four ticked chips are
 *   |                                        |          | ONE screen; Back
 *   |                                        |          | means "off this
 *   |                                        |          | list", not "untick
 *   |                                        |          | one"
 *
 * All three shipped wrong once and none of them is visible in a URL: from a
 * redirected route Back could not escape at all, Back from a screen's fourth
 * lens left the screen entirely, and Back to a list the reader had scrolled
 * halfway down landed at the top.
 *
 * SCROLL IS A PROPERTY OF A HISTORY ENTRY, NOT OF A URL. The same screen
 * reached twice is two places the reader has been, and keying a position by
 * URL collapses them onto one. Each navigation stamps a key into
 * `history.state` and files the outgoing position under it; an entry with NO
 * key is exactly the test for "somewhere new", which is the only case that
 * starts at the top.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

export interface Route {
  /** Path segments, already decoded. `#/seats/pm` → `["seats", "pm"]`. */
  path: string[];
  /** The query string, parsed. */
  query: URLSearchParams;
  /** The whole hash, for keys and comparisons. */
  hash: string;
}

const RouteContext = createContext<Route | null>(null);

export function useRoute(): Route {
  const route = useContext(RouteContext);
  if (!route) throw new Error("useRoute outside a Router");
  return route;
}

/** The current screen's first segment, or "" for the overview. */
export function useScreen(): string {
  return useRoute().path[0] ?? "";
}

export function parseHash(raw: string): Route {
  const hash = raw.replace(/^#/, "") || "/";
  const qIdx = hash.indexOf("?");
  const pathPart = qIdx < 0 ? hash : hash.slice(0, qIdx);
  const queryPart = qIdx < 0 ? "" : hash.slice(qIdx + 1);
  const path = pathPart
    .split("/")
    .filter(Boolean)
    .map((s) => {
      try {
        return decodeURIComponent(s);
      } catch {
        // A hand-edited URL with a stray `%` should land on a screen that says
        // so, not throw during routing and leave a blank page.
        return s;
      }
    });
  return { path, query: new URLSearchParams(queryPart), hash: `#${hash}` };
}

export function buildHash(
  path: string[],
  query?: URLSearchParams | Record<string, string>,
): string {
  const p = path.map((s) => encodeURIComponent(s)).join("/");
  const q =
    query instanceof URLSearchParams
      ? query
      : new URLSearchParams(Object.entries(query ?? {}).filter(([, v]) => v !== ""));
  const qs = q.toString();
  return `#/${p}${qs ? `?${qs}` : ""}`;
}

// ---------------------------------------------------------------------------
// Moved routes
// ---------------------------------------------------------------------------

/**
 * Old path → new path, applied on the FIRST segment (and the second where a
 * route carried an id).
 *
 * These links are in bookmarks, in chat threads and in other people's notes. A
 * redirect costs one `hashchange`; a dead link costs the reader the thing they
 * were looking for.
 */
const MOVED: Record<string, (path: string[], query: URLSearchParams) => string[] | null> = {
  // The seat list left the org screen and became a screen of its own.
  agents: (p) => (p.length > 1 ? ["seats", p[1] as string] : ["people"]),
  people: () => null, // already current — listed so the intent is explicit
  // NOT `work`. It redirected to `#/runs` back when "work" meant a coding
  // run, and the work board took the name — so the entry would have sent
  // every reader of a live route to a different screen, for ever, with the
  // address bar agreeing with them. A redirect whose old path is now a real
  // route is strictly worse than a dead link: the dead link is visible.
  // The "no redirect claims a path a live screen now owns" test beside
  // this holds the rule.
  tokens: () => ["spend"],
  company: () => ["org"],
  audit: () => ["config"],
  // `#/events` was the feed; `#/events/{id}` is still one event.
  events: (p) => (p.length > 1 ? null : ["activity"]),
};

/**
 * The first segments a redirect claims, so a test can hold them apart from
 * the ones a live screen owns. Exported for that test alone — see
 * `router.test.ts`, and the `work` entry that is deliberately absent above.
 */
export const MOVED_PATHS: string[] = Object.keys(MOVED).filter(
  (key) => MOVED[key]?.([key], new URLSearchParams()) !== null,
);

/**
 * A LENS can move too, and it does not look like a moved route: the path is
 * still live, so the screen would simply fall back to its default lens and
 * silently put the reader somewhere else.
 */
const MOVED_LENSES: Record<string, string[]> = {
  "org?seats": ["people"],
  "org?people": ["people"],
};

interface Redirect {
  path: string[];
  query: URLSearchParams;
}

function redirectFor(route: Route): Redirect | null {
  const head = route.path[0] ?? "";
  const lens = route.query.get("lens");
  if (lens) {
    const moved = MOVED_LENSES[`${head}?${lens}`];
    if (moved) {
      const query = new URLSearchParams(route.query);
      query.delete("lens");
      return { path: moved, query };
    }
  }
  const rule = MOVED[head];
  if (!rule) return null;
  const next = rule(route.path, route.query);
  if (!next) return null;
  const query = new URLSearchParams(route.query);
  if (head === "audit") query.set("lens", "audit");
  return { path: next, query };
}

// ---------------------------------------------------------------------------
// Scroll memory
// ---------------------------------------------------------------------------

const positions = new Map<string, number>();
let keySeq = 0;

function stateKey(): string | null {
  const st = history.state as { crewletKey?: string } | null;
  return st?.crewletKey ?? null;
}

function stampKey(): string {
  const key = `k${++keySeq}`;
  history.replaceState({ ...(history.state as object), crewletKey: key }, "");
  return key;
}

// ---------------------------------------------------------------------------
// Navigation
// ---------------------------------------------------------------------------

function scrollTarget(): HTMLElement | null {
  return document.getElementById("screen-scroll");
}

function go(hash: string, replace: boolean): void {
  // File the outgoing position under the entry we are leaving, before the
  // entry changes.
  const from = stateKey();
  const el = scrollTarget();
  if (from && el) positions.set(from, el.scrollTop);

  if (replace) history.replaceState({ crewletKey: from }, "", hash);
  else history.pushState({}, "", hash);
  // `pushState` does not fire `hashchange`, so the router is told directly.
  window.dispatchEvent(new Event("crewlet:route"));
}

export interface Navigator {
  /** A new screen. Pushes. */
  to: (path: string[], query?: URLSearchParams | Record<string, string>) => void;
  /** A lens or a tab within this screen. Pushes — the reader called it. */
  section: (key: string, value: string) => void;
  /** A chip, a sort, a search box. Replaces — it is the same screen. */
  filter: (patch: Record<string, string | null>) => void;
  /** A moved path. Replaces, so Back cannot land on a route that redirects. */
  replace: (path: string[], query?: URLSearchParams | Record<string, string>) => void;
  back: () => void;
}

const NavContext = createContext<Navigator | null>(null);

export function useNavigator(): Navigator {
  const nav = useContext(NavContext);
  if (!nav) throw new Error("useNavigator outside a Router");
  return nav;
}

/** An href for an anchor, so a link is a real link — middle-clickable. */
export function href(path: string[], query?: Record<string, string>): string {
  return buildHash(path, query);
}

// ---------------------------------------------------------------------------

export function Router({ children }: { children: ReactNode }) {
  const [route, setRoute] = useState<Route>(() => parseHash(location.hash));
  const pending = useRef<string | null>(null);

  useEffect(() => {
    const read = () => setRoute(parseHash(location.hash));
    window.addEventListener("hashchange", read);
    window.addEventListener("popstate", read);
    window.addEventListener("crewlet:route", read);
    return () => {
      window.removeEventListener("hashchange", read);
      window.removeEventListener("popstate", read);
      window.removeEventListener("crewlet:route", read);
    };
  }, []);

  // Redirects run as an effect rather than during render: a redirect is a
  // history mutation, and doing it in a render body makes the first paint of
  // the dead route real.
  useEffect(() => {
    const r = redirectFor(route);
    if (!r) return;
    const next = buildHash(r.path, r.query);
    if (next === route.hash) return;
    go(next, true);
  }, [route]);

  const nav = useMemo<Navigator>(
    () => ({
      to: (path, query) => go(buildHash(path, query), false),
      replace: (path, query) => go(buildHash(path, query), true),
      section: (key, value) => {
        const query = new URLSearchParams(parseHash(location.hash).query);
        if (value) query.set(key, value);
        else query.delete(key);
        go(buildHash(parseHash(location.hash).path, query), false);
      },
      filter: (patch) => {
        const current = parseHash(location.hash);
        const query = new URLSearchParams(current.query);
        for (const [k, v] of Object.entries(patch)) {
          if (v == null || v === "") query.delete(k);
          else query.set(k, v);
        }
        go(buildHash(current.path, query), true);
      },
      back: () => history.back(),
    }),
    [],
  );

  // Restore scroll for an entry we have seen; start at the top for one we have
  // not. A restored position is re-applied for a short window while the rows
  // arrive — a scroll is clamped to the height that exists, so one attempt
  // lands short — and abandoned the moment the reader touches the page.
  useEffect(() => {
    const key = stateKey();
    const el = scrollTarget();
    if (!el) return;
    if (!key) {
      stampKey();
      el.scrollTop = 0;
      return;
    }
    const want = positions.get(key);
    if (want == null) {
      el.scrollTop = 0;
      return;
    }
    pending.current = key;
    let tries = 0;
    const settle = () => {
      if (pending.current !== key || tries++ > 12) return;
      el.scrollTop = want;
      if (Math.abs(el.scrollTop - want) > 1) requestAnimationFrame(settle);
    };
    const abandon = () => {
      pending.current = null;
    };
    el.addEventListener("wheel", abandon, { once: true, passive: true });
    el.addEventListener("touchstart", abandon, { once: true, passive: true });
    requestAnimationFrame(settle);
    return () => {
      el.removeEventListener("wheel", abandon);
      el.removeEventListener("touchstart", abandon);
    };
  }, [route.hash]);

  return (
    <RouteContext.Provider value={route}>
      <NavContext.Provider value={nav}>{children}</NavContext.Provider>
    </RouteContext.Provider>
  );
}

/**
 * A parameter that lives in the URL and behaves like state.
 *
 * `kind` decides the history rule, which is the only judgement call in this
 * router: a lens or a tab is a screen the reader called, a chip is a narrowing
 * of the one they are on.
 */
export function useParam(
  key: string,
  fallback: string,
  kind: "section" | "filter" = "filter",
): [string, (value: string) => void] {
  const route = useRoute();
  const nav = useNavigator();
  const value = route.query.get(key) ?? fallback;
  const set = useCallback(
    (next: string) => {
      if (kind === "section") nav.section(key, next === fallback ? "" : next);
      else nav.filter({ [key]: next === fallback ? null : next });
    },
    [nav, key, fallback, kind],
  );
  return [value, set];
}
