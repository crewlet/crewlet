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

test("a row keeps its call line with no call out, and its head line whatever it holds", () => {
  expect(rule(".live-call")).toMatch(/min-height: 1lh;/);
  expect(rule(".live-head")).toMatch(/min-height: var\(--live-head-line\);/);
  expect(rule(".live-head")).not.toMatch(/align-items: baseline/);
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
