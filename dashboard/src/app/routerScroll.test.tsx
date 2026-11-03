/**
 * What the router does to the page's scroll on a move.
 *
 * A REPLACE STAYS ON THE ENTRY, so the router leaves the scroll alone. It used
 * to "restore" the position the replace had just filed — after the screen's
 * own effects, for a dozen frames — and so undid any scroll a screen made in
 * answer to its own filter: a turn's span detail, opened under the waterfall
 * on a phone and scrolled into view, was scrolled straight back out of it.
 */

import { act, cleanup, render } from "@testing-library/react";
import { useLayoutEffect } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { Router, useNavigator, useParam } from "./router.tsx";

let nav: ReturnType<typeof useNavigator> | null = null;
let setSpan: (v: string) => void = () => {};

/** A screen that scrolls to 900 when its own `span=` filter opens something. */
function Screen() {
  nav = useNavigator();
  const [span, set] = useParam("span", "", "filter");
  setSpan = set;
  useLayoutEffect(() => {
    if (!span) return;
    const el = document.getElementById(SCREEN_SCROLL_ID)!;
    el.scrollTop = 900;
  }, [span]);
  return <p>{span || "nothing open"}</p>;
}

function mount() {
  const view = render(
    <div id={SCREEN_SCROLL_ID}>
      <Router>
        <Screen />
      </Router>
    </div>,
  );
  // jsdom lays nothing out, so its `scrollTop` is always 0: a plain value
  // stands in, which is all the router reads and writes.
  const el = document.getElementById(SCREEN_SCROLL_ID)!;
  let top = 0;
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    get: () => top,
    set: (v: number) => {
      top = v;
    },
  });
  return { view, el };
}

/** The browser's own frame scheduler, taken before a case counts the page's. */
const nextFrame = window.requestAnimationFrame.bind(window);

/** The frames the page has asked for that the browser has not run yet. */
const waiting = new Set<number>();

/**
 * Runs every frame the page has asked for, and every frame those ask for in
 * turn, until it asks for no more. THE ROUTER'S SCROLL WORK IS FRAMES — a
 * restore tries again a frame later until the position holds or it gives up
 * — so once none is waiting, nothing the router will ever do to the scroll is
 * still to come, however many frames it took. Frames run in the order they
 * were asked for, so the one this asks for runs after every frame the page
 * had asked for first.
 */
async function settleFrames() {
  while (waiting.size > 0) {
    await act(() => new Promise<void>((resolve) => nextFrame(() => resolve())));
  }
}

beforeEach(() => {
  history.replaceState(null, "", "#/live/turns/t1");
  vi.spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    const id = nextFrame((time) => {
      waiting.delete(id);
      callback(time);
    });
    waiting.add(id);
    return id;
  });
  const cancel = window.cancelAnimationFrame.bind(window);
  vi.spyOn(window, "cancelAnimationFrame").mockImplementation((id) => {
    waiting.delete(id);
    cancel(id);
  });
});
afterEach(() => {
  cleanup();
  nav = null;
  vi.restoreAllMocks();
  waiting.clear();
});

test("a filter's own scroll survives the replace that opened it", async () => {
  const { el } = mount();
  await settleFrames();
  el.scrollTop = 100;
  act(() => setSpan("t1|execute|1.r1"));
  await settleFrames();
  expect(location.hash).toContain("span=");
  expect(el.scrollTop, "the router scrolled the screen back to where it was").toBe(900);
});

test("a move to a new entry still starts at the top", async () => {
  const { el } = mount();
  await settleFrames();
  el.scrollTop = 400;
  act(() => nav!.to(["live", "turns", "t2"]));
  await settleFrames();
  expect(el.scrollTop).toBe(0);
});
