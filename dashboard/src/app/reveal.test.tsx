/**
 * Revealing what a link names, on arrival and at no other time.
 *
 * All three rules are invisible in a URL and each was the obvious code's
 * failure: a screen scrolling in its own effect was scrolled back to the top
 * by the router's reset that runs after it; a reveal on Back fought the
 * position restore for its frames; and `scrollIntoView` does not exist in
 * jsdom at all, so the route smoke test would have thrown.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { Router, useNavigator, useRevealOnArrival, type Navigator } from "./router.tsx";

let scroller: HTMLElement;
let nav: Navigator | null = null;
const measure = HTMLElement.prototype.getBoundingClientRect;

/** The one scroll container, with a scrollTop jsdom will actually keep. */
function makeScroller(): HTMLElement {
  const el = document.createElement("div");
  el.id = "screen-scroll";
  let top = 0;
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    get: () => top,
    // Clamped, as a browser clamps it: there is nothing above the top.
    set: (v: number) => {
      top = Math.max(0, v);
    },
  });
  el.getBoundingClientRect = () => ({ top: 100 }) as DOMRect;
  document.body.appendChild(el);
  return el;
}

/**
 * Lay the target out `top` pixels down the viewport, for everything measured
 * from here on. jsdom lays nothing out, so a test says where things are.
 */
function layOut(top: number) {
  HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
    return (this.id === "unit-backend" ? { top } : { top: 0 }) as DOMRect;
  };
}

function Probe({ show, target }: { show: boolean; target: string | null }) {
  nav = useNavigator();
  useRevealOnArrival(target);
  return show ? <div id="unit-backend">Backend</div> : null;
}

function mount(show: boolean, target: string | null = "unit-backend") {
  const ui = (s: boolean) => (
    <Router>
      <Probe show={s} target={target} />
    </Router>
  );
  const view = render(ui(show), { container: scroller });
  return { rerender: (s: boolean) => view.rerender(ui(s)) };
}

beforeEach(() => {
  history.replaceState(null, "", "#/org?unit=Backend");
  scroller = makeScroller();
  layOut(0);
});

afterEach(() => {
  cleanup();
  HTMLElement.prototype.getBoundingClientRect = measure;
  scroller.remove();
  nav = null;
  history.replaceState(null, "", "#/");
});

test("an arrival reveals the element after the router's reset", () => {
  layOut(900);
  mount(true);
  // 900 down the viewport, the scroller's own top at 100: 800 into it.
  expect(scroller.scrollTop).toBe(800);
});

test("an element that appears after the arrival is revealed when it does", () => {
  const view = mount(false);
  expect(scroller.scrollTop).toBe(0);
  // The org projection lands on the socket after the route did.
  layOut(400);
  view.rerender(true);
  expect(scroller.scrollTop).toBe(300);
});

test("a reader who has started reading is not moved", () => {
  const view = mount(false);
  scroller.dispatchEvent(new Event("wheel"));
  layOut(400);
  view.rerender(true);
  expect(scroller.scrollTop).toBe(0);
});

test("a filter change replaces the entry and reveals nothing", () => {
  const view = mount(false);
  act(() => nav!.filter({ unit: "Frontend" }));
  scroller.scrollTop = 120;
  layOut(400);
  view.rerender(true);
  // The reader is already looking at what they changed.
  expect(scroller.scrollTop).toBe(120);
});

test("Back to an entry restores its position instead of revealing", async () => {
  mount(true);
  const key = (history.state as { crewletKey?: string } | null)?.crewletKey;
  expect(key).toBeTruthy();
  scroller.scrollTop = 250;
  // Away to a new entry, which files this one's position under its key.
  act(() => nav!.to(["people"]));
  expect(scroller.scrollTop).toBe(0);

  // And Back: the entry returned to carries its key, so it is a restore.
  layOut(900);
  act(() => {
    history.pushState({ crewletKey: key }, "", "#/org?unit=Backend");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await act(() => new Promise<void>((resolve) => requestAnimationFrame(() => resolve())));
  // Where the reader was, not where the link points.
  expect(scroller.scrollTop).toBe(250);
});
