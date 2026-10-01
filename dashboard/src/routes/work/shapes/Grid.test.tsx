/**
 * How the tracker's one grid draws a PERSON, on both of its column sets.
 *
 * The rule is `docs/reference/dashboard-design.md`'s "A person in a grid cell
 * is a resolved name": the org chart's name, the handle only where the chart
 * has none, and the kind marked only where it is not the ordinary one. Three
 * cells here draw a person — the list's assignee, the table's assignee and a
 * trash listing's `removed_by` — and each had its own spelling of the last
 * clause, which is exactly the drift that let one of them mark every row.
 *
 * MOUNTED ON `WorkGrid` DIRECTLY rather than through the screen, because the
 * rule belongs to the column sets: reached through `ItemsView` these cases
 * would also be asserting a view strip, a scope segment and two queries, and
 * would go red for reasons that have nothing to do with how a seat is drawn.
 */

import { cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";

import { WorkGrid } from "./Grid.tsx";
import { Router } from "~/app/router.tsx";
import type { RowChrome } from "~/components/work.tsx";
import type { WorkActivityRecord, WorkSummary } from "~/protocol/index.ts";
import {
  CLAIMANT,
  CLAIMANT_TITLE,
  DUPLICATE,
  DUPLICATE_TITLE,
  SHARED_KEY,
  collidingRows,
} from "~/test/keyCollision.ts";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const row = (over: Partial<WorkSummary> = {}): WorkSummary => ({
  id: "t-1",
  key: "ENG-9",
  project: "ENG",
  title: "the wrong subtree",
  type: "task",
  status: "todo",
  updated: "2031-04-16T09:00:00Z",
  version: 1,
  ...over,
});

/**
 * The chart, as the screen hands it down: a handle in, a name and a kind out.
 *
 * `iris` is the HUMAN seat and `ada` the agent, and `departed` is in neither —
 * a handle the chart no longer holds, which is a third answer rather than a
 * missing one.
 */
const chrome: RowChrome = {
  seatName: (handle) =>
    handle === "ada" ? "Ada Okonkwo" : handle === "iris" ? "Iris Chen" : handle,
  seatKind: (handle) => (handle === "ada" ? "agent" : handle === "iris" ? "human" : undefined),
};

/** The one identity badge a single-row grid drew. */
function badge(container: HTMLElement): HTMLElement {
  const marks = container.querySelectorAll(".crewlet-avatar");
  if (marks.length !== 1) throw new Error(`expected one identity badge, drew ${marks.length}`);
  return marks[0] as HTMLElement;
}

const removal = (over: Partial<WorkActivityRecord> = {}): WorkActivityRecord => ({
  id: "h-1",
  log_seq: 9,
  log_stream: "CREWLET_WORK_LOG",
  log_generation: 1,
  at: "2031-04-15T00:00:00Z",
  effective_at: "2031-04-15T00:00:00Z",
  kind: "removed",
  actor: "ada",
  actor_kind: "agent",
  subject_kind: "task",
  subject_id: "t-1",
  subject_key: "ENG-9",
  notified: false,
  ...over,
});

/** The `removed_by` cell: the seat link's own container, and nothing else. */
function whoCell(): HTMLElement {
  const link = screen.getByTitle("@ada").closest(".cell-seat");
  const cell = link?.parentElement;
  if (!cell) throw new Error("the removed_by cell drew no seat");
  return cell;
}

function mount(
  shape: "list" | "table",
  rows: WorkSummary[],
  removals?: Map<string, WorkActivityRecord>,
  onOpen: (row: WorkSummary) => void = () => {},
) {
  return render(grid(shape, rows, removals, onOpen));
}

/** The grid as `mount` draws it, for a case that hands it a second answer. */
function grid(
  shape: "list" | "table",
  rows: WorkSummary[],
  removals?: Map<string, WorkActivityRecord>,
  onOpen: (row: WorkSummary) => void = () => {},
) {
  return (
    <Router>
      <WorkGrid
        shape={shape}
        rows={rows}
        groups={[]}
        axis=""
        chrome={chrome}
        workspace={false}
        hrefOf={(r) => `#/work/${r.key}`}
        onOpen={onOpen}
        onOverflow={() => {}}
        overflowHref={() => "#/work"}
        removals={removals}
      />
    </Router>
  );
}

// THE CHART'S NAME, NOT THE HANDLE. A handle is the database's word for a
// person and every surface in this product resolves it — so the table's
// assignee column, which is the one a reader came to the table FOR, prints
// what the chart calls them and links to their seat.
test("the table's assignee is the resolved name, linking to the seat", () => {
  mount("table", [row({ assignee: "ada" })]);
  const link = screen.getByText("Ada Okonkwo").closest("a") as HTMLAnchorElement;
  expect(link).toBeTruthy();
  expect(link.getAttribute("href")).toBe("#/company/people/ada");
  // THE HANDLE IS STILL REACHABLE, on the hover title — it is what an operator
  // types into a filter, so resolving the name must not destroy it.
  expect(link.getAttribute("title")).toBe("@ada");
  expect(screen.queryByText("ada")).toBeNull();
});

// A HANDLE THE CHART HAS NO SEAT FOR IS STILL A PERSON. A task filed by a seat
// the company has since removed names a handle nothing resolves, and the cell
// draws the handle rather than a blank: "somebody the chart no longer has" is
// a fact, and an empty cell reads as "nobody holds this", which is a different
// one the same column already has its own mark for.
test("a handle the chart cannot resolve is drawn as the handle", () => {
  mount("table", [row({ assignee: "departed" })]);
  expect(screen.getByText("departed")).toBeTruthy();
});

// AND NOBODY IS A STATE, not a blank. An unassigned item routes to the
// project's lead, and a project with no lead routes to nobody at all.
test("an unassigned row says nobody holds it", () => {
  const { container } = mount("table", [row({})]);
  const marks = [...container.querySelectorAll(".crewlet-empty-value")].filter((el) =>
    (el.textContent ?? "").includes("Nobody holds this"),
  );
  expect(marks.length).toBe(1);
});

// THE LIST DRAWS THE BADGE ALONE, and the badge still carries the resolved
// name — the compact row spends its width on the title, so the name is the
// avatar's identity rather than a second column of it. Drawn from the HANDLE
// the initials would be "d" for every seat whose handle starts with one.
test("the list's assignee resolves the name even where it prints no name", () => {
  const { container } = mount("list", [row({ assignee: "ada" })]);
  const titled = container.querySelector('[title="Ada Okonkwo"]');
  expect(titled).toBeTruthy();
});

// A REMOVAL BY AN AGENT WEARS NO KIND TAG, and this is the case the column
// existed to separate from. `agent` is `tracker.AuthorAgent`, one of the four
// the engine mints (`agent`/`human`/`operator`/`system`) — there is no `seat`
// among them, so the word this cell used to check against matched nothing and
// the tag was drawn on EVERY row of the trash, which separates nothing.
test("an ordinary agent removal draws the name and no kind tag", () => {
  const removals = new Map([["t-1", removal()]]);
  mount("table", [row({})], removals);
  expect(screen.getByText("Ada Okonkwo")).toBeTruthy();
  expect(screen.queryByText("agent")).toBeNull();
  // SCOPED TO THE CELL, because the row carries a status badge that is a tag
  // too: a container-wide "no tag" would go red for the status and green for
  // a kind tag that had simply moved.
  expect(whoCell().querySelector(".crewlet-tag")).toBeNull();
});

// AND A REMOVAL BY ANYTHING ELSE SAYS WHICH. An assistant removing a subtree
// and a person removing one task look identical without it, which is the
// question a trash screen is opened with.
test("an operator's removal is marked as one, beside the name", () => {
  const removals = new Map([["t-1", removal({ actor: "ada", actor_kind: "operator" })]]);
  mount("table", [row({})], removals);
  expect(screen.getByText("Ada Okonkwo")).toBeTruthy();
  expect(screen.getByText("operator")).toBeTruthy();
});

// ONE SEAT LOOKS LIKE ONE SEAT ON BOTH COLUMN SETS. The dashed ring is the
// only variant an identity badge has and it is STRUCTURAL — a human seat, the
// engine does not run it — so it cannot depend on which set is drawing. It
// did: `SeatCell` took a kind and the compact `Assignee` had no way to be
// handed one, so the same person was drawn two ways on one grid.
test("a human seat wears the dashed ring on both column sets", () => {
  for (const shape of ["list", "table"] as const) {
    const { container } = mount(shape, [row({ assignee: "iris" })]);
    expect(badge(container).className).toContain("dashed");
    cleanup();
  }
});

// AND AN AGENT DOES NOT, which is what makes the ring worth drawing: a mark
// every row wears separates nothing, the same rule `normal` priority keeps.
test("an agent seat wears no ring on either column set", () => {
  for (const shape of ["list", "table"] as const) {
    const { container } = mount(shape, [row({ assignee: "ada" })]);
    expect(badge(container).className).not.toContain("dashed");
    cleanup();
  }
});

// A HANDLE THE CHART DOES NOT HOLD IS NOT AN AGENT. It is a seat that was
// renamed or removed, and the honest badge is the neutral disc rather than a
// ring claiming the chart said something it did not.
test("a handle the chart does not hold falls back to the neutral disc", () => {
  for (const shape of ["list", "table"] as const) {
    const { container } = mount(shape, [row({ assignee: "departed" })]);
    expect(badge(container).className).not.toContain("dashed");
    cleanup();
  }
});

// A ROW IS A REAL ANCHOR, so ⌘-click and middle-click open the item's own
// page. A plain click opens the PEEK instead, because the list is the place
// the reader is — and it is the one gesture that has to call
// `preventDefault`, or the click pushes `peek=` and then follows the href,
// destroying the panel it just opened. The board card and the Projects
// directory both went through `peekRow` for exactly that; this grid passed a
// bare handler beside its `rowHref` and navigated away on every plain click,
// on BOTH column sets.
test("a plain click on a row peeks and a modified click follows the link", () => {
  for (const shape of ["list", "table"] as const) {
    const onOpen = vi.fn();
    const { container } = mount(shape, [row({})], undefined, onOpen);
    const anchor = container.querySelector("a.row-link") as HTMLAnchorElement;
    expect(anchor).toBeTruthy();
    expect(anchor.getAttribute("href")).toBe("#/work/ENG-9");

    const plain = new MouseEvent("click", { bubbles: true, cancelable: true });
    anchor.dispatchEvent(plain);
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(plain.defaultPrevented).toBe(true);

    // AND A MODIFIED CLICK IS LEFT ALONE, or the affordance is a lie: the row
    // looks like a link, so every way a browser opens one elsewhere has to
    // keep working.
    const meta = new MouseEvent("click", { bubbles: true, cancelable: true, metaKey: true });
    anchor.dispatchEvent(meta);
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(meta.defaultPrevented).toBe(false);
    cleanup();
  }
});

// THE TRASH'S `removed_by` IS THE THIRD CELL ON THIS GRID THAT DRAWS A PERSON,
// and it reads the same resolver: somebody who emptied a subtree is drawn the
// way they are drawn on the row above.
test("a human removal wears the ring in the trash column", () => {
  const removals = new Map([["t-1", removal({ actor: "iris", actor_kind: "human" })]]);
  const { container } = mount("table", [row({})], removals);
  expect(badge(container).className).toContain("dashed");
});

/** The grid's rows, in the order they are drawn. */
const drawnRows = (container: HTMLElement) =>
  Array.from(container.querySelectorAll<HTMLElement>(".grid-row"));

// TWO TASKS CAN HOLD ONE KEY, and they are still two rows. A key is the
// tracker's ADDRESS, not its identity: a counter restored beside tasks minted
// after it mints numbers those tasks already hold, the applier writes what the
// record says rather than stall the log, and `key_collision` is the attention
// flag that lists exactly those tasks — together, on this grid. Keyed on the
// key, React met two children under one key and reconciled them by place, so
// whatever a row holds that the answer does not (here the focus a reader left
// on it) stayed where the row WAS when the next answer ordered them the other
// way. Keyed on the id, the one thing two tasks never share, it travels with
// its task — as it already did on the board, the calendar and the timeline.
test("two tasks holding one key are two rows, each keeping its own state", () => {
  const original = row({ id: "t-1", title: "the original" });
  const restored = row({ id: "t-2", title: "minted after the restore" });
  for (const shape of ["list", "table"] as const) {
    const { container, rerender } = render(grid(shape, [original, restored]));
    expect(drawnRows(container)).toHaveLength(2);

    const link = drawnRows(container)[0]!.querySelector<HTMLAnchorElement>("a.row-link")!;
    link.focus();
    expect(document.activeElement).toBe(link);

    // The next answer orders the same two tasks the other way round.
    rerender(grid(shape, [restored, original]));
    const [top, below] = drawnRows(container);
    expect(top!.textContent).toContain("minted after the restore");
    expect(below!.textContent).toContain("the original");
    expect((document.activeElement as HTMLElement).closest(".grid-row")).toBe(below);
    cleanup();
  }
});

// AND IN THE TRASH, THE CALL THAT BRINGS ONE BACK NAMES THAT ONE. The copy
// button carried `restore_work_item` on the row's key, and a key two tasks
// hold opens the one that claimed it first — so the call copied off the
// duplicate's row would restore the claimant, which is not in the trash, and
// leave the task a reader meant where it was.
test("the restore call for a removed duplicate names it by its id", () => {
  const [claimant, duplicate] = collidingRows();
  const removals = new Map([
    [CLAIMANT, removal({ subject_id: CLAIMANT, subject_key: SHARED_KEY })],
    [DUPLICATE, removal({ subject_id: DUPLICATE, subject_key: SHARED_KEY })],
  ]);
  const { container } = mount("table", [claimant, duplicate], removals);
  const call = (title: string) =>
    drawnRows(container)
      .find((el) => el.textContent?.includes(title))
      ?.querySelector("button")
      ?.getAttribute("title");
  expect(call(DUPLICATE_TITLE)).toBe(`restore_work_item {"item":"${DUPLICATE}"}`);
  expect(call(CLAIMANT_TITLE)).toBe(`restore_work_item {"item":"${SHARED_KEY}"}`);
});
