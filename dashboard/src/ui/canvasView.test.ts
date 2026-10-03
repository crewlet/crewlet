/**
 * The view a screen chooses for a tree canvas, as arithmetic: the floor it
 * holds, where it puts the node it is anchored on, and the gesture it makes to
 * get there. The two charts that hold it are exercised end to end in their own
 * suites (`OrgChart.test.tsx`, `CanvasView.test.tsx`); this is the rule itself,
 * checked against the kit's own zoom arithmetic rather than a copy of it.
 */

import {
  CANVAS_FIT_PADDING,
  CANVAS_REVEAL_PADDING,
  CANVAS_ZOOM_LIMITS,
  wheelZoomFactor,
  zoomAt,
  type CanvasPoint,
  type CanvasRect,
  type CanvasView,
} from "@crewlethq/ui";
import { afterEach, describe, expect, test } from "vitest";

import {
  LEGIBLE_ZOOM,
  canvasControls,
  holdLegible,
  largestRoot,
  type CanvasControls,
} from "./canvasView.ts";

/** Controls over a view that exists only as numbers, recording the gestures made. */
function controlsOver(
  view: CanvasView,
  boxes: Record<string, CanvasRect>,
  size = { width: 1142, height: 747 },
  bounds: CanvasRect | null = null,
): CanvasControls & { made: { factor: number; at: CanvasPoint }[] } {
  const made: { factor: number; at: CanvasPoint }[] = [];
  return {
    made,
    view: () => `translate(${view.x}px, ${view.y}px) scale(${view.k})`,
    current: () => view,
    size: () => size,
    place: (id) => boxes[id] ?? null,
    bounds: () => bounds,
    fit: () => {},
    zoomAbout: (factor, at) => made.push({ factor, at }),
  };
}

/** What the kit's canvas draws after the gesture: its own zoom, about the point. */
const after = (view: CanvasView, made: { factor: number; at: CanvasPoint }) =>
  zoomAt(view, made.factor, made.at, CANVAS_ZOOM_LIMITS);

// Nimbus at 1440: the builder's canvas is 1142px across and fitted the chart
// at 57%, its root 141px left of the canvas's centre.
const FITTED: CanvasView = { x: 24, y: 190, k: 0.57 };
const ROOT: CanvasRect = { x: 700, y: 0, width: 150, height: 48 };
const LEAF: CanvasRect = { x: 1500, y: 540, width: 190, height: 48 };

