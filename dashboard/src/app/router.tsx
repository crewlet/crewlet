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
 *
 * WORK THAT EXISTS NOWHERE ELSE IS NOT LEFT BEHIND UNASKED. A surface holding
 * it (an editor with typed changes) registers a leave guard, and every move
 * to ANOTHER ENTRY is put to it first: a push from code, a link, and Back or
 * Forward. A replace is never held, by the table above: it stays on the
 * entry. Back has already happened by the time a page hears of it, so a held
 * one is undone at once and made again only if the reader agrees, which is
 * why each entry carries its place in the session (`crewletIndex`) as well as
 * its key. A reload or a closed tab gets the browser's own prompt.
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

/**
 * Whether a path is the page the reader is already on.
 *
 * For the links a component draws WITHOUT knowing where it is rendered. A
 * phase card carries "event →" to its own event, which is a way out on the
 * turn and on the seat — and on that event's own page it is a link back to
 * itself: the reader clicks, the URL does not change, nothing moves, and the
 * only thing they learn is that the control was a lie.
 *
 * Compared on the parsed path rather than on the hash string, so a query the
 * reader happens to have on the URL — a filter, a tab — does not make the
 * same page look like a different one.
 */
export function useIsCurrent(path: readonly string[]): boolean {
  // NOT useRoute: that one throws outside a Router, deliberately, because a
  // screen reached without one is a wiring bug. This is a leaf component's
  // question, asked by cards that render perfectly well on their own — and
  // outside a router the honest answer is "no", there is no page to be on.
  // Throwing here would make a router the price of drawing a phase card.
  const route = useContext(RouteContext);
  return route ? samePath(route.path, path) : false;
}

/**
 * Whether two parsed paths name the same page.
 *
 * Split out of the hook so the RULE is testable beside the router's other
 * rules, which run with no DOM at all. Compared segment by segment rather than
 * by joining: a join makes `["events", "a/b"]` and `["events", "a", "b"]`
 * the same string, and an id is not a path.
 */
