/**
 * Two tasks under one key open as two tasks, in every shape the work list draws.
 *
 * A key counter restored beside newer work hands out numbers already held, so
 * two tasks answer to `ENG-7` — and the engine opens that key on the one that
 * claimed it first and flags the other `key_collision`. Every shape built its
 * link, its peek, its stepper and its "this is the open one" check out of
 * `row.key`, so both cards opened the claimant and the duplicate could not be
 * reached by clicking it anywhere. These cases click each of the pair in each
 * shape and read where it went.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, afterAll, expect, test, vi } from "vitest";

import { Work } from "./Work.tsx";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type QueryName, type WorkSummary } from "~/protocol/index.ts";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { ReactNode } from "react";
import {
  CLAIMANT_HREF,
  CLAIMANT_TITLE,
  DUPLICATE,
  DUPLICATE_HREF,
  DUPLICATE_TITLE,
  SHARED_KEY,
  collidingRows,
  peekNow,
} from "~/test/keyCollision.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// jsdom implements no scrolling, and the grid scrolls its cursor row.
const scrollIntoView = Element.prototype.scrollIntoView;
beforeAll(() => {
  Element.prototype.scrollIntoView = function () {};
});
afterAll(() => {
  Element.prototype.scrollIntoView = scrollIntoView;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** The client the screen reads: a real store, and a socket answering from `answers`. */
let client: { store: Store; socket: LiveSocket };

/** The pair, dated so the timeline draws a bar and the calendar a chip. */
const rows = (): WorkSummary[] =>
  collidingRows({ start: "2031-04-08T00:00:00Z", due: "2031-04-10T00:00:00Z" });

/** The engine, answering the list with the pair — as columns for a board. */
function serving(shape: string) {
  const pair = rows();
  const answers: Partial<Record<QueryName, unknown>> = {
    work_items:
      shape === "board"
        ? {
            items: [],
            groups: [{ key: "todo", label: "To do", count: 2, rows: pair }],
            complete: true,
          }
        : { items: pair, groups: [], complete: true },
  };
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [] } as never);
  const socket = new LiveSocket(store);
  socket.query = (async (what: string) =>
    answers[what as QueryName] ?? {}) as unknown as typeof socket.query;
  client = { store, socket };
}

/** Where each shape draws a row: the element carrying its link and its click. */
const ANCHOR: Record<string, string> = {
  list: ".grid-row",
  table: ".grid-row",
  board: ".work-card",
  timeline: ".tl-bar",
  calendar: ".work-cal-chip",
};

/** The element a title is drawn in, in this shape — the one marked as open. */
function anchorOf(shape: string, title: string): HTMLElement {
  const all = [...document.querySelectorAll<HTMLElement>(ANCHOR[shape] ?? "a")];
  const hit = all.find((el) =>
    shape === "timeline" ? el.title.includes(title) : el.textContent?.includes(title),
  );
  if (!hit) throw new Error(`no ${ANCHOR[shape]} carries “${title}” in the ${shape} shape`);
  return hit;
}

/**
 * The row's own link: the element itself where the row IS an anchor, and the
 * overlay a grid row stretches over its cells (`DataGrid`'s `.row-link`).
 */
const linkIn = (el: HTMLElement): HTMLElement =>
  el.matches("a") ? el : (el.querySelector<HTMLElement>("a.row-link") ?? el);

/**
 * Where the row's link goes, without the list it carries: a task opened from a
 * list carries that list's question (`?list=`) so its page can step through it,
 * and which task it opens is the path.
 */
const hrefOf = (el: HTMLElement) => (linkIn(el).getAttribute("href") ?? "").split("?")[0];

/** A screen inside the frame's readings — who the viewer is and their counts. */
function framed(children: ReactNode) {
  return (
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={client}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>
  );
}

for (const shape of ["list", "table", "board", "timeline", "calendar"]) {
  test(`the ${shape} opens each of two tasks under one key as itself`, async () => {
    location.hash = `#/work?shape=${shape}&month=2031-04`;
    serving(shape);
    render(framed(<Work />));
    await waitFor(() => expect(anchorOf(shape, DUPLICATE_TITLE)).toBeTruthy());

    // THE LINK: the claimant by the key a person reads, the duplicate by its
    // id — the key would open the claimant.
    expect(hrefOf(anchorOf(shape, CLAIMANT_TITLE))).toBe(CLAIMANT_HREF);
    expect(hrefOf(anchorOf(shape, DUPLICATE_TITLE))).toBe(DUPLICATE_HREF);

    // THE PEEK a plain click opens, which is the same address.
    fireEvent.click(linkIn(anchorOf(shape, DUPLICATE_TITLE)));
    await waitFor(() => expect(peekNow()).toBe(`item:${DUPLICATE}`));
    // AND THE OPEN ONE IS DRAWN AS OPEN, which only a comparison of
    // addresses can say: matched on the key, both of the pair lit up.
    if (shape !== "calendar") {
      await waitFor(() =>
        expect(anchorOf(shape, DUPLICATE_TITLE).classList.contains("selected")).toBe(true),
      );
      expect(anchorOf(shape, CLAIMANT_TITLE).classList.contains("selected")).toBe(false);
    }

    fireEvent.click(linkIn(anchorOf(shape, CLAIMANT_TITLE)));
    await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
    if (shape !== "calendar") {
      await waitFor(() =>
        expect(anchorOf(shape, CLAIMANT_TITLE).classList.contains("selected")).toBe(true),
      );
      expect(anchorOf(shape, DUPLICATE_TITLE).classList.contains("selected")).toBe(false);
    }
    // AND BOTH ARE STILL DRAWN UNDER THE KEY a person reads: the address
    // decides where a row goes, never what it is called.
    expect(screen.queryAllByText(SHARED_KEY).length).toBeGreaterThanOrEqual(2);
  });
}

// `[` AND `]` STEP FROM ONE OF THE PAIR TO THE OTHER.
//
// The rail walks the order the list publishes, matching the open peek against
// it by token. Published by key, the pair was two entries naming the claimant:
// `]` from the claimant found its own token next and stayed put, and the
// duplicate, once open by its id, matched no entry at all, so its rail had no
// stepper. The keys are pressed on the window, as a reader presses them.
test("[ and ] step the rail between two tasks under one key", async () => {
  location.hash = "#/work?shape=list";
  serving("list");
  render(
    framed(
      <PeekNeighbours>
        <Work />
        <PeekHost />
      </PeekNeighbours>,
    ),
  );
  await waitFor(() => expect(anchorOf("list", DUPLICATE_TITLE)).toBeTruthy());

  // IN AN ASYNC ACT: the item's peek body is a lazy chunk, and the click that
  // opens it suspends the rail until the chunk has loaded.
  await act(async () => {
    fireEvent.click(linkIn(anchorOf("list", CLAIMANT_TITLE)));
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
  // THE RAIL IS UP before a key is pressed, or `]` lands on no binding and
  // the case reads a stepper that was never drawn as one that did not move.
  await waitFor(() => expect(screen.getByLabelText("Next")).toBeTruthy());

  await act(async () => {
    fireEvent.keyDown(window, { key: "]" });
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${DUPLICATE}`));

  // AND BACK: the duplicate's own entry is found by its id, so its rail has
  // a stepper at all.
  await waitFor(() => expect(screen.getByLabelText("Previous")).toBeTruthy());
  await act(async () => {
    fireEvent.keyDown(window, { key: "[" });
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
});
