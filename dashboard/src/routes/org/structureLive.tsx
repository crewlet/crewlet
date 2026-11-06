/**
 * The running company as Structure's two tabs read it: every seat's live
 * reading, taken once for the page (`lib/seatLive.ts`) and read by each chart
 * card and table row for itself, and the status the page filters by (`show`):
 * the table's chip, and the chart's legend key where it has one.
 *
 * A STORE, NOT A CONTEXT VALUE. The reading is taken again on every tick of the
 * clock and every push, and handed down as one value it drew every row of the
 * page each second with nothing on screen changed. The page holds it here: a
 * seat's reading keeps its identity while it says the same thing, a reading that
 * says nothing new tells nobody, and each row subscribes to what it draws
 * ([useLiveValue]), so a push draws only the row it changed.
 */

import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { useParam } from "~/app/router.tsx";
import {
  LEGEND_FILTERS,
  matches,
  statusFilterOf,
  useLiveOrg,
  type LiveOrg,
  type SeatLive,
  type StatusFilter,
} from "~/lib/seatLive.ts";
import { sameValue } from "~/protocol/equal.ts";

/** Whether two readings of a seat say the same. Its org entry is compared by identity. */
function sameReading(a: SeatLive, b: SeatLive): boolean {
  return a.seat === b.seat && sameValue({ ...a, seat: null }, { ...b, seat: null });
}

/** `next` with every seat that says what it said in `was` kept as it was; `was` when all do. */
function keepingWhatHolds(was: LiveOrg, next: LiveOrg): LiveOrg {
  const live = new Map<string, SeatLive>();
  let moved = was.live.size !== next.live.size;
  for (const [handle, reading] of next.live) {
    const held = was.live.get(handle);
    const kept = held && sameReading(held, reading) ? held : reading;
    if (kept !== held) moved = true;
    live.set(handle, kept);
  }
  const same = was.index === next.index && was.loaded === next.loaded && was.nameOf === next.nameOf;
  return !moved && same ? was : { ...next, live };
}

/** The page's live reading, as a store a row subscribes to. */
class LiveStore {
  private readonly listeners = new Set<() => void>();
  constructor(private value: LiveOrg) {}
  read = (): LiveOrg => this.value;
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };
  set(next: LiveOrg): void {
    const kept = keepingWhatHolds(this.value, next);
    if (kept === this.value) return;
    this.value = kept;
    for (const listener of [...this.listeners]) listener();
  }
}

const Live = createContext<LiveStore | null>(null);
const NOTHING = () => () => {};

export function StructureLiveProvider({
  value,
  children,
}: {
  value: LiveOrg;
  children: ReactNode;
}) {
  const [store] = useState(() => new LiveStore(value));
  useLayoutEffect(() => store.set(value), [store, value]);
  return <Live.Provider value={store}>{children}</Live.Provider>;
}

/**
 * The page's reading, around what it is given. TAKEN HERE, NOT IN THE PAGE: this
 * draws again on every tick, and `children` are the page's own elements, which
 * React leaves as they are while the page has not drawn again.
 */
export function LiveStructure({ children }: { children: ReactNode }) {
  return <StructureLiveProvider value={useLiveOrg()}>{children}</StructureLiveProvider>;
}

/**
 * One value read off the page's live reading, drawing the caller again only when
 * it changes: `same` says when, identity by default, and a seat's reading keeps
 * its identity while it says the same thing. The reading is null outside
 * Structure: the builder is also drawn by suites with no running company.
 */
export function useLiveValue<T>(
  select: (live: LiveOrg | null) => T,
  same: (a: T, b: T) => boolean = Object.is,
): T {
  const store = useContext(Live);
  const held = useRef<{ value: T } | null>(null);
  const read = () => {
    const value = select(store ? store.read() : null);
    // WRITTEN DURING RENDER, as a cache: a function of the readings drawn.
    if (held.current && same(held.current.value, value)) return held.current.value;
    held.current = { value };
    return value;
  };
  return useSyncExternalStore(store ? store.subscribe : NOTHING, read, read);
}

/**
 * The reading as it is now, for a sort or a press that asks at its own moment:
 * reading it subscribes nothing.
 */
export function useLiveNow(): () => LiveOrg | null {
  const store = useContext(Live);
  return store ? store.read : NO_READING;
}
const NO_READING = () => null;

/** A seat's reading, by the handle the running company knows it by; see [liveHandleOf]. */
export function useSeatLive(handle: string | undefined): SeatLive | undefined {
  return useLiveValue((live) => (handle ? live?.live.get(handle) : undefined));
}

/**
 * The handles of the seats `keep` keeps, drawing the caller again only when
 * the set changes, as a filter's rows and who may be picked do.
 */
export function useLiveHandles(keep: (s: SeatLive) => boolean): ReadonlySet<string> {
  const key = useLiveValue((live) =>
    live
      ? [...live.live.values()]
          .filter(keep)
          .map((s) => s.seat.handle)
          .join("\n")
      : "",
  );
  return useMemo(() => new Set(key === "" ? [] : key.split("\n")), [key]);
}

/** The status chip on screen, kept in the page's address (`show`) so a return finds it chosen. */
export function useStatusFilter(): [StatusFilter, (next: StatusFilter) => void] {
  const [value, set] = useParam("show", "all", "filter");
  return [statusFilterOf(value), set];
}

/** The filter the chart draws: the address's, when its legend keys it, and all otherwise. */
export function useChartFilter(): [StatusFilter, (next: StatusFilter) => void] {
  const [show, set] = useStatusFilter();
  return [(LEGEND_FILTERS as readonly string[]).includes(show) ? show : "all", set];
}

/** What names a seat to the running company: its saved handle, until a save applies a new one. */
export interface SeatRef {
  readonly type?: string;
  readonly handle: string;
  readonly saved?: { readonly handle: string } | null | undefined;
}

/** The handle the running company knows a seat node by. */
export function liveHandleOf(view: SeatRef): string {
  return view.saved?.handle ?? view.handle;
}

/**
 * Whether the chart's legend leaves a node out: a seat its pressed key does
 * not keep, drawn in grey. Units, the company, and every node while no key is
 * pressed stay as they are.
 */
export function useSeatDimmed(): (view: Partial<SeatRef> | undefined) => boolean {
  const [show] = useChartFilter();
  const reading = useLiveValue((live) => live !== null);
  const kept = useLiveHandles((s) => show !== "all" && matches(s, show));
  return (view) => {
    if (show === "all" || !reading || !view?.handle || (view.type && view.type !== "seat")) {
      return false;
    }
    return !kept.has(view.saved?.handle ?? view.handle);
  };
}
