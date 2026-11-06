/**
 * Where a chart of Structure stands, kept in the page's address so a Back, a
 * reload and a seat page's crumb put it back exactly: which branches the
 * reader closed (`closed`), and the zoom and pan they left it at (`zoom`).
 *
 * A SAVED VIEW IS PUT BACK ONLY ON THE CHART IT WAS TAKEN ON. It is tagged
 * with what the chart IS (the arrangement, the legend's key and the closed
 * branches), so a view of the teams is never laid over the reporting lines.
 * What REFITS it (Expand all, Collapse all, a fresh read of the company) is
 * left out of the tag: those count again from nothing on every mount, so a
 * Back or a crumb that compared them never found its view, and a refit writes
 * the view it lands on into the address anyway.
 *
 * A FRESH ARRIVAL OPENS WHERE IT CAN BE READ. With nothing in the address to
 * put back, a chart whose whole fit is below `CHART_LEGIBLE_ZOOM` opens on its
 * top ranks with the deeper branches closed, written into the address as a
 * reader's own closing is, so a return and a reload find it the same. It is
 * decided on the canvas's first report, in the layout effect before anything
 * is painted, and the canvas is drawn afresh on the answer, so it neither shows
 * the small chart nor eases from it.
 */

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { CanvasView, TreeInput } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { CHART_LEGIBLE_ZOOM } from "~/ui/canvasView.ts";
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

/** How many cards each rank of a forest holds with every branch open. */
export function rankWidths(nodes: readonly TreeInput[]): number[] {
  const widths: number[] = [];
  const walk = (list: readonly TreeInput[], depth: number) => {
    for (const node of list) {
      widths[depth] = (widths[depth] ?? 0) + 1;
      if (node.children?.length) walk(node.children, depth + 1);
    }
  };
  walk(nodes, 0);
  return widths;
}

/**
 * The deepest rank a chart can open showing and be read, or null where its
 * whole fit already can.
 *
 * ESTIMATED FROM THE FIT IT WOULD HAVE HAD. A chart this wide is fitted by its
 * widest rank, so showing a narrower one scales the fit by the ratio; the kit
 * does not lay out a chart it is not asked to draw. Rank 0 is the floor: on a
 * phone, where even three cards in a rank are too small, the top is what opens.
 */
export function openingDepth(
  widths: readonly number[],
  fit: number,
  legible: number,
): number | null {
  if (fit >= legible) return null;
  const widest = Math.max(...widths);
  for (let depth = widths.length - 2; depth > 0; depth--) {
    if ((fit * widest) / Math.max(...widths.slice(0, depth + 1)) >= legible) return depth;
  }
  return 0;
}

/**
 * The branches at `depth` and below, the ones closing them leaves `depth` the
 * last rank drawn, but for those on the way to `keep`: a node the address
 * selected stays in sight.
 */
export function branchesFrom(
  nodes: readonly TreeInput[],
  depth: number,
  keep: string | null,
): string[] {
  const out: string[] = [];
  const walk = (list: readonly TreeInput[], at: number): boolean => {
    let holds = false;
    for (const node of list) {
      const kids = node.children ?? [];
      const inside = walk(kids, at + 1);
      if (kids.length > 0 && at >= depth && !inside) out.push(node.id);
      holds ||= inside || node.id === keep;
    }
    return holds;
  };
  walk(nodes, 0);
  return out;
}

export interface ChartPlace {
  /** Changes on every layout action that refits the chart. */
  fitKey: string;
  /** The view a return puts back, for the chart it was taken on. */
  saved: CanvasView | undefined;
  onViewChange: (view: CanvasView) => void;
  expanded: ReadonlySet<string>;
  onExpandedChange: (open: ReadonlySet<string>) => void;
  /** Changes when the opening closed branches: the canvas is drawn afresh on it. */
  drawing: number;
}

/** What a fresh arrival's opening has to respect. */
export interface ChartOpening {
  /** The selected node, whose path stays open. */
  keep: string | null;
  /** Open whole whatever the fit: a node is being added, and its ghost is already placed. */
  whole: boolean;
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

/**
 * A chart's place, over `nodes`: its saved view tagged with `identity` and its
 * closed branches, and refitted whenever either changes or `refit` does.
 */
export function useChartPlace(
  nodes: readonly TreeInput[],
  identity: string,
  refit: string,
  opening: ChartOpening,
): ChartPlace {
  const nav = useNavigator();
  const [zoomParam] = useParam("zoom", "", "filter");
  const [closedParam] = useParam("closed", "", "filter");
  const { expanded, onExpandedChange, closedKey } = useClosedBranches(nodes);
  // A FRESH ARRIVAL IS ANSWERED ONCE, on the canvas's first report. The bare
  // address reached with the chart already up (the sidebar's row, pressed here)
  // is one too, and a canvas drawn afresh is what is sure to report.
  const bare = zoomParam === "" && closedParam === "";
  const arriving = useRef(bare);
  const wasBare = useRef(bare);
  const [drawing, setDrawing] = useState(0);
  // A READER OPENING THE LAST CLOSED BRANCH reaches it too, before the view has
  // reached the address, and that is theirs to keep.
  const own = useRef(false);
  useLayoutEffect(() => {
    if (bare && !wasBare.current && !own.current) {
      arriving.current = true;
      setDrawing((n) => n + 1);
    }
    wasBare.current = bare;
    own.current = false;
  });
  const onOwnChange = useCallback(
    (open: ReadonlySet<string>) => {
      own.current = true;
      onExpandedChange(open);
    },
    [onExpandedChange],
  );
  const now = useRef({ nodes, opening, onExpandedChange });
  now.current = { nodes, opening, onExpandedChange };
  const fitKey = `${identity}|${closedKey}|${refit}`;
  const tag = tagOf(`${identity}|${closedKey}`);
  // READ ONCE, ON ARRIVAL: after that the view is the canvas's, and the address follows it.
  const [saved] = useState(() => readView(zoomParam, tag));

  const latestTag = useRef(tag);
  latestTag.current = tag;
  const timer = useRef(0);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const onViewChange = useCallback(
    (view: CanvasView) => {
      if (arriving.current) {
        arriving.current = false;
        const { nodes: forest, opening: asked, onExpandedChange: open } = now.current;
        const depth = asked.whole
          ? null
          : openingDepth(rankWidths(forest), view.k, CHART_LEGIBLE_ZOOM);
        const closing = new Set(depth === null ? [] : branchesFrom(forest, depth, asked.keep));
        if (closing.size > 0) {
          open(new Set(branchesOf(forest).filter((id) => !closing.has(id))));
          setDrawing((n) => n + 1);
          return;
        }
      }
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(
        () => nav.note({ zoom: writeView(view, latestTag.current) }),
        VIEW_SETTLE_MS,
      );
    },
    [nav],
  );
  return { fitKey, saved, onViewChange, expanded, onExpandedChange: onOwnChange, drawing };
}