export function samePath(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((seg, i) => seg === b[i]);
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
// Where the reader stands in the session
// ---------------------------------------------------------------------------

/** An entry's place among the session's entries, as the router stamped it; `null` for none. */
function entryIndex(): number | null {
  const index = (history.state as { crewletIndex?: unknown } | null)?.crewletIndex;
  return typeof index === "number" ? index : null;
}

/**
 * The entry the router is on. Module state beside the scroll positions,
 * because both describe the one session history a page has, whichever
 * Router is mounted.
 */
let settled = { hash: "", index: 0 };

/** Whether a held Back or Forward is being undone, and the move it was should be ignored. */
let reverting = false;

/** Whether the reader agreed to a held Back or Forward, which the next move makes. */
let bypassing = false;

/** Adopts the entry the page loaded on, stamping its place if the router never has. */
function adoptEntry(): void {
  let index = entryIndex();
  if (index === null) {
    index = 0;
    history.replaceState({ ...(history.state as object), crewletIndex: index }, "");
  }
  settled = { hash: location.hash, index };
  reverting = false;
  bypassing = false;
}

// ---------------------------------------------------------------------------
// Navigation
// ---------------------------------------------------------------------------

/**
 * The ONE scroll container: the shell's main region, named by its id.
 *
 * Exported because two other things read it, and a second spelling of it is a
 * second thing that stops matching when the shell moves: the settled list asks
 * whether the reader is at the top before it splices rows in, and the palette
 * has the same reason the router does for scrolling this element directly
 * rather than calling `scrollIntoView`, which scrolls every scrollable
 * ancestor it can find.
 */
export function screenScroller(): HTMLElement | null {
  return document.getElementById("screen-scroll");
}

function scrollTarget(): HTMLElement | null {
  return screenScroller();
}

function go(hash: string, replace: boolean, agreed = false): void {
  // A PUSH IS A MOVE TO ANOTHER ENTRY, which a surface holding work may hold
  // until the reader agrees. A replace stays on the entry, and is never held.
  if (!replace && !agreed && held(parseHash(hash), () => go(hash, false, true))) return;

  // File the outgoing position under the entry we are leaving, before the
  // entry changes.
  const from = stateKey();
  const el = scrollTarget();
  if (from && el) positions.set(from, el.scrollTop);

  // THE PLACE IS THE ENTRY'S OWN. A screen can navigate in its first effect,
  // which React runs before this router's own (`adoptEntry`), and after a
  // reload the entry stands wherever the session had got to while `settled`
  // still says 0: a replace would stamp the wrong place on it, and every Back
  // after it would be undone in the wrong direction.
  const here = entryIndex() ?? settled.index;
  const index = replace ? here : here + 1;
  if (replace) history.replaceState({ crewletKey: from, crewletIndex: index }, "", hash);
  else history.pushState({ crewletIndex: index }, "", hash);
  settled = { hash: location.hash, index };
  // `pushState` does not fire `hashchange`, so the router is told directly.
  window.dispatchEvent(new Event("crewlet:route"));
}

/**
 * Reads a move the BROWSER made (Back, Forward, a link, a URL typed into the
 * address bar) and answers whether the router follows it.
 *
 * A browser fires two events for one such move (`popstate`, then
 * `hashchange`), and one more pair when a held move is undone; the second of
 * a pair, and the pair of the undo, find the router already where the entry
 * is and change nothing. An entry the browser made itself (a link, a typed
 * URL) carries no place yet: it is the one after the entry the reader was
 * on, and is stamped so.
 */
function traversed(): boolean {
  const hash = location.hash;
  const stamped = entryIndex();
  if (stamped === settled.index && hash === settled.hash) {
    reverting = false;
    return false;
  }
  if (reverting) return false;
  const index = stamped ?? settled.index + 1;
  if (stamped === null) {
    history.replaceState({ ...(history.state as object), crewletIndex: index }, "");
  }
  const delta = index - settled.index;
  if (delta !== 0 && !bypassing) {
    const leave = () => {
      bypassing = true;
      history.go(delta);
    };
    if (held(parseHash(hash), leave)) {
      reverting = true;
      history.go(-delta);
      return false;
    }
  }
  bypassing = false;
  settled = { hash, index };
  return true;
}

// ---------------------------------------------------------------------------
// Leave guards
// ---------------------------------------------------------------------------

/**
 * Asked before the reader moves to another entry, with the route the move
 * goes to. Answering true holds the move: the guard asks the reader, and
 * calls `leave` to make the move once they agree to lose what it holds.
 * Answering false lets the move go.
 */
export type LeaveGuard = (to: Route, leave: () => void) => boolean;

/**
 * The guards that hold moves, in the order they began to hold. The one that
 * began last is asked first: it is the surface the reader opened last (an
 * editor over the lens that holds a draft), so its question is the one on
 * top.
 */
const guards: { readonly ask: LeaveGuard }[] = [];

/** Whatever a reload or a closed tab would lose, for the browser's own prompt. */
const unloadHolders = new Set<object>();

/**
 * Holds the browser's prompt for a reload or a closed tab until the result is
 * called.
 *
 * LISTENING ONLY WHILE SOMETHING HOLDS. A page with a `beforeunload` listener
 * is kept out of the browser's back-forward cache by some browsers, so every
 * screen of the dashboard would be loaded from scratch on a Back from another
 * site. The listener is added by the first holder and removed with the last.
 */
function holdUnload(holder: object): () => void {
  if (unloadHolders.size === 0) window.addEventListener("beforeunload", onBeforeUnload);
  unloadHolders.add(holder);
  return () => {
    unloadHolders.delete(holder);
    if (unloadHolders.size === 0) window.removeEventListener("beforeunload", onBeforeUnload);
  };
}

/**
 * Whether a guard holds a move to `to`, asking from the last guard down.
 * EVERY GUARD IS ASKED BEFORE THE MOVE IS MADE: the `leave` a guard is handed
 * asks the guards below it, and only the last agreement makes the move, so an
 * editor agreeing to lose its form never also loses the lens's work unasked.
 * The guards are the ones holding when the move was asked for, because
 * agreeing can close a surface and take its guard off the list.
 */
function held(to: Route, move: () => void): boolean {
  const asking = [...guards];
  const from = (i: number): boolean => {
    for (let at = i; at >= 0; at--) {
      if (asking[at]!.ask(to, () => from(at - 1) || move())) return true;
    }
    return false;
  };
  return from(asking.length - 1);
}

function onBeforeUnload(e: BeforeUnloadEvent): void {
  // The browser shows its own sentence; the page only says that it holds
  // something. Both halves, because browsers honour one or the other.
  e.preventDefault();
  e.returnValue = "";
}

/**
 * Holds every move to another entry while `guard` is set, and a reload or a
 * closed tab asks first: for work that exists nowhere else, such as an
 * editor's typed changes. `null` holds nothing.
 */
export function useLeaveGuard(guard: LeaveGuard | null): void {
  const latest = useRef(guard);
  latest.current = guard;
  const holding = guard !== null;
  useEffect(() => {
    if (!holding) return;
    const entry = { ask: (to: Route, leave: () => void) => latest.current?.(to, leave) ?? false };
    guards.push(entry);
    const release = holdUnload(entry);
    return () => {
      guards.splice(guards.indexOf(entry), 1);
      release();
    };
  }, [holding]);
}

/**
 * A reload or a closed tab asks first while `holding`: for work that
 * survives a move within the page (it is kept elsewhere, such as the org
 * builder's draft in session storage) but not the tab going away.
 */
export function useUnloadGuard(holding: boolean): void {
  useEffect(() => (holding ? holdUnload({}) : undefined), [holding]);
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

/** An href for an anchor, so a link is a real link: middle-clickable. */
export function href(path: string[], query?: Record<string, string>): string {
  return buildHash(path, query);
}

// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Revealing what a link points at
// ---------------------------------------------------------------------------

/**
 * An entry the reader has just ARRIVED at, and whether a reveal may still
 * move the page for it.
 *
 * `open` closes on the first reveal and on the first sign the reader is
 * reading: a wheel, a touch, a pointer or a key on the scroller. The page
 * moves only when the reader is not reading.
 */
interface Arrival {
  hash: string;
  open: boolean;
}

interface Arrivals {
  current: { value: Arrival | null };
  /** Reveal attempts to run the moment the router has settled an arrival. */
  attempts: Set<() => void>;
}

const ArrivalContext = createContext<Arrivals | null>(null);

/**
 * Scroll an element into view when the reader arrives at a NEW entry.
 *
 * For a link that names a thing inside a screen (`#/org?unit=Backend`): the
 * reader followed it to see that thing, and a long chart that opens at the
 * top has put it somewhere they have to hunt for.
 *
 * THREE RULES, each of which the obvious implementation breaks:
 *
 *  - IT RUNS AFTER THE ROUTER'S SCROLL RESET. A screen's own effect runs
 *    before this component's (React runs a child's passive effects first), so
 *    a screen that scrolled on its own was scrolled straight back to the top
 *    by the reset for a new entry.
 *  - ONLY FOR SOMEWHERE NEW, never while a Back restore is pending. Back to a
 *    chart the reader had scrolled means back to where they were, and a
 *    reveal fighting the restore for its frames is a page that jumps twice.
 *    A filter change replaces the entry and restores its position too, so it
 *    never reveals: the reader is already looking at what they changed.
 *  - IT SCROLLS `#screen-scroll` DIRECTLY. `scrollIntoView` scrolls every
 *    scrollable ancestor it finds, including ones this layout does not own,
 *    and jsdom does not implement it at all. The element's own
 *    `scroll-margin-top` is honoured, so the stylesheet says how much room to
 *    leave above it.
 *
 * The element may not exist yet on arrival (the org projection arrives on the
 * socket after the route does), so an arrival stays open and the hook tries
 * again on every render until it finds the element or the reader moves.
 * `null` reveals nothing.
 */
export function useRevealOnArrival(elementId: string | null): void {
  const arrivals = useContext(ArrivalContext);
  const route = useContext(RouteContext);
  const latest = useRef({ elementId, hash: route?.hash ?? "" });
  latest.current = { elementId, hash: route?.hash ?? "" };

  const attempt = useCallback(() => {
    const arrival = arrivals?.current.value;
    const { elementId: id, hash } = latest.current;
    if (!id || !arrival || !arrival.open || arrival.hash !== hash) return;
    const scroller = scrollTarget();
    const target = document.getElementById(id);
    if (!scroller || !target || !scroller.contains(target)) return;
    arrival.open = false;
    const margin = parseFloat(getComputedStyle(target).scrollMarginTop) || 0;
    const offset = target.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
    scroller.scrollTop += offset - margin;
  }, [arrivals]);

  useEffect(() => {
    if (!arrivals) return;
    arrivals.attempts.add(attempt);
    return () => {
      arrivals.attempts.delete(attempt);
    };
  }, [arrivals, attempt]);

  // Every render, deliberately: the element appears when the data does, and
  // the attempt is one lookup that does nothing once the arrival has closed.
  useEffect(attempt);
}

export function Router({ children }: { children: ReactNode }) {
  const [route, setRoute] = useState<Route>(() => parseHash(location.hash));
  const pending = useRef<string | null>(null);
  const arrivals = useMemo<Arrivals>(() => ({ current: { value: null }, attempts: new Set() }), []);

  useEffect(() => {
    adoptEntry();
    // The entry the browser is on, when this state is not already on it. The
    // comparison is what makes the catch-up below free: an event for a hash
    // this router already holds leaves the route object alone, so nothing
    // below it re-renders for a move that did not happen.
    const read = () =>
      setRoute((was) => {
        const next = parseHash(location.hash);
        return next.hash === was.hash ? was : next;
      });
    const follow = () => {
      if (traversed()) read();
    };
    window.addEventListener("hashchange", follow);
    window.addEventListener("popstate", follow);
    window.addEventListener("crewlet:route", read);
    // A SCREEN CAN MOVE IN ITS OWN FIRST EFFECT, and React runs a child's
    // effects before its parent's, so the listeners above did not exist when
    // it did: a table correcting a page past the end of its rows wrote the
    // corrected page into the URL and this state never heard of it, leaving
    // the reader on a page the link no longer names and no rows on it.
    read();
    return () => {
      window.removeEventListener("hashchange", follow);
      window.removeEventListener("popstate", follow);
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
    arrivals.current.value = null;
    if (!el) return;
    // Somewhere new starts at the top, and is the one case a link may then
    // reveal what it points at. See [useRevealOnArrival].
    const arrive = () => {
      el.scrollTop = 0;
      const arrival: Arrival = { hash: route.hash, open: true };
      arrivals.current.value = arrival;
      const reading = () => {
        arrival.open = false;
      };
      const events = ["wheel", "touchstart", "pointerdown", "keydown"] as const;
      for (const type of events) el.addEventListener(type, reading, { once: true, passive: true });
      for (const attempt of arrivals.attempts) attempt();
      return () => {
        for (const type of events) el.removeEventListener(type, reading);
      };
    };
    if (!key) {
      stampKey();
      return arrive();
    }
    const want = positions.get(key);
    if (want == null) return arrive();
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
  }, [route.hash, arrivals]);

  return (
    <RouteContext.Provider value={route}>
      <NavContext.Provider value={nav}>
        <ArrivalContext.Provider value={arrivals}>{children}</ArrivalContext.Provider>
      </NavContext.Provider>
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
