/**
 * The peek's width and its Open link, as the frame wires them onto the kit's
 * rail.
 *
 * THE WIDTH IS THE READER'S, then the screen's: a width dragged on one screen
 * is kept per browser under the frame's own storage key and wins on every
 * screen, and until there is one the rail rests at the width the screen under
 * it asked for (a canvas asks for 330). The rail draws that width as one
 * value its column and its drawer both read, which is the kit's to hold; what
 * is asserted here is that the frame hands it the right one and keeps a
 * reader's resize.
 *
 * Asserted on the rail's own published value rather than through layout,
 * because jsdom computes none.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Suspense } from "react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { PeekHost } from "./PeekHost.tsx";
import { peekHref } from "./peekAddress.ts";
import { PeekRestingWidth, PeekWidthRequest, usePeekWidth } from "./peekWidth.ts";
import { Router } from "~/app/router.tsx";
import { CANVAS_PEEK_WIDTH, PEEK_MAX } from "~/app/layout.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

beforeEach(() => {
  localStorage.clear();
  location.hash = "#/company/org/roster?peek=seat:ceo";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

async function host(resting?: number) {
  const body = (
    <Suspense fallback={null}>
      <PeekHost />
    </Suspense>
  );
  // IN AN ASYNC ACT: a peek body is a lazy chunk, and a render that suspends
  // on one leaves its effects queued until the act that started it settles.
  let view!: ReturnType<typeof render>;
  await act(async () => {
    view = render(
      <Router>
        {resting === undefined ? (
          body
        ) : (
          <PeekRestingWidth.Provider value={resting}>{body}</PeekRestingWidth.Provider>
        )}
      </Router>,
    );
  });
  return view;
}

function railWidth(): string {
  const rail = document.querySelector<HTMLElement>(".crewlet-peek-rail");
  expect(rail).not.toBeNull();
  return rail!.style.getPropertyValue("--crewlet-peek-rail-width");
}

test("the rail rests at the frame's width where the screen asks for none", async () => {
  await host();
  expect(railWidth()).toBe("420px");
});

// A CANVAS RESTS ITS PEEK NARROWER: a chart is shrunk into what the rail
// leaves it, so the org chart asks for the approved 330 rather than a list's
// 420, and the rail honours the screen's width until the reader drags one.
test("the rail rests at the width the screen under it asked for", async () => {
  await host(CANVAS_PEEK_WIDTH);
  expect(railWidth()).toBe(`${CANVAS_PEEK_WIDTH}px`);
});

// THE READER'S WIDTH IS THEIR PREFERENCE, on every screen: a width dragged on
// one screen is not undone by the next one's resting width.
test("a width the reader dragged wins over the screen's resting width", async () => {
  localStorage.setItem(STORAGE_KEYS.peekWidth, "500");
  await host(CANVAS_PEEK_WIDTH);
  expect(railWidth()).toBe("500px");
});

// A RESIZE IS KEPT WHERE IT ENDS, under the frame's own key, so the next
// load opens at it. The grip is a separator with a value, so the keyboard
// sizes it as well as the pointer; End is the widest the rail may be.
test("a resize is drawn at once and kept under the frame's storage key", async () => {
  await host();
  const grip = screen.getByRole("separator", { name: "Resize the detail panel" });
  // IN AN ASYNC ACT, for the reason `host` gives: the resize draws the rail
  // again, and a peek body that suspends on its lazy chunk inside a sync act
  // leaves the act unawaited.
  await act(async () => {
    fireEvent.keyDown(grip, { key: "End" });
  });
  expect(railWidth()).toBe(`${PEEK_MAX}px`);
  expect(localStorage.getItem(STORAGE_KEYS.peekWidth)).toBe(String(PEEK_MAX));
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

// A KIND WHOSE PEEK IS ITS PAGE (`KindSpec.peekIsPage`): a person, whose
// directory row is read beside People and nowhere else. A new tab opened on
// its "page" was the bare list, which forgot who it was about, and an `Open`
// over that list could only close the peek. The CONTROL is a seat, whose
// page is its own. Mutation: build its address from the path alone, or draw
// Open whatever the page, and a line here goes red.
describe("a kind whose peek is its page", () => {
  const person = { kind: "person" as const, id: "p-1" };

  test("its address is the list it is read beside, with its peek open", () => {
    expect(peekHref(person)).toBe("#/settings/people?peek=person%3Ap-1");
    expect(peekHref({ kind: "seat", id: "ceo" })).toBe("#/company/org/seats/ceo");
  });

  test("over that list the rail draws no Open; opened anywhere else, Open leads there", async () => {
    location.hash = "#/settings/people?peek=person:p-1";
    const first = await host();
    expect(screen.queryByRole("link", { name: /^Open/ })).toBeNull();
    first.unmount();

    location.hash = "#/company/org?peek=person:p-1";
    await host();
    expect(screen.getByRole("link", { name: /^Open/ }).getAttribute("href")).toBe(
      "#/settings/people?peek=person%3Ap-1",
    );
  });
});