describe("holding a view at the floor", () => {
  // EXACTLY THE FLOOR. Pressing Zoom in until the floor was passed came to
  // rest at 0.57 × 1.2³ ≈ 0.99 — "99%" at 1440 and "101%" at 1280 — beside
  // the other chart's 100%.
  test("a fit below the floor is drawn at exactly the floor, in one gesture", () => {
    const c = controlsOver(FITTED, { root: ROOT });
    holdLegible(c, "root");
    expect(c.made).toHaveLength(1);
    expect(after(FITTED, c.made[0]!).k).toBeCloseTo(LEGIBLE_ZOOM, 12);
  });

  // THE ROOT IS WHERE A CHART IS READ FROM. Zoomed about the canvas's centre,
  // Nimbus's root sat off centre and a sliver of one card stood at one edge.
  // Down the canvas it goes where the kit's own fit puts a chart: centred when
  // the whole chart is shorter than the room, read from the top when not.
  test("the root is centred across, and a chart shorter than the canvas is centred down it", () => {
    const chart = { x: 0, y: 0, width: 2000, height: 588 };
    const c = controlsOver(FITTED, { root: ROOT }, undefined, chart);
    expect(holdLegible(c, "root")).toBe("anchored");
    const view = after(FITTED, c.made[0]!);
    expect(view.x + (ROOT.x + ROOT.width / 2) * view.k).toBeCloseTo(1142 / 2, 9);
    const top = view.y + chart.y * view.k;
    const bottom = view.y + (chart.y + chart.height) * view.k;
    expect(top).toBeCloseTo(747 - bottom, 9);
  });

  test("a chart taller than the canvas is read from its top edge", () => {
    const chart = { x: 0, y: 0, width: 2000, height: 1400 };
    const c = controlsOver(FITTED, { root: ROOT }, undefined, chart);
    holdLegible(c, "root");
    const view = after(FITTED, c.made[0]!);
    expect(view.y + chart.y * view.k).toBeCloseTo(CANVAS_FIT_PADDING, 9);
  });

  // A NODE THE READER CHOSE is centred across, and the chart stays where the
  // fit puts it down the canvas — centring a leaf both ways cut the ranks above
  // it off the top of a chart that had room to show them all.
  test("a node the reader chose is centred across, the chart where the fit puts it", () => {
    const chart = { x: 0, y: 0, width: 2000, height: 588 };
    const c = controlsOver(FITTED, { leaf: LEAF }, undefined, chart);
    expect(holdLegible(c, "leaf")).toBe("anchored");
    const view = after(FITTED, c.made[0]!);
    expect(view.x + (LEAF.x + LEAF.width / 2) * view.k).toBeCloseTo(1142 / 2, 9);
    expect(view.y + chart.y * view.k).toBeCloseTo(747 - (view.y + chart.height * view.k), 9);
  });

  // AND ONE BELOW THE FOLD of a chart taller than the canvas is pulled just far
  // enough to be shown, with the kit's reveal padding under it.
  test("a chosen node below the fold of a tall chart is pulled into view, and no further", () => {
    const deep = { x: 1500, y: 1200, width: 190, height: 48 };
    const chart = { x: 0, y: 0, width: 2000, height: 1400 };
    const c = controlsOver(FITTED, { deep }, undefined, chart);
    holdLegible(c, "deep");
    const view = after(FITTED, c.made[0]!);
    expect(view.y + (deep.y + deep.height) * view.k).toBeCloseTo(747 - CANVAS_REVEAL_PADDING, 9);
  });

  // A NODE WITH NO CARD (inside a closed parent) cannot be placed, so the view
  // is zoomed about the centre and the caller reveals it.
  test("an anchor with no card is zoomed about the centre and left to a reveal", () => {
    const c = controlsOver(FITTED, {});
    expect(holdLegible(c, "hidden")).toBe("centred");
    expect(c.made[0]!.at).toEqual({ x: 1142 / 2, y: 747 / 2 });
    expect(after(FITTED, c.made[0]!).k).toBeCloseTo(LEGIBLE_ZOOM, 12);
  });

  test("a fit at or above the floor is the view, and nothing moves", () => {
    const c = controlsOver({ ...FITTED, k: 0.92 }, { root: ROOT });
    expect(holdLegible(c, "root")).toBe("fitted");
    expect(c.made).toEqual([]);
  });
});

describe("the gesture", () => {
  let host: HTMLElement | null = null;
  afterEach(() => host?.remove());

  // THE READER'S OWN ZOOM: ctrl and the wheel over a point, with the delta the
  // kit's published arithmetic turns into exactly the factor asked for.
  test("a zoom is ctrl and the wheel over the point, by exactly the factor", () => {
    host = document.createElement("div");
    host.innerHTML = `<div role="group" aria-roledescription="canvas"><div></div></div>`;
    document.body.append(host);
    const seen: WheelEvent[] = [];
    host.firstElementChild!.addEventListener("wheel", (e) => seen.push(e as WheelEvent));
    canvasControls(host).zoomAbout(1.49, { x: 300, y: 40 });
    expect(seen).toHaveLength(1);
    const wheel = seen[0]!;
    expect(wheel.ctrlKey).toBe(true);
    expect(wheel.deltaMode).toBe(0);
    expect(wheelZoomFactor(wheel.deltaY)).toBeCloseTo(1.49, 12);
    expect([wheel.clientX, wheel.clientY]).toEqual([300, 40]);
  });
});

// THE ROOT A FOREST IS READ FROM is the one the organization hangs from, not
// whoever the data listed first — a person with no manager, at the far left.
test("the root of a forest is the one with the most nodes under it", () => {
  expect(
    largestRoot([
      { id: "founder", label: "Founder" },
      {
        id: "ceo",
        label: "CEO",
        children: [
          { id: "cto", label: "CTO" },
          { id: "pm", label: "PM" },
        ],
      },
    ]),
  ).toBe("ceo");
  expect(largestRoot([])).toBeNull();
});
