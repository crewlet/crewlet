/**
 * The atlas's address: the search, the filters and the selection the map and
 * the catalogue share, and the engine parameters they become. Both read them
 * from here, so neither can hold a selection or a search the other does not.
 */

import { useCallback, useEffect, useMemo } from "react";
import { refToken } from "~/app/frame/objects.ts";
import { usePeek } from "~/app/frame/peekAddress.ts";
import { useNavigator, useParam, useRoute } from "~/app/router.tsx";
import type {
  AtlasHealth,
  AtlasKind,
  AtlasLane,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";

/**
 * How often an open atlas screen asks again: thirty seconds, the cadence of a
 * screen whose signals carry an alarm nothing pushes (Schedules, the queue
 * count). The atlas changes at a settings revision or a record, minutes apart.
 */
export const ATLAS_POLL_MS = 30_000;

/** Which way a link narrows the path the map lights from the selection. */
export type TraceChoice = "" | "up" | "down" | "both";

/** The narrowings the toolbar sets, as the address holds them. */
export interface AtlasFilters {
  q: string;
  kind: AtlasKind | "";
  lane: AtlasLane | "";
  vendor: AtlasVendor | "";
  confidence: AtlasWord | "";
  health: AtlasHealth | "";
  /** The unconfirmed entries are taken in, which a read leaves out unless asked. */
  unconfirmed: boolean;
}

/** The address keys a filter lives at, every one replacing the history entry. */
export const FILTER_KEYS = [
  "q",
  "kind",
  "lane",
  "vendor",
  "confidence",
  "health",
  "unconfirmed",
] as const;

/** The filters the address holds. */
export function useAtlasFilters(): AtlasFilters {
  const route = useRoute();
  return useMemo(() => {
    const get = (key: (typeof FILTER_KEYS)[number]) => route.query.get(key) ?? "";
    return {
      q: get("q"),
      kind: get("kind") as AtlasFilters["kind"],
      lane: get("lane") as AtlasFilters["lane"],
      vendor: get("vendor") as AtlasFilters["vendor"],
      confidence: get("confidence") as AtlasFilters["confidence"],
      health: get("health") as AtlasFilters["health"],
      unconfirmed: get("unconfirmed") === "true",
    };
  }, [route]);
}

/** Set one filter, or clear the ones named. */
export function useSetFilter(): {
  set: (key: (typeof FILTER_KEYS)[number], value: string) => void;
  clearKeys: (keys: readonly (typeof FILTER_KEYS)[number][]) => void;
} {
  const nav = useNavigator();
  return useMemo(
    () => ({
      set: (key, value) => nav.filter({ [key]: value === "" ? null : value }),
      clearKeys: (keys) => nav.filter(Object.fromEntries(keys.map((key) => [key, null]))),
    }),
    [nav],
  );
}

/** Whether any filter narrows the atlas. */
export function narrowed(filters: AtlasFilters): boolean {
  return FILTER_KEYS.some((key) =>
    key === "unconfirmed" ? filters.unconfirmed : filters[key] !== "",
  );
}

/** The engine's parameters for the filters: only what is set. */
export function queryParams(filters: AtlasFilters): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (const key of FILTER_KEYS) {
    if (key === "unconfirmed") {
      if (filters.unconfirmed) out.unconfirmed = true;
    } else if (filters[key] !== "") {
      out[key] = filters[key];
    }
  }
  return out;
}

/** The address key the selection lives at, while no sheet is open on it. */
export const SELECT_KEY = "sel";

/**
 * The one selection the map and the catalogue share. It is the entry the
 * sheet is open on, or the one the address names (`sel=`): a card pressed on
 * the map is selected without opening anything, and a sheet that closes
 * leaves its entry selected.
 */
export function useSelection(): {
  selectedId: string | null;
  /** Select an entry, moving an open sheet to it; `null` clears and closes. */
  select: (id: string | null) => void;
} {
  const nav = useNavigator();
  const route = useRoute();
  const peek = usePeek();
  const peeked = peek?.kind === "atlas" ? peek.id : null;
  const named = route.query.get(SELECT_KEY) ?? "";
  // THE SHEET'S ENTRY IS THE SELECTION, so closing the sheet leaves it lit.
  useEffect(() => {
    if (peeked !== null && named !== peeked) nav.filter({ [SELECT_KEY]: peeked });
  }, [nav, peeked, named]);
  const select = useCallback(
    (id: string | null) => {
      // A TRACE BELONGS TO THE ENTRY A LINK ARRIVED ON, never to the next one.
      if (id === null) {
        nav.filter({ [SELECT_KEY]: null, [TRACE_KEY]: null, ...(peeked ? { peek: null } : {}) });
      } else {
        nav.filter({
          [SELECT_KEY]: id,
          [TRACE_KEY]: null,
          ...(peeked ? { peek: refToken({ kind: "atlas", id }) } : {}),
        });
      }
    },
    [nav, peeked],
  );
  return { selectedId: peeked ?? (named === "" ? null : named), select };
}

/** The address key the map's trace lives at: what a link lit along a trace carries. */
export const TRACE_KEY = "trace";

/** Whether `value` is a way the trace can light. */
export function isTraceChoice(value: string): value is Exclude<TraceChoice, ""> {
  return value === "up" || value === "down" || value === "both";
}

/**
 * The way a link asked the selection to light: what depends on it, what it
 * depends on, or "" for its whole path both ways, which is how the map lights
 * a selection nobody narrowed.
 */
export function useTrace(): TraceChoice {
  const [trace] = useParam(TRACE_KEY, "", "filter");
  return isTraceChoice(trace) ? trace : "";
}
