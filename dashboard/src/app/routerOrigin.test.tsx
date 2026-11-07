/**
 * The router's part in bringing a reader back to exactly where they were: a
 * page opened from a place that names itself its origin carries that place's
 * address, a screen notes where its reader is without a move, a panel laid
 * over a list keeps the list's scroll, and an address that says where its
 * screen was scrolled opens there.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { Router, useNavigator, useRoute, useTrailOrigin, type Route } from "./router.tsx";

let nav: ReturnType<typeof useNavigator> | null = null;
let shown: Route | null = null;

const toSeat = (to: Route) => to.path[0] === "seats";

/** The origin screen: a list that names itself the origin of a seat's page. */
function Origin() {
  useTrailOrigin(toSeat);
  return <p>list</p>;
}

function Screen() {
  nav = useNavigator();
  shown = useRoute();
  return shown.path[0] === "list" ? <Origin /> : <p>{shown.path.join("/")}</p>;
}

function mount() {
  render(
    <div id={SCREEN_SCROLL_ID}>
      <Router>
        <Screen />
      </Router>
    </div>,
  );
  const el = document.getElementById(SCREEN_SCROLL_ID)!;
  let top = 0;
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    get: () => top,
    set: (v: number) => {
      top = v;
    },
  });
  return el;
}

const nextFrame = window.requestAnimationFrame.bind(window);
const waiting = new Set<number>();
async function settleFrames() {
  while (waiting.size > 0) {
    await act(() => new Promise<void>((resolve) => nextFrame(() => resolve())));
  }
}

beforeEach(() => {
  history.replaceState(null, "", "#/list?show=needs");
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
  shown = null;
  vi.restoreAllMocks();
  waiting.clear();
});

// A PAGE OPENED FROM A PLACE THAT NAMES ITSELF ITS ORIGIN carries the place's
// whole address, the view it noted included, whichever way it was opened.
//
// Mutation: drop the stamp from `go`, or read the address before the note,
// and this fails.
test("a page opened from an origin carries its whole address", async () => {
  mount();
  await settleFrames();
  act(() => nav!.note({ at: "420" }));
  act(() => nav!.to(["seats", "pm"], { tab: "work" }));
  await settleFrames();
  const from = new URLSearchParams(location.hash.split("?")[1]).get("from");
  expect(from).toBe("/list?show=needs&at=420");
  expect(shown?.query.get("tab")).toBe("work");
});

test("a page opened from anywhere else carries no origin", async () => {
  history.replaceState(null, "", "#/home");
  mount();
  await settleFrames();
  act(() => nav!.to(["seats", "pm"]));
  await settleFrames();
  expect(location.hash).toBe("#/seats/pm");
});

// A NOTE IS WHERE THE READER ALREADY IS: the address says it, and nothing
// moves: no history entry, no new route for the screen to redraw for.
test("a note writes the address without a move", async () => {
  mount();
  await settleFrames();
  const before = history.length;
  const route = shown;
  act(() => nav!.note({ zoom: "0.5,0,0,t" }));
  expect(location.hash).toContain("zoom=0.5");
  expect(history.length).toBe(before);
  expect(shown).toBe(route);
});

// A PANEL OVER THE SCREEN IS A NEW ENTRY THE SCREEN STANDS STILL UNDER: a seat
// panel opened from row 25 leaves row 25 in view, where a new screen starts at
// the top.
test("a panel laid over a list keeps the list's scroll", async () => {
  const el = mount();
  await settleFrames();
  el.scrollTop = 640;
  act(() => nav!.overlay("peek", "seat:pm"));
  await settleFrames();
  expect(location.hash).toContain("peek=seat%3Apm");
  expect(el.scrollTop).toBe(640);
});

// AN ADDRESS THAT SAYS WHERE ITS SCREEN WAS SCROLLED opens there, the way a
// seat page's crumb back to the list it came from does.
test("a new entry opens at the scroll its address names", async () => {
  const el = mount();
  await settleFrames();
  act(() => nav!.to(["list"], { at: "300" }));
  await settleFrames();
  expect(el.scrollTop).toBe(300);
});
