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

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "~/test/inCase.ts";
import { afterAll, afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";

import { WorkSearch } from "./WorkSearch.tsx";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type WorkRanked } from "~/protocol/index.ts";
import { BugGlyph } from "@crewlethq/icons/glyphs";
import {
  CLAIMANT_HREF,
  CLAIMANT_TITLE,
  DUPLICATE,
  DUPLICATE_HREF,
  DUPLICATE_TITLE,
  SHARED_KEY,
  collidingHits,
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

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/work/search?q=auth";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** Two items' ids, in the shape the tracker mints them (a UUIDv7). */
const AUTH = "0198f0a0-0000-7000-8000-00000000000a";
const RUNBOOK = "0198f0a0-0000-7000-8000-00000000000b";

/**
 * One hit, IN THE WIRE'S OWN SHAPE.
 *
 * Typed as `WorkRanked` rather than a loose record, because the loose record
 * is how this suite came to omit the `id` every real hit carries — it is the
 * `tracker_tasks` primary key, read in the same query as the title — while the
 * grid keys its rows on it. Every hit was keyed `undefined`, so React printed
 * the duplicate-key warning on each run and fell back to keying by POSITION,
 * which is the one keying a ranked answer cannot have: see the re-rank case
 * below. Typed, a fixture missing a field the engine always sends is a
 * typecheck failure rather than a console line nobody reads.
 */
const hit = (over: Partial<WorkRanked>): WorkRanked => ({
  id: AUTH,
  key: "ENG-1",
  title: "Authentication rework",
  project: "ENG",
  type: "bug",
  status: "in_progress",
  priority: "normal",
  rank: 1,
  snippet: "",
  ...over,
});

/** What the engine answers: one fixed list, or one chosen by the phrase asked. */
type Answer = WorkRanked[] | ((q: string) => WorkRanked[]);

/**
 * The screen over a socket answering `answer`, with `outcome` beside the hits
 * (what mode was served, and why) and every question it was asked pushed onto
 * `asked`. With `rail`, it is mounted the way the frame mounts it — beside the
 * peek rail, inside the provider the screen publishes its `[`/`]` order to —
 * so a case can step the rail.
 */
function mount(
  answer: Answer,
  {
    rail = false,
    outcome = {},
    asked = [],
  }: {
    rail?: boolean;
    outcome?: Record<string, unknown>;
    asked?: Record<string, unknown>[];
  } = {},
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as { query: (w: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (_what, params) => {
    asked.push(params);
    return Promise.resolve({
      hits: typeof answer === "function" ? answer(String(params.q ?? "")) : answer,
      available: true,
      mode: params.mode ?? "hybrid",
      ...outcome,
    });
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        {rail ? (
          <PeekNeighbours>
            <WorkSearch />
            <PeekHost />
          </PeekNeighbours>
        ) : (
          <WorkSearch />
        )}
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
    hit({ id: AUTH, key: "ENG-1", title: "Authentication rework", type: "bug" }),
    hit({ id: RUNBOOK, key: "ENG-2", title: "Write the runbook", type: "task", rank: 2 }),
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

/** The row at a place in the drawn order. */
const rowAt = (place: number) => document.querySelectorAll<HTMLElement>(".grid-row")[place]!;

// A ROW IS ITS HIT, WHEREVER THE NEXT RANKING PUTS IT.
//
// A refined phrase re-ranks the same items, and the grid stays on screen while
// the new answer arrives (`useQuery` keeps the last one). Whatever a row holds
// that the answer does not — here the keyboard focus a reader left on it — has
// to travel with the HIT rather than stay at the PLACE, and the row key is what
// decides which. Keyed on the hit's id, React moves the row. Keyed on anything
// two hits can share — `undefined` from a fixture without ids, a rank, a type —
// React reconciles by position, and the focus a reader put on one hit is
// silently on whichever hit holds that place now.
test("a re-ranked answer keeps each row's own state with its own hit", async () => {
  const auth = hit({ id: AUTH, key: "ENG-1", title: "Authentication rework", type: "bug" });
  const runbook = hit({ id: RUNBOOK, key: "ENG-2", title: "Write the runbook", type: "task" });
  mount((q) =>
    q === "runbook"
      ? [
          { ...runbook, rank: 1 },
          { ...auth, rank: 2 },
        ]
      : [
          { ...auth, rank: 1 },
          { ...runbook, rank: 2 },
        ],
  );
  await waitFor(() => expect(within(rowAt(0)).getByText("Authentication rework")).toBeTruthy());

  const link = rowAt(0).querySelector<HTMLAnchorElement>("a.row-link");
  expect(link).toBeTruthy();
  link!.focus();
  expect(document.activeElement).toBe(link);

  // The reader refines the phrase without leaving the row: a change and a
  // submit move no focus.
  const field = screen.getByRole("searchbox", { name: "Search the company’s work" });
  fireEvent.change(field, { target: { value: "runbook" } });
  fireEvent.submit(field.closest("form")!);
  await waitFor(() => expect(within(rowAt(0)).getByText("Write the runbook")).toBeTruthy());

  // THE FOCUS IS STILL ON THE AUTHENTICATION HIT, which is second now.
  expect((document.activeElement as HTMLElement).closest(".grid-row")).toBe(rowAt(1));
  expect(within(rowAt(1)).getByText("Authentication rework")).toBeTruthy();
});

// TWO HITS UNDER ONE KEY ARE TWO TASKS.
//
// A search that finds both holders of a key — a restored counter's duplicate
// beside the task that claimed the key first — drew two rows that opened one
// task: this screen addressed a hit by its key wherever it had one, and the
// key opens the claimant. The flagged hit goes by its id, in its link, in the
// peek a plain click opens, and in which row is drawn as the open one.
test("two hits under one key open two different tasks", async () => {
  mount(collidingHits());
  await waitFor(() => expect(screen.getByText(DUPLICATE_TITLE)).toBeTruthy());

  const link = (title: string) => rowOf(title).querySelector<HTMLAnchorElement>("a.row-link")!;
  expect(link(CLAIMANT_TITLE).getAttribute("href")).toBe(CLAIMANT_HREF);
  expect(link(DUPLICATE_TITLE).getAttribute("href")).toBe(DUPLICATE_HREF);

  fireEvent.click(link(DUPLICATE_TITLE));
  await waitFor(() => expect(peekNow()).toBe(`item:${DUPLICATE}`));
  await waitFor(() => expect(rowOf(DUPLICATE_TITLE).classList.contains("selected")).toBe(true));
  expect(rowOf(CLAIMANT_TITLE).classList.contains("selected")).toBe(false);

  fireEvent.click(link(CLAIMANT_TITLE));
  await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
  await waitFor(() => expect(rowOf(CLAIMANT_TITLE).classList.contains("selected")).toBe(true));
  expect(rowOf(DUPLICATE_TITLE).classList.contains("selected")).toBe(false);
});

// AND `[` / `]` STEP DOWN THE RANKING FROM ONE OF THE PAIR TO THE OTHER. The
// rail walks the order this screen publishes, matched against the open peek
// by token: published by key, both hits were the claimant's entry, so `]` from
// the claimant stayed put and the duplicate's rail, matching no entry, drew no
// stepper at all.
test("[ and ] step the rail between two hits under one key", async () => {
  mount(collidingHits(), { rail: true });
  await waitFor(() => expect(screen.getByText(DUPLICATE_TITLE)).toBeTruthy());

  const link = (title: string) => rowOf(title).querySelector<HTMLAnchorElement>("a.row-link")!;
  // IN AN ASYNC ACT: the item's peek body is a lazy chunk, and the click that
  // opens it suspends the rail until the chunk has loaded.
  await act(async () => {
    fireEvent.click(link(CLAIMANT_TITLE));
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
  await waitFor(() => expect(screen.getByLabelText("Next")).toBeTruthy());

  await act(async () => {
    fireEvent.keyDown(window, { key: "]" });
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${DUPLICATE}`));

  await waitFor(() => expect(screen.getByLabelText("Previous")).toBeTruthy());
  await act(async () => {
    fireEvent.keyDown(window, { key: "[" });
  });
  await waitFor(() => expect(peekNow()).toBe(`item:${SHARED_KEY}`));
});

// THE MODE REACHES THE WIRE. Hybrid, Keyword and Meaning are the engine's
// three rankings (`hybrid`, `keyword`, `semantic`); the segment writes `mode=`
// into the address and the question carries it — Meaning is the word on the
// control and never on the wire.
test("the mode a reader picks is the mode the engine is asked for", async () => {
  const asked: Record<string, unknown>[] = [];
  mount([hit({})], { asked });
  await waitFor(() => expect(asked.at(-1)?.mode).toBe("hybrid"));
  fireEvent.click(screen.getByRole("radio", { name: "Meaning" }));
  await waitFor(() => expect(asked.at(-1)?.mode).toBe("semantic"));
  expect(location.hash).toContain("mode=semantic");
});

// WHAT WAS SERVED IS SAID when it is not what was asked: a company with no
// embeddings provider that asked for Hybrid is answered Keyword, and a keyword
// ranking passed off as a hybrid one is a claim nobody made.
test("an answer served in another mode says so, and why", async () => {
  mount([hit({})], { outcome: { served_mode: "keyword", degraded: "no_embeddings" } });
  await waitFor(() => expect(screen.getByText(/Asked for Hybrid, served Keyword/)).toBeTruthy());
  expect(screen.getByText(/no embeddings provider/)).toBeTruthy();
});

// AND A MODE THIS BUILD DOES NOT DRAW falls back to the default rather than
// meeting a refusal over the whole screen because of one stale address key.
test("an unknown mode off the address asks for the default", async () => {
  location.hash = "#/work/search?q=auth&mode=vibes";
  const asked: Record<string, unknown>[] = [];
  mount([hit({})], { asked });
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
