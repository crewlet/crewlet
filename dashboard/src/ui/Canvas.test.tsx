/**
 * The canvas: who owns a key, a wheel and a finger.
 *
 * The arithmetic is `viewport.test.ts`. What is asserted here is the wiring
 * that jsdom can observe: which element a key belongs to, when the wheel is
 * the page's rather than the chart's, that a finger does not trap the page
 * until the canvas is asked for it, and that a data push never moves the view.
 * jsdom has no layout, so the viewport's size is fed through a controllable
 * ResizeObserver and every rectangle starts at the origin.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createRef, useState } from "react";
import { createPortal } from "react-dom";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Canvas, useCanvasOverlay, type CanvasHandle } from "./Canvas.tsx";
import { ZOOM_STEP, fit, type Rect, type View } from "./viewport.ts";

type Callback = (entries: { contentRect: { width: number; height: number } }[]) => void;

class FakeResizeObserver {
  static last: FakeResizeObserver | null = null;
  constructor(private callback: Callback) {
    FakeResizeObserver.last = this;
  }
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
  resize(width: number, height: number) {
    act(() => this.callback([{ contentRect: { width, height } }]));
  }
}

// Assigned rather than stubbed: the setup file defines the property writable
// but not configurable, so it can be replaced and must be put back by hand.
const realResizeObserver = globalThis.ResizeObserver;
beforeEach(() => {
  globalThis.ResizeObserver = FakeResizeObserver as unknown as typeof ResizeObserver;
});
afterEach(() => {
  cleanup();
  globalThis.ResizeObserver = realResizeObserver;
});

const SIZE = { width: 800, height: 600 };
const SMALL: Rect = { x: 0, y: 0, width: 400, height: 300 };

function Harness({
  content,
  handle,
  onItem,
}: {
  content: Rect | null;
  handle?: React.Ref<CanvasHandle>;
  onItem?: () => void;
}) {
  return (
    <Canvas label="Organization chart" content={content} ref={handle}>
      <div role="tree" aria-label="Units">
        <div role="treeitem" aria-selected={false} tabIndex={-1} onClick={onItem}>
          Chief Executive
        </div>
      </div>
      <button>card action</button>
    </Canvas>
  );
}

function world(container: HTMLElement): HTMLElement {
  return container.querySelector(".canvas-world") as HTMLElement;
}

function viewOf(container: HTMLElement): View {
  const match = /translate\((-?[\d.e+-]+)px, (-?[\d.e+-]+)px\) scale\(([\d.e+-]+)\)/.exec(
    world(container).style.transform,
  );
  if (!match)
    throw new Error(`no transform on the world layer: ${world(container).style.transform}`);
  return { x: Number(match[1]), y: Number(match[2]), k: Number(match[3]) };
}

function viewport(): HTMLElement {
  return screen.getByRole("group", { name: "Organization chart" });
}

function mount(content: Rect | null = SMALL, onItem?: () => void) {
  const handle = createRef<CanvasHandle>();
  const utils = render(<Harness content={content} handle={handle} onItem={onItem} />);
  FakeResizeObserver.last!.resize(SIZE.width, SIZE.height);
  return { ...utils, handle };
}

test("the viewport is a focusable, labelled canvas that says what its keys do", () => {
  mount();
  const el = viewport();
  expect(el.tabIndex).toBe(0);
  expect(el.getAttribute("aria-roledescription")).toBe("canvas");
  const help = document.getElementById(el.getAttribute("aria-describedby")!);
  expect(help?.textContent).toMatch(/plus and minus zoom/);
});

test("nothing is shown or fitted until both the viewport and the content are measured", () => {
  const { container, rerender } = render(<Harness content={null} />);
  const canvas = container.querySelector(".canvas")!;
  expect(canvas.getAttribute("data-ready")).toBe("false");

  FakeResizeObserver.last!.resize(SIZE.width, SIZE.height);
  expect(canvas.getAttribute("data-ready")).toBe("false");

  const content = { x: 0, y: 0, width: 2000, height: 900 };
  rerender(<Harness content={content} />);
  expect(canvas.getAttribute("data-ready")).toBe("true");
  expect(viewOf(container)).toEqual(fit(content, SIZE));
});

test("a data push that reshapes the content never refits the operator's view", () => {
  const { container, rerender, handle } = mount();
  fireEvent.keyDown(viewport(), { key: "+" });
  const zoomed = viewOf(container);

  const grown = { x: 0, y: 0, width: 900, height: 700 };
  rerender(<Harness content={grown} handle={handle} />);
  expect(viewOf(container)).toEqual(zoomed);

  // Only a request fits again.
  act(() => handle.current!.fit());
  expect(viewOf(container)).toEqual(fit(grown, SIZE));
});

test("zoom keys act only while the viewport itself holds focus", () => {
  const { container } = mount();
  const start = viewOf(container);

  // Each key checked on its own: "+" then "0" would land back on the fit and
  // hide a zoom that should never have happened.
  for (const key of ["+", "-", "ArrowLeft"]) {
    fireEvent.keyDown(screen.getByRole("treeitem"), { key });
    expect(viewOf(container)).toEqual(start);
  }

  fireEvent.keyDown(viewport(), { key: "+" });
  expect(viewOf(container).k).toBeCloseTo(start.k * ZOOM_STEP);
  fireEvent.keyDown(viewport(), { key: "=", ctrlKey: true });
  expect(viewOf(container).k).toBeCloseTo(start.k * ZOOM_STEP * ZOOM_STEP);
  fireEvent.keyDown(viewport(), { key: "-", metaKey: true });
  expect(viewOf(container).k).toBeCloseTo(start.k * ZOOM_STEP);
  fireEvent.keyDown(viewport(), { key: "0" });
  expect(viewOf(container)).toEqual(fit(SMALL, SIZE));

  // A plain `=` is not a zoom key: it belongs to whatever else wants it.
  expect(fireEvent.keyDown(viewport(), { key: "=" })).toBe(true);
});

test("the arrow keys pan a focused viewport", () => {
  const { container } = mount();
  const start = viewOf(container);
  fireEvent.keyDown(viewport(), { key: "ArrowRight" });
  expect(viewOf(container).x).toBeLessThan(start.x);
});

test("a plain wheel is the page's until the canvas holds focus; Ctrl or Command zooms anywhere", () => {
  const { container } = mount();
  const start = viewOf(container);

  // Not focused: the page scrolls, and the chart does not move.
  expect(fireEvent.wheel(viewport(), { deltaY: 80 })).toBe(true);
  expect(viewOf(container)).toEqual(start);

  expect(fireEvent.wheel(viewport(), { deltaY: -100, ctrlKey: true })).toBe(false);
  const zoomed = viewOf(container);
  expect(zoomed.k).toBeCloseTo(start.k * ZOOM_STEP);

  viewport().focus();
  expect(fireEvent.wheel(viewport(), { deltaY: 80 })).toBe(false);
  expect(viewOf(container).y).not.toBe(zoomed.y);
});

function touch(
  type: "pointerDown" | "pointerMove" | "pointerUp",
  id: number,
  x: number,
  y: number,
) {
  const init = { pointerId: id, pointerType: "touch", clientX: x, clientY: y, button: 0 };
  if (type === "pointerDown") fireEvent.pointerDown(viewport(), init);
  else fireEvent[type](window, init);
}

test("one finger pans only after a tap activates the canvas, and Done hands it back", () => {
  const { container } = mount();
  const start = viewOf(container);

  // A swipe on an inactive canvas is the page scrolling, not a pan.
  touch("pointerDown", 1, 100, 100);
  touch("pointerMove", 1, 220, 160);
  touch("pointerUp", 1, 220, 160);
  expect(viewOf(container)).toEqual(start);
  expect(screen.queryByRole("button", { name: "Done" })).toBeNull();

  // A tap activates it.
  touch("pointerDown", 2, 100, 100);
  touch("pointerUp", 2, 101, 100);
  expect(screen.getByRole("button", { name: "Done" })).toBeDefined();
  expect(container.querySelector(".canvas")!.getAttribute("data-touch-active")).toBe("true");

  touch("pointerDown", 3, 100, 100);
  touch("pointerMove", 3, 160, 130);
  touch("pointerUp", 3, 160, 130);
  expect(viewOf(container).x).toBeCloseTo(start.x + 60);
  expect(viewOf(container).y).toBeCloseTo(start.y + 30);

  fireEvent.click(screen.getByRole("button", { name: "Done" }));
  expect(screen.queryByRole("button", { name: "Done" })).toBeNull();
});

test("two fingers pinch toward their midpoint without activating the canvas first", () => {
  const { container } = mount();
  expect(viewOf(container).k).toBe(1);
  touch("pointerDown", 1, 300, 300);
  touch("pointerDown", 2, 500, 300);
  touch("pointerMove", 2, 700, 300);
  // The spread went from 200 to 400 pixels.
  expect(viewOf(container).k).toBeCloseTo(2);
  touch("pointerUp", 1, 300, 300);
  touch("pointerUp", 2, 700, 300);
});

test("a mouse drag pans, and the click it ends with does not reach the item", async () => {
  const onItem = vi.fn();
  const { container } = mount(SMALL, onItem);
  const start = viewOf(container);
  const item = screen.getByRole("treeitem");
  const mouse = { pointerId: 9, pointerType: "mouse", button: 0 };

  fireEvent.pointerDown(item, { ...mouse, clientX: 10, clientY: 10 });
  fireEvent.pointerMove(window, { ...mouse, clientX: 60, clientY: 10 });
  fireEvent.pointerUp(window, { ...mouse, clientX: 60, clientY: 10 });
  fireEvent.click(item);
  expect(viewOf(container).x).toBeCloseTo(start.x + 50);
  expect(onItem).not.toHaveBeenCalled();

  // A later, real click is not eaten.
  await new Promise((resolve) => setTimeout(resolve, 0));
  fireEvent.click(item);
  expect(onItem).toHaveBeenCalledTimes(1);
});

test("the pointer is captured only once a press becomes a drag, so a plain click reaches its item", () => {
  mount();
  // jsdom has no pointer capture: this is the browser's, feature-detected.
  const captured = vi.fn();
  (viewport() as HTMLElement & { setPointerCapture: (id: number) => void }).setPointerCapture =
    captured;
  const mouse = { pointerId: 12, pointerType: "mouse", button: 0 };
  const item = screen.getByRole("treeitem");

  fireEvent.pointerDown(item, { ...mouse, clientX: 10, clientY: 10 });
  fireEvent.pointerMove(window, { ...mouse, clientX: 12, clientY: 11 });
  fireEvent.pointerUp(window, { ...mouse, clientX: 12, clientY: 11 });
  expect(captured).not.toHaveBeenCalled();

  fireEvent.pointerDown(item, { ...mouse, clientX: 10, clientY: 10 });
  fireEvent.pointerMove(window, { ...mouse, clientX: 80, clientY: 10 });
  expect(captured).toHaveBeenCalledWith(12);
  fireEvent.pointerUp(window, { ...mouse, clientX: 80, clientY: 10 });
});

test("a press on a control inside the canvas never starts a pan", () => {
  const { container } = mount();
  const start = viewOf(container);
  const mouse = { pointerId: 4, pointerType: "mouse", button: 0 };
  fireEvent.pointerDown(screen.getByRole("button", { name: "card action" }), {
    ...mouse,
    clientX: 0,
    clientY: 0,
  });
  fireEvent.pointerMove(window, { ...mouse, clientX: 90, clientY: 90 });
  fireEvent.pointerUp(window, { ...mouse, clientX: 90, clientY: 90 });
  expect(viewOf(container)).toEqual(start);
});

test("a browser scrolling the viewport to show a focused item becomes a pan", () => {
  const { container } = mount();
  const start = viewOf(container);
  const el = viewport();
  let left = 30;
  Object.defineProperty(el, "scrollLeft", {
    configurable: true,
    get: () => left,
    set: (v: number) => {
      left = v;
    },
  });
  fireEvent.scroll(el);
  expect(left).toBe(0);
  expect(viewOf(container).x).toBeCloseTo(start.x - 30);
});

test("a request eases and a gesture does not", () => {
  const { container, handle } = mount();
  const canvas = container.querySelector(".canvas")!;
  act(() => handle.current!.zoomBy(ZOOM_STEP));
  expect(canvas.getAttribute("data-animate")).toBe("true");
  fireEvent.wheel(viewport(), { deltaY: 20, ctrlKey: true });
  expect(canvas.getAttribute("data-animate")).toBe("false");
});

test("a data push while a requested move eases does not cut the easing short", () => {
  const { container, handle, rerender } = mount();
  const canvas = container.querySelector(".canvas")!;
  act(() => handle.current!.fit());
  act(() => handle.current!.zoomBy(ZOOM_STEP));
  expect(canvas.getAttribute("data-animate")).toBe("true");
  // The same bounds as a new object: what every push of a live chart sends.
  rerender(<Harness content={{ ...SMALL }} handle={handle} />);
  expect(canvas.getAttribute("data-animate")).toBe("true");
});

test("items inside reach an overlay layer that the zoom does not transform", () => {
  function Menu() {
    const layer = useCanvasOverlay();
    const [open] = useState(true);
    return layer && open
      ? createPortal(<div role="menu" aria-label="Seat actions" />, layer)
      : null;
  }
  const { container } = render(
    <Canvas label="Organization chart" content={SMALL}>
      <Menu />
    </Canvas>,
  );
  const menu = screen.getByRole("menu", { name: "Seat actions" });
  expect(menu.parentElement?.classList.contains("canvas-overlay")).toBe(true);
  expect(world(container).contains(menu)).toBe(false);
});
