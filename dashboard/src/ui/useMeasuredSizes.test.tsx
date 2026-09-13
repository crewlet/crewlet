/**
 * Measured sizes: one observer, real changes only, nothing before measurement,
 * and the acted-on node kept still.
 *
 * jsdom has no layout and its ResizeObserver never fires, so the observer is
 * replaced by one this suite drives: it reports exactly the sizes a browser
 * would, and counts how many observers were made.
 */

import { act, cleanup, render, renderHook } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useLayoutAnchor, useMeasuredSizes } from "./useMeasuredSizes.ts";

class FakeResizeObserver {
  static made: FakeResizeObserver[] = [];
  observed = new Set<Element>();
  constructor(private callback: ResizeObserverCallback) {
    FakeResizeObserver.made.push(this);
  }
  observe(el: Element) {
    this.observed.add(el);
  }
  unobserve(el: Element) {
    this.observed.delete(el);
  }
  disconnect() {
    this.observed.clear();
  }
  report(sizes: [Element, number, number][]) {
    act(() => this.deliver(sizes));
  }
  /** As the browser delivers it: outside React's act, between layout and paint. */
  deliver(sizes: [Element, number, number][]) {
    const entries = sizes.map(([target, inlineSize, blockSize]) => ({
      target,
      borderBoxSize: [{ inlineSize, blockSize }],
    })) as unknown as ResizeObserverEntry[];
    this.callback(entries, this as unknown as ResizeObserver);
  }
}

const realResizeObserver = globalThis.ResizeObserver;
beforeEach(() => {
  FakeResizeObserver.made = [];
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
});
afterEach(() => {
  cleanup();
  globalThis.ResizeObserver = realResizeObserver;
});

function Cards({
  ids,
  onRender,
}: {
  ids: string[];
  onRender: (m: ReturnType<typeof useMeasuredSizes>) => void;
}) {
  const measured = useMeasuredSizes();
  onRender(measured);
  return (
    <>
      {ids.map((id) => (
        <div key={id} data-id={id} ref={measured.measure(id)} />
      ))}
    </>
  );
}

function card(container: HTMLElement, id: string): Element {
  return container.querySelector(`[data-id="${id}"]`)!;
}

test("every card shares one observer, and nothing counts as measured until all of them are", () => {
  let latest!: ReturnType<typeof useMeasuredSizes>;
  const { container } = render(<Cards ids={["a", "b", "c"]} onRender={(m) => (latest = m)} />);
  expect(FakeResizeObserver.made).toHaveLength(1);
  const observer = FakeResizeObserver.made[0]!;
  expect(observer.observed.size).toBe(3);
  expect(latest.measured(["a", "b", "c"])).toBe(false);

  observer.report([
    [card(container, "a"), 240, 80],
    [card(container, "b"), 240, 132],
  ]);
  expect(latest.measured(["a", "b", "c"])).toBe(false);
  expect(latest.measured(["a", "b"])).toBe(true);

  observer.report([[card(container, "c"), 240, 60]]);
  expect(latest.measured(["a", "b", "c"])).toBe(true);
  expect(latest.sizes.get("b")).toEqual({ width: 240, height: 132 });
});

test("a size change is applied synchronously, so a layout never paints a stale frame", () => {
  let latest!: ReturnType<typeof useMeasuredSizes>;
  const { container } = render(<Cards ids={["a"]} onRender={(m) => (latest = m)} />);
  const observer = FakeResizeObserver.made[0]!;
  // Delivered outside act, as a browser does. An update React merely
  // scheduled would not have rendered by the next line; one flushed
  // synchronously has, which is what "before paint" means here.
  const wasActEnvironment = (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean })
    .IS_REACT_ACT_ENVIRONMENT;
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = false;
  try {
    observer.deliver([[card(container, "a"), 240, 80]]);
    expect(latest.sizes.get("a")).toEqual({ width: 240, height: 80 });
  } finally {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT =
      wasActEnvironment;
  }
});

test("sub-pixel re-measurement is not a change and does not re-render", () => {
  const renders = vi.fn();
  const { container } = render(<Cards ids={["a"]} onRender={renders} />);
  const observer = FakeResizeObserver.made[0]!;
  observer.report([[card(container, "a"), 240, 80]]);
  const before = renders.mock.calls.length;
  const sizes = renders.mock.calls.at(-1)![0].sizes;

  observer.report([[card(container, "a"), 240.3, 80.2]]);
  expect(renders.mock.calls.length).toBe(before);
  expect(renders.mock.calls.at(-1)![0].sizes).toBe(sizes);

  observer.report([[card(container, "a"), 240, 96]]);
  expect(renders.mock.calls.at(-1)![0].sizes.get("a")).toEqual({ width: 240, height: 96 });
});

test("the ref callback for an id is stable, and an unmounted card stops being observed", () => {
  const seen: ((el: HTMLElement | null) => void)[] = [];
  function Toggle() {
    const [ids, setIds] = useState(["a", "b"]);
    const measured = useMeasuredSizes();
    seen.push(measured.measure("a"));
    return (
      <>
        <button onClick={() => setIds(["a"])}>drop b</button>
        {ids.map((id) => (
          <div key={id} ref={measured.measure(id)} />
        ))}
      </>
    );
  }
  const { getByRole } = render(<Toggle />);
  const observer = FakeResizeObserver.made[0]!;
  expect(observer.observed.size).toBe(2);
  act(() => getByRole("button").click());
  expect(observer.observed.size).toBe(1);
  expect(new Set(seen).size).toBe(1);
});

test("the observer is disconnected when the hook's owner unmounts", () => {
  const { unmount } = render(<Cards ids={["a"]} onRender={() => {}} />);
  const observer = FakeResizeObserver.made[0]!;
  unmount();
  expect(observer.observed.size).toBe(0);
});

test("the anchored node's move is handed to keep, before paint, and nothing else is", () => {
  const keep = vi.fn();
  const first = new Map([
    ["unit:Engineering", { x: 100, y: 50 }],
    ["seat:cto", { x: 0, y: 0 }],
  ]);
  const { rerender } = renderHook(
    ({ positions, anchor }) => useLayoutAnchor(positions, anchor, keep),
    {
      initialProps: {
        positions: first as Map<string, { x: number; y: number }> | null,
        anchor: "unit:Engineering" as string | null,
      },
    },
  );
  // The first layout has nothing to be kept relative to.
  expect(keep).not.toHaveBeenCalled();

  const moved = new Map([
    ["unit:Engineering", { x: 180, y: 50 }],
    ["seat:cto", { x: 0, y: 0 }],
  ]);
  rerender({ positions: moved, anchor: "unit:Engineering" });
  expect(keep).toHaveBeenCalledWith({ x: 100, y: 50 }, { x: 180, y: 50 });

  // A new layout that leaves the anchor where it was asks for nothing.
  keep.mockClear();
  rerender({ positions: new Map(moved), anchor: "unit:Engineering" });
  expect(keep).not.toHaveBeenCalled();

  // Neither does changing which node is anchored, on its own.
  rerender({ positions: moved, anchor: "seat:cto" });
  expect(keep).not.toHaveBeenCalled();

  // Nor a node that did not exist before.
  rerender({ positions: new Map([...moved, ["seat:new", { x: 9, y: 9 }]]), anchor: "seat:new" });
  expect(keep).not.toHaveBeenCalled();
});
