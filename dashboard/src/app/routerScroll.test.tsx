/**
 * What the router does to the page's scroll on a move.
 *
 * A REPLACE STAYS ON THE ENTRY, so the router leaves the scroll alone. It used
 * to "restore" the position the replace had just filed — after the screen's
 * own effects, for a dozen frames — and so undid any scroll a screen made in
 * answer to its own filter: a turn's span detail, opened under the waterfall
 * on a phone and scrolled into view, was scrolled straight back out of it.
 */

import { act, cleanup, render } from "~/test/inCase.ts";
import { useLayoutEffect } from "react";
import { afterEach, beforeEach, expect, test } from "vitest";

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

async function frames(n = 20) {
  for (let i = 0; i < n; i++) {
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => r(null)));
    });
  }
}

beforeEach(() => {
  history.replaceState(null, "", "#/live/turns/t1");
});
afterEach(() => {
  cleanup();
  nav = null;
});

test("a filter's own scroll survives the replace that opened it", async () => {
  const { el } = mount();
  await frames(2);
  el.scrollTop = 100;
  act(() => setSpan("t1|execute|1.r1"));
  await frames();
  expect(location.hash).toContain("span=");
  expect(el.scrollTop, "the router scrolled the screen back to where it was").toBe(900);
});

test("a move to a new entry still starts at the top", async () => {
  const { el } = mount();
  await frames(2);
  el.scrollTop = 400;
  act(() => nav!.to(["live", "turns", "t2"]));
  await frames();
  expect(el.scrollTop).toBe(0);
});
