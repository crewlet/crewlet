/**
 * The one thing the rail's width has to be: a single value both readers see.
 *
 * The grid track that sizes the peek column is declared on `.app`, which is
 * the rail's ANCESTOR, and the narrow-layout drawer's own `width` is on the
 * rail itself. A custom property inherits DOWNWARD ONLY, so the value written
 * on the `<aside>` — where it was — could never reach the track: the column
 * stayed at its 420px fallback for ever, `.peek-rail` deliberately declares no
 * width of its own, and a drag moved React state, wrote localStorage and
 * changed nothing on screen.
 *
 * Asserted on the document element rather than through layout, because jsdom
 * computes none — and because the root IS the fix: what makes the column and
 * the panel unable to disagree is that neither of them owns the value.
 */

import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { DetailRail, peekRow } from "./DetailRail.tsx";
import { PeekRestingWidth, PeekWidthRequest, usePeekWidth } from "./peekWidth.ts";
import { Router } from "~/app/router.tsx";
import { CANVAS_PEEK_WIDTH } from "~/app/layout.ts";

beforeEach(() => {
  localStorage.clear();
  location.hash = "#/agents/roster?peek=seat:ceo";
});

afterEach(() => {
  cleanup();
  document.documentElement.style.removeProperty("--peek-w");
  location.hash = "#/";
});

function rail(resting?: number) {
  const body = (
    <DetailRail ref={{ kind: "seat", id: "ceo" }}>
      <p>a seat</p>
    </DetailRail>
  );
  return render(
    <Router>
      {resting === undefined ? (
        body
      ) : (
        <PeekRestingWidth.Provider value={resting}>{body}</PeekRestingWidth.Provider>
      )}
    </Router>,
  );
}

function published(): string {
  return document.documentElement.style.getPropertyValue("--peek-w");
}

test("the rail publishes its width where the grid track can read it", () => {
  const { container } = rail();
  expect(published()).toBe("420px");
  // AND NOT ON THE RAIL. An inline value there satisfies the drawer and
  // nothing else — it is below `.app` in the tree, so the track above it
  // cannot inherit it.
  const aside = container.querySelector<HTMLElement>("aside.peek-rail");
  expect(aside).not.toBeNull();
  expect(aside!.style.getPropertyValue("--peek-w")).toBe("");
});

test("a drag moves the value the grid track resolves", () => {
  const { container } = rail();
  const grip = container.querySelector<HTMLElement>(".peek-grip");
  const aside = container.querySelector<HTMLElement>("aside.peek-rail");
  expect(grip).not.toBeNull();
  // THE RAIL'S OWN RIGHT EDGE is what a width is measured from, not the
  // window's: the sheet sits 8px inside the window, so `innerWidth - clientX`
  // made the panel 8px wider than the pointer asked for. jsdom lays nothing
  // out, so the edge is given here — 1016, a 1024 window less the inset.
  aside!.getBoundingClientRect = () => ({ right: 1016 }) as DOMRect;
  fireEvent.mouseDown(grip!);
  // A pointer at 516 asks for a 500px rail — inside the 330..640 it clamps to.
  fireEvent.mouseMove(window, { clientX: 516 });
  expect(published()).toBe("500px");
  fireEvent.mouseUp(window);
  expect(localStorage.getItem("crewlet.peek.width")).toBe("500");
});

// A CANVAS RESTS ITS PEEK NARROWER: a chart is shrunk into what the rail
// leaves it, so the org chart asks for the approved 330 rather than a list's
// 420 — and the rail honours the screen's width until the reader drags one.
test("the rail rests at the width the screen under it asked for", () => {
  rail(CANVAS_PEEK_WIDTH);
  expect(published()).toBe(`${CANVAS_PEEK_WIDTH}px`);
});

// THE READER'S WIDTH IS THEIR PREFERENCE, on every screen: a width dragged on
// one screen is not undone by the next one's resting width.
test("a width the reader dragged wins over the screen's resting width", () => {
  localStorage.setItem("crewlet.peek.width", "500");
  rail(CANVAS_PEEK_WIDTH);
  expect(published()).toBe("500px");
});

// A SCREEN ASKS, AND TAKES IT BACK: the next screen's rail must not rest at
// the last one's width.
test("a screen's request for a resting width is withdrawn when it goes", () => {
  const asked: (number | null)[] = [];
  function Canvas() {
    usePeekWidth(CANVAS_PEEK_WIDTH);
    return null;
  }
  const view = render(
    <PeekWidthRequest.Provider value={(width) => asked.push(width)}>
      <Canvas />
    </PeekWidthRequest.Provider>,
  );
  expect(asked).toEqual([CANVAS_PEEK_WIDTH]);
  view.unmount();
  expect(asked).toEqual([CANVAS_PEEK_WIDTH, null]);
});

test("a closed rail leaves no width pinned on the document", () => {
  const view = rail();
  expect(published()).toBe("420px");
  view.unmount();
  expect(published()).toBe("");
});

// THE KEYBOARD PATH IS WHY `peekRow` EXISTS, and it is the half that broke.
//
// `rowPeekHandler` decides on `e.button`; a KeyboardEvent has none, so
// `undefined !== 0` read as "the reader meant elsewhere" and `DataGrid`'s
// `enter` chord opened nothing. Three screens each wrote this adapter
// privately before it lived here, so the rule is pinned once.
describe("peekRow", () => {
  test("a keyboard activation opens, having no modifiers to read", () => {
    const seen: string[] = [];
    const handler = peekRow<{ id: string }>((r) => seen.push(r.id));
    // What DataGrid's `enter` chord passes: a KeyboardEvent, no `button`.
    handler({ id: "one" }, { key: "Enter" } as unknown as React.KeyboardEvent);
    expect(seen).toEqual(["one"]);
  });

  test("a plain click opens and takes the navigation with it", () => {
    const seen: string[] = [];
    let prevented = false;
    const handler = peekRow<{ id: string }>((r) => seen.push(r.id));
    handler({ id: "two" }, {
      button: 0,
      defaultPrevented: false,
      preventDefault: () => {
        prevented = true;
      },
    } as unknown as React.MouseEvent);
    expect(seen).toEqual(["two"]);
    // The row is an anchor to the PAGE; a plain click means the rail instead,
    // so the default has to be taken or the reader gets both.
    expect(prevented).toBe(true);
  });

  test("⌘-click is left alone, so the browser opens the page in a tab", () => {
    const seen: string[] = [];
    let prevented = false;
    const handler = peekRow<{ id: string }>((r) => seen.push(r.id));
    handler({ id: "three" }, {
      button: 0,
      metaKey: true,
      defaultPrevented: false,
      preventDefault: () => {
        prevented = true;
      },
    } as unknown as React.MouseEvent);
    expect(seen).toEqual([]);
    expect(prevented).toBe(false);
  });
});
