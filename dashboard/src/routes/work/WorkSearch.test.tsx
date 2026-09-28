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

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";

import { WorkSearch } from "./WorkSearch.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { BugGlyph } from "@crewlethq/icons/glyphs";

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

function mount(
  hits: unknown[],
  outcome: Record<string, unknown> = {},
  asked: Record<string, unknown>[] = [],
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as { query: (w: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (_what, params) => {
    asked.push(params);
    return Promise.resolve({ hits, available: true, mode: params.mode ?? "hybrid", ...outcome });
  };
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
  // AND IT IS THE SAME GLYPH THE BOARD AND THE LIST GIVE IT: the design
  // system's own bug, one Lucide drawing at every size.
  const { container: reference } = render(<BugGlyph />);
  expect(pathOf(bug)).toBe(reference.querySelector("path")?.getAttribute("d"));
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

// THE MODE REACHES THE WIRE. Hybrid, Keyword and Meaning are the engine's
// three rankings (`hybrid`, `keyword`, `semantic`); the segment writes `mode=`
// into the address and the question carries it — Meaning is the word on the
// control and never on the wire.
test("the mode a reader picks is the mode the engine is asked for", async () => {
  const asked: Record<string, unknown>[] = [];
  mount([hit({})], {}, asked);
  await waitFor(() => expect(asked.at(-1)?.mode).toBe("hybrid"));
  fireEvent.click(screen.getByRole("radio", { name: "Meaning" }));
  await waitFor(() => expect(asked.at(-1)?.mode).toBe("semantic"));
  expect(location.hash).toContain("mode=semantic");
});

// WHAT WAS SERVED IS SAID when it is not what was asked: a company with no
// embeddings provider that asked for Hybrid is answered Keyword, and a keyword
// ranking passed off as a hybrid one is a claim nobody made.
test("an answer served in another mode says so, and why", async () => {
  mount([hit({})], { served_mode: "keyword", degraded: "no_embeddings" });
  await waitFor(() => expect(screen.getByText(/Asked for Hybrid, served Keyword/)).toBeTruthy());
  expect(screen.getByText(/no embeddings provider/)).toBeTruthy();
});

// AND A MODE THIS BUILD DOES NOT DRAW falls back to the default rather than
// meeting a refusal over the whole screen because of one stale address key.
test("an unknown mode off the address asks for the default", async () => {
  location.hash = "#/work/search?q=auth&mode=vibes";
  const asked: Record<string, unknown>[] = [];
  mount([hit({})], {}, asked);
  await waitFor(() => expect(asked.at(-1)?.mode).toBe("hybrid"));
});

// A SNIPPET IS A CUT OF A MARKDOWN BODY, and it is read as the prose it
// renders to: `**Repro:**` on a ranked row is two pairs of asterisks nobody
// wrote to be seen.
test("a hit's snippet is drawn as prose, without its markdown marks", async () => {
  mount([hit({ snippet: "**Repro:** run `make soak` on a #cold node" })]);
  await waitFor(() => expect(screen.getByText("Authentication rework")).toBeTruthy());
  const row = rowOf("Authentication rework");
  expect(row.textContent).toContain("Repro: run make soak on a #cold node");
  expect(row.textContent).not.toContain("**");
});
