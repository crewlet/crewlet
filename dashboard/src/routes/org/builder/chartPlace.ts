/**
 * Where a chart of Structure stands, kept in the page's address so a Back, a
 * reload and a seat page's crumb put it back exactly: which branches the
 * reader closed (`closed`), and the zoom and pan they left it at (`zoom`).
 *
 * A SAVED VIEW IS PUT BACK ONLY ON THE CHART IT WAS TAKEN ON. It is tagged
 * with the chart's fit key (the arrangement, the closed branches, the status
 * chip and the company's revision), so a view of the teams is never laid over
 * the reporting lines, and one taken before a layout action that refits the
 * chart is not laid over its new fit.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { CanvasView, TreeInput } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import type { BaseCompany } from "./model/reducer.ts";

/** How long the view rests before it is written into the address: the end of a gesture, not each frame. */
const VIEW_SETTLE_MS = 250;

/** Every node of a forest that has children: the branches a chart can open and close. */
export function branchesOf(nodes: readonly TreeInput[]): string[] {
  const out: string[] = [];
  const walk = (list: readonly TreeInput[]) => {
    for (const node of list) {
      if (node.children?.length) {
        out.push(node.id);
        walk(node.children);
      }
    }
  };
  walk(nodes);
  return out;
}

/** The address's closed branches, as ids. */
export function closedOf(value: string): ReadonlySet<string> {
  return new Set(value.split(",").filter(Boolean));
}

/** A short stable tag of a fit key (FNV-1a), so an address carries no chart's whole shape. */
export function tagOf(key: string): string {
  let h = 2166136261;
  for (let i = 0; i < key.length; i++) h = Math.imul(h ^ key.charCodeAt(i), 16777619) >>> 0;
  return h.toString(36);
}

/** A view as the address writes it: `k,x,y,tag`. */
export function writeView(view: CanvasView, tag: string): string {
  return `${view.k.toFixed(4)},${view.x.toFixed(1)},${view.y.toFixed(1)},${tag}`;
}

/** The view an address holds for the chart tagged `tag`, or undefined. */
export function readView(value: string, tag: string): CanvasView | undefined {
  const [ks, xs, ys, at] = value.split(",");
  const [k, x, y] = [ks, xs, ys].map(Number);
  if (at !== tag || !k || !Number.isFinite(k) || !Number.isFinite(x) || !Number.isFinite(y)) {
    return undefined;
  }
  return { k: k!, x: x!, y: y! };
}

/** The company the builder stands on, as a number that changes when it is loaded again. */
const baseKeys = new WeakMap<object, number>();
let baseSeq = 0;
export function baseKeyOf(base: BaseCompany): number {
  const document = base.document;
  if (document === null) return 0;
  let key = baseKeys.get(document);
  if (key === undefined) {
    key = ++baseSeq;
    baseKeys.set(document, key);
  }
  return key;
}

export interface ChartPlace {
  /** Changes on every layout action that refits the chart. */
  fitKey: string;
  /** The view a return puts back, for the chart it was taken on. */
  saved: CanvasView | undefined;
  onViewChange: (view: CanvasView) => void;
  expanded: ReadonlySet<string>;
  onExpandedChange: (open: ReadonlySet<string>) => void;
}

/**
 * The branches of a tree the reader closed, held in the address (`closed`), for
 * a kit tree's `expanded` and `onExpandedChange`: every branch is open until
 * the reader closes it, and a return finds it as they left it.
 */
export function useClosedBranches(nodes: readonly TreeInput[]): {
  expanded: ReadonlySet<string>;
  onExpandedChange: (open: ReadonlySet<string>) => void;
  /** The closed branches, sorted, as one value a fit can key on. */
  closedKey: string;
} {
  const [closedParam, setClosed] = useParam("closed", "", "filter");
  const branches = useMemo(() => branchesOf(nodes), [nodes]);
  const closed = useMemo(() => closedOf(closedParam), [closedParam]);
  const expanded = useMemo(
    () => new Set(branches.filter((id) => !closed.has(id))),
    [branches, closed],
  );
  const latestBranches = useRef(branches);
  latestBranches.current = branches;
  const onExpandedChange = useCallback(
    (open: ReadonlySet<string>) => {
      setClosed(latestBranches.current.filter((id) => !open.has(id)).join(","));
    },
    [setClosed],
  );
  return { expanded, onExpandedChange, closedKey: [...closed].sort().join(",") };
}

/** A chart's place, over `nodes`, refitted whenever `layout` or its closed branches change. */
export function useChartPlace(nodes: readonly TreeInput[], layout: string): ChartPlace {
  const nav = useNavigator();
  const [zoomParam] = useParam("zoom", "", "filter");
  const { expanded, onExpandedChange, closedKey } = useClosedBranches(nodes);
  const fitKey = `${layout}|${closedKey}`;
  const tag = tagOf(fitKey);
  // READ ONCE, ON ARRIVAL: after that the view is the canvas's, and the address follows it.
  const [saved] = useState(() => readView(zoomParam, tag));

  const latestTag = useRef(tag);
  latestTag.current = tag;
  const timer = useRef(0);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const onViewChange = useCallback(
    (view: CanvasView) => {
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(
        () => nav.note({ zoom: writeView(view, latestTag.current) }),
        VIEW_SETTLE_MS,
      );
    },
    [nav],
  );
  return { fitKey, saved, onViewChange, expanded, onExpandedChange };
}
