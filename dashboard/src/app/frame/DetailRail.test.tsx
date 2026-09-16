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
import { Router } from "~/app/router.tsx";

beforeEach(() => {
  localStorage.clear();
  location.hash = "#/company/people?peek=seat:ceo";
});

afterEach(() => {
  cleanup();
  document.documentElement.style.removeProperty("--peek-w");
  location.hash = "#/";
});

function rail() {
  return render(
    <Router>
      <DetailRail ref={{ kind: "seat", id: "ceo" }}>
        <p>a seat</p>
      </DetailRail>
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
  expect(grip).not.toBeNull();
  fireEvent.mouseDown(grip!);
  // jsdom's window is 1024 wide, so a pointer at 524 asks for a 500px rail —
  // inside the 360..640 the drag clamps to.
  fireEvent.mouseMove(window, { clientX: 524 });
  expect(published()).toBe("500px");
  fireEvent.mouseUp(window);
  expect(localStorage.getItem("crewlet.peek.width")).toBe("500");
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
