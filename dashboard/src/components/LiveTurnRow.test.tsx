/**
 * What a running turn's row promises beyond what it says: it is one size whatever
 * its turn is doing, and nothing beside the elapsed time can push it out of a card.
 * The row's words are held by Home's and Live's own suites, which draw it in place.
 */

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { LiveRoom } from "./LiveTurnRow.tsx";

const SHEET = readFileSync(
  join(dirname(fileURLToPath(import.meta.url)), "../styles/screens.css"),
  "utf8",
);

/** The first rule for `selector` in the stylesheet, with its braces. */
function rule(selector: string): string {
  const at = SHEET.indexOf(`\n${selector} {`);
  return at < 0 ? "" : SHEET.slice(at, SHEET.indexOf("}", at));
}

afterEach(cleanup);

/*
 * THE ROW'S STILLNESS IS ITS STYLESHEET'S, and a browser measures it: 30 scripted
 * updates move no row, stepper or elapsed time. These hold the declarations that
 * measurement stands on. Mutation: give the name `flex: none`, or drop any one.
 */
test("the elapsed time has a slot of its own that the name and the state give way to", () => {
  expect(rule(".live-elapsed")).toMatch(/flex: none;/);
  expect(rule(".live-elapsed")).toMatch(/inline-size: 8ch;/);
  expect(rule(".live-elapsed")).toMatch(/text-align: end;/);
  expect(rule(".live-elapsed")).toMatch(/font-variant-numeric: tabular-nums;/);
  for (const giving of [".live-name", ".live-state", ".live-doing"]) {
    expect(rule(giving), giving).toMatch(/min-width: 0;/);
    expect(rule(giving), giving).not.toMatch(/flex: none;/);
  }
});

/*
 * A LONG NAME LEFT THE STATE NO ROOM: the name kept its whole width, the state's box
 * went to zero and its unshrinking tag and count painted over the time. The parts
 * that give way must share one flex line. Mutation: make .live-state a flex box again.
 */
test("in a wide row the name and what the seat is doing share one line, and the rest stays whole", () => {
  expect(rule(".live-state")).toMatch(/display: contents;/);
  expect(rule(".live-name")).toMatch(/flex: 0 1 auto;/);
  expect(rule(".live-doing")).toMatch(/flex: 0 1 auto;/);
  expect(rule(".live-quiet")).toMatch(/flex: none;/);
  expect(rule(".live-waiting")).toMatch(/flex: none;/);
  expect(rule(".live-elapsed")).toMatch(/margin-inline-start: auto;/);
  // The narrow row still puts the state on a line of its own.
  expect(SHEET).toMatch(
    /@container live-row \(width < 30rem\) \{[^@]*?\.live-state \{\s*display: flex;\s*flex: 0 0 100%;/,
  );
});

test("a row keeps its call line with no call out, and its head line whatever it holds", () => {
  expect(rule(".live-call")).toMatch(/min-height: 1lh;/);
  expect(rule(".live-head")).toMatch(/min-height: var\(--live-head-line\);/);
  expect(rule(".live-head")).not.toMatch(/align-items: baseline/);
});

/*
 * The card clips, so an outset ring lost its sides: every row flush inside a card
 * shares one inset ring (components.css). Mutation: drop a row from the list, or
 * give one a box-shadow ring of its own again.
 */
test("a focused row inside a card rings inside its edge", () => {
  const shared = readFileSync(
    join(dirname(fileURLToPath(import.meta.url)), "../styles/components.css"),
    "utf8",
  );
  const at = shared.indexOf(".list-row.clickable:focus-visible,");
  expect(at).toBeGreaterThan(-1);
  const head = shared.slice(at, shared.indexOf("{", at));
  for (const row of [
    ".list-row.clickable",
    "a.list-row",
    ".feed-row",
    ".project-row",
    "a.live-row",
  ]) {
    expect(head, row).toContain(`${row}:focus-visible`);
  }
  const ring = shared.slice(shared.indexOf("{", at), shared.indexOf("}", at));
  expect(ring).toMatch(/outline: 2px solid var\(--color-focus\);/);
  expect(ring).toMatch(/outline-offset: var\(--size-focus-ring-inset-offset\);/);
  for (const row of [".project-row", "a.live-row", ".feed-row", ".list-row"]) {
    expect(SHEET, row).not.toMatch(new RegExp(`\\n${row.replace(".", "\\.")}:focus-visible \\{`));
  }
});

test("a narrow row is two lines at its own width, not the window's", () => {
  expect(rule(".live-body")).toMatch(/container: live-row \/ inline-size;/);
  expect(SHEET).toMatch(
    /@container live-row \(width < 30rem\) \{\s*\.live-head \{\s*flex-wrap: wrap;/,
  );
});

/** A stand-in that lets a test report a size to the room's one observer. */
class FakeObserver {
  static last: FakeObserver | null = null;
  callback: ResizeObserverCallback;
  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
    FakeObserver.last = this;
  }
  observe() {}
  unobserve() {}
  disconnect() {}
  report(width: number, height: number) {
    act(() =>
      this.callback(
        [{ contentRect: { width, height } } as ResizeObserverEntry],
        this as unknown as ResizeObserver,
      ),
    );
  }
}

/*
 * A TURN THAT FINISHES LEAVES ITS ROOM. Measured on Home, every card under this one
 * moved 48 px as a row came and went. Mutation: follow the body's height down, or
 * keep the old height across a new width, where a row's own height is different.
 */
test("the room holds its tallest height for the visit, and a new width starts again", () => {
  const real = globalThis.ResizeObserver;
  globalThis.ResizeObserver = FakeObserver as unknown as typeof ResizeObserver;
  const { container } = render(
    <LiveRoom>
      <ul className="live-list" />
    </LiveRoom>,
  );
  const room = container.querySelector<HTMLElement>(".live-room")!;
  const width = container.querySelector("ul")!.parentElement!.getBoundingClientRect().width;
  const observer = FakeObserver.last!;

  observer.report(width, 240);
  expect(room.style.minBlockSize).toBe("240px");
  observer.report(width, 120);
  expect(room.style.minBlockSize).toBe("240px");
  observer.report(width, 360);
  expect(room.style.minBlockSize).toBe("360px");
  observer.report(width + 200, 300);
  expect(room.style.minBlockSize).toBe("300px");
  globalThis.ResizeObserver = real;
});
