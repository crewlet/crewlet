/**
 * The atlas's address: the filters the map and the list share, and the
 * engine parameters they become. A filter carries from one shape to the
 * other because both read it from here.
 */

import { useMemo } from "react";
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

/** The two shapes one answer is drawn in. */
export type Shape = "map" | "list";

/** Which way the map's trace lights from the selection. */
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

/** Set one filter, or clear every one. */
export function useSetFilter(): {
  set: (key: (typeof FILTER_KEYS)[number], value: string) => void;
  clear: () => void;
} {
  const nav = useNavigator();
  return useMemo(
    () => ({
      set: (key, value) => nav.filter({ [key]: value === "" ? null : value }),
      clear: () => nav.filter(Object.fromEntries(FILTER_KEYS.map((key) => [key, null]))),
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

/** The shape the address draws, the map unless it says the list. */
export function useShape(): [Shape, (shape: Shape) => void] {
  const [shape, setShape] = useParam("shape", "map", "section");
  return [shape === "list" ? "list" : "map", setShape as (shape: Shape) => void];
}

/** The map's trace, off unless the address says which way. */
export function useTrace(): [TraceChoice, (trace: TraceChoice) => void] {
  const [trace, setTrace] = useParam("trace", "", "filter");
  const valid: TraceChoice = trace === "up" || trace === "down" || trace === "both" ? trace : "";
  return [valid, setTrace as (trace: TraceChoice) => void];
}
