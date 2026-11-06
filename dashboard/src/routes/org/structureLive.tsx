/**
 * The running company as Structure's two tabs read it: every seat's live
 * reading, taken once for the page (`lib/seatLive.ts`) and handed to each
 * chart card and table row through context rather than read again by each,
 * and the status the page filters by (`show`): the table's chip, and the
 * chart's legend key where it has one.
 */

import { createContext, useContext, type ReactNode } from "react";
import { useParam } from "~/app/router.tsx";
import {
  LEGEND_FILTERS,
  matches,
  statusFilterOf,
  type LiveOrg,
  type SeatLive,
  type StatusFilter,
} from "~/lib/seatLive.ts";

const Live = createContext<LiveOrg | null>(null);

export function StructureLiveProvider({
  value,
  children,
}: {
  value: LiveOrg;
  children: ReactNode;
}) {
  return <Live.Provider value={value}>{children}</Live.Provider>;
}

/**
 * The page's live reading, or null outside Structure: the builder is also
 * drawn by suites with no running company, and draws no live state there.
 */
export function useStructureLive(): LiveOrg | null {
  return useContext(Live);
}

/**
 * A node's live reading, by the handle the running company knows the seat
 * by: its saved one, until a save applies a new handle.
 */
export function useSeatLive(handle: string | undefined): SeatLive | undefined {
  const live = useContext(Live);
  return handle ? live?.live.get(handle) : undefined;
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
  const live = useContext(Live);
  const [show] = useChartFilter();
  return (view) => {
    if (show === "all" || !live || !view?.handle || (view.type && view.type !== "seat")) {
      return false;
    }
    const s = live.live.get(view.saved?.handle ?? view.handle);
    return !s || !matches(s, show);
  };
}
