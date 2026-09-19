/**
 * What a ranked hit says it IS.
 *
 * The Item column drew `<TextCell icon="check">` — one hardcoded mark on every
 * row, whatever the item was — and that is wrong twice over. A bug, an epic and
 * a spike are three drawings on the board, the list, the table, the timeline and
 * the item's own header, and one here. And a TICK is this product's completion
 * vocabulary, so a hit whose Status cell one column over read "In progress"
 * carried a mark saying it was finished.
 *
 * This screen has no Type column either, so the mark is the ONLY place the type
 * is stated — which is why the fix is `TypeIcon` and not `icon={typeIcon(...)}`:
 * a name-keyed cell mark renders `aria-hidden` on purpose, so the right drawing
 * alone would still be a fact nobody can name.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";

import { WorkSearch } from "./WorkSearch.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { LOCAL_DRAWINGS } from "~/ui/glyph.tsx";

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

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/work/search?q=auth";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const hit = (over: Record<string, unknown>) => ({
  key: "ENG-1",
  title: "Authentication rework",
  type: "bug",
  status: "in_progress",
  rank: 1,
  score: 1,
  snippet: "",
  ...over,
});

function mount(hits: unknown[]) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ hits, available: true, mode: "hybrid" });
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <WorkSearch />
      </Router>
    </ClientContext.Provider>,
  );
}

/** The row a title sits in. */
const rowOf = (title: string) => screen.getByText(title).closest(".grid-row") as HTMLElement;

/** The drawing a row's type mark actually renders. */
const pathOf = (row: HTMLElement) => row.querySelector(".work-type path")?.getAttribute("d");

test("a hit wears its own type's mark, and says which type that is", async () => {
  mount([
    hit({ key: "ENG-1", title: "Authentication rework", type: "bug" }),
    hit({ key: "ENG-2", title: "Write the runbook", type: "task", rank: 2 }),
  ]);
  await waitFor(() => expect(screen.getByText("Authentication rework")).toBeTruthy());

  const bug = rowOf("Authentication rework");
  const task = rowOf("Write the runbook");
  // THE WIRING: a hardcoded `<TypeIcon type="task">` would give the bug row the
  // wrong word and fail here.
  expect(within(bug).getByText("Bug")).toBeTruthy();
  expect(within(task).getByText("Task")).toBeTruthy();
  // THE VISIBLE DEFECT: the DRAWING varies per row. A component that named the
  // type correctly but drew one mark for all of them passes the two above.
  expect(pathOf(bug)).not.toBe(pathOf(task));
  // AND IT IS THE SAME GLYPH THE BOARD AND THE LIST GIVE IT. `TypeIcon` passes
  // `size="sm"` = 14px, and `glyphOpticalSize` only returns 24 above 20.
  expect(pathOf(bug)).toBe(LOCAL_DRAWINGS["bug_report"]?.[20]);
});

// TWO ANSWERS ON ONE ROW. A tick is the completion mark, so a hit the engine
// reports as in progress carried a mark saying it was done, one column from the
// status badge saying otherwise.
test("an in-progress hit is not marked done", async () => {
  mount([hit({})]);
  await waitFor(() => expect(screen.getByText("Authentication rework")).toBeTruthy());
  const row = rowOf("Authentication rework");
  expect(within(row).getByText("In progress")).toBeTruthy();
  expect(within(row).getByText("Bug")).toBeTruthy();
  expect(row.textContent).not.toContain("Done");
});
