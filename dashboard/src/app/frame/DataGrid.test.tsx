/**
 * The one rule a grid row's own link has to obey: it may not be an ancestor
 * of the links inside it.
 *
 * The row used to BE the `<a>` whenever `rowHref` was given, and cells link
 * all the time — `SeatCell` goes to a seat, `KeyCell` to a work item — so
 * every such grid shipped `<a>` inside `<a>`. The HTML parser does not nest
 * them: it CLOSES the outer one at the inner, which left the row link
 * covering the cells before the first seat chip and nothing after it. Both
 * halves of the row looked identical and one of them did nothing, which is
 * why this went unnoticed through a redesign — it is invisible in a
 * screenshot and silent in a type checker.
 *
 * Asserted through the DOM rather than by reading the component, because the
 * failure IS a DOM shape: a component that renders `<a>` inside `<a>` is
 * perfectly well-typed React.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, expect, test } from "vitest";

import { DataGrid } from "./DataGrid.tsx";
import { Router } from "~/app/router.tsx";

afterEach(cleanup);

beforeAll(() => {
  // jsdom implements no layout and so no scrolling, and the cursor keys below
  // scroll the row they land on into view. Stubbed here rather than in the
  // shared setup: it is a fact about what these cases exercise, not a gap
  // every suite has.
  Element.prototype.scrollIntoView = () => {};
});

interface Row {
  id: string;
  who: string;
}

const ROWS: Row[] = [
  { id: "one", who: "ceo" },
  { id: "two", who: "cto" },
];

// THE ROUTER IS REAL, not a stub: `DataGrid` reads `sort=` and `cols=` off
// the URL through `useParam`, and a grid without one throws before it renders
// a row.
function grid(onRowActivate?: (row: Row) => void) {
  return render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        rowHref={(r) => `#/activity/turns/${r.id}`}
        onRowActivate={onRowActivate}
        columns={[
          { key: "id", header: "Id", cell: (r) => r.id },
          {
            key: "who",
            header: "Who",
            // The shape every linking cell has.
            cell: (r) => <a href={`#/company/people/${r.who}`}>{r.who}</a>,
          },
        ]}
      />
    </Router>,
  );
}

test("a row's link never contains a cell's link", () => {
  const { container } = grid();
  expect(container.querySelectorAll("a a")).toHaveLength(0);
  // And the row still HAS one: dropping `rowHref` would also satisfy the
  // assertion above, and would take ⌘-click, middle-click and "copy link
  // address" with it.
  const links = container.querySelectorAll<HTMLAnchorElement>("a.row-link");
  expect(links).toHaveLength(ROWS.length);
  expect(links[0]?.getAttribute("href")).toBe("#/activity/turns/one");
});

test("the row's link is named by the row, not left unnamed", () => {
  // A stretched link has no text of its own, and an unnamed link is what a
  // screen reader announces as "link". The row it points at is the name the
  // anchor row carried before.
  const { container } = grid();
  const link = container.querySelector<HTMLAnchorElement>("a.row-link");
  const named = link?.getAttribute("aria-labelledby");
  expect(named).toBeTruthy();
  expect(container.querySelector(`#${CSS.escape(named ?? "")}`)?.className).toContain("grid-row");
});

test("a plain click on the row still activates it", () => {
  // The overlay carries the click handler, so a reader clicking anywhere on a
  // row that is not one of its own links gets the row's own gesture — which
  // is what opens a peek.
  const seen: string[] = [];
  const { container } = grid((r) => seen.push(r.id));
  const link = container.querySelector<HTMLAnchorElement>("a.row-link");
  if (link) fireEvent.click(link);
  expect(seen).toEqual(["one"]);
});

test("a grid with no row link renders no overlay at all", () => {
  // `rowHref` is what mints it. A grid whose rows are not addressable must
  // not get a transparent anchor over every row swallowing its clicks.
  render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[{ key: "id", header: "Id", cell: (r) => r.id }]}
      />
    </Router>,
  );
  expect(screen.queryAllByRole("link")).toHaveLength(0);
});

/**
 * AND THE KEYBOARD BELONGS TO ONE GRID AT A TIME.
 *
 * `j`, `k` and `enter` are bound on `window` — a grid whose rows are anchors
 * takes no focus of its own, so there is nothing else to bind them to. Several
 * screens carry two: the spend by seat over the recent turns, a page's grid
 * under one in the peek rail. Unscoped, both handlers ran on every keystroke —
 * one `j` moved two cursors and one Enter opened a seat peek and then a turn
 * peek over it, so the object the reader got was never the one their cursor
 * was on.
 */
function twoGrids(seen: string[]) {
  const columns = [{ key: "id", header: "Id", cell: (r: Row) => r.id }];
  return render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        name="upper"
        rowKey={(r) => r.id}
        onRowActivate={(r) => seen.push(`upper:${r.id}`)}
        columns={columns}
      />
      <DataGrid<Row>
        rows={ROWS}
        name="lower"
        rowKey={(r) => r.id}
        onRowActivate={(r) => seen.push(`lower:${r.id}`)}
        columns={columns}
      />
    </Router>,
  );
}

// THE TRACK LIST IS DECLARED ONCE, ON THE GRID.
//
// The head and every row were grid containers of their own, each handed the
// same track list as an inline string — which looks like one layout and is
// not: an intrinsic track resolves against the content of ITS OWN container,
// so every row sized its columns against its own cells alone. Measured against
// a running engine at 1600px, the work table's fifth column started at 78.4px
// in the head and 74px in the rows, and the audit's last column sat 245px from
// the heading that named it.
//
// jsdom computes no layout, so the drift itself cannot be asserted here — what
// can is the shape that caused it: a second declaration. The geometry is
// asserted where it is visible, in `styles/frame.test.ts`, which holds the
// subgrid chain the wrap's tracks reach a cell through.
test("the columns are declared on the grid, not copied onto every row", () => {
  const { container } = grid();
  const wrap = container.querySelector<HTMLElement>(".grid-wrap")!;
  expect(wrap.style.gridTemplateColumns).toContain("minmax(0, 1fr)");
  const heads = container.querySelectorAll<HTMLElement>(".grid-head");
  const rows = container.querySelectorAll<HTMLElement>(".grid-row");
  expect(heads).toHaveLength(1);
  expect(rows.length).toBeGreaterThan(1);
  for (const el of [...heads, ...rows]) expect(el.style.gridTemplateColumns).toBe("");
});

test("one keystroke activates one grid, not every grid on the screen", () => {
  const seen: string[] = [];
  twoGrids(seen);
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  // The first grid mounted drives, which on every screen that has one is the
  // primary grid: a reader who has clicked nothing keeps what they had.
  expect(seen).toEqual(["upper:one"]);
});

test("the grid the reader last touched is the one the keyboard drives", () => {
  const seen: string[] = [];
  const { container } = twoGrids(seen);
  const wraps = container.querySelectorAll<HTMLElement>(".grid-wrap");
  expect(wraps).toHaveLength(2);
  fireEvent.pointerDown(wraps[1]!);
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  // Two `j`s in the lower grid alone — the upper one's cursor never moved, so
  // an Enter that reached it would have activated nothing at all and this
  // assertion would pass on a second row it never touched.
  expect(seen).toEqual(["lower:two"]);
});

// A COLUMN HEAD WITH NO ORDER TO ASK FOR IS NOT A BUTTON.
//
// Every head used to be one, `disabled` where the column had no `sortValue`.
// That is the right BEHAVIOUR — the click does nothing — and the wrong
// element: a disabled button is still a button in the accessibility tree, so
// the heads whose label is a glyph or nothing at all (a type mark, a row's
// restore action) were announced as "button" with no name. Every grid in the
// product that carries such a column shipped one unnamed control per column.
//
// ASSERTED THROUGH THE ROLES, because the failure is an accessibility-tree
// shape: a `<button disabled>` with an empty label is perfectly good React and
// perfectly good CSS, and it is invisible in a screenshot.
test("only a sortable column head is a button, and every button is named", () => {
  render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          { key: "id", header: "Id", sortValue: (r) => r.id, cell: (r) => r.id },
          // The two shapes that had no name: a glyph column and an action
          // column, both headed by nothing.
          { key: "mark", header: "", cell: () => <span>·</span> },
          { key: "who", header: "Who", cell: (r) => r.who },
        ]}
      />
    </Router>,
  );

  const heads = screen.getAllByRole("columnheader");
  const buttons = screen.getAllByRole("button");
  // ONE BUTTON, and it is the sortable column.
  expect(buttons.map((b) => (b.textContent ?? "").trim())).toEqual(["Id"]);
  // AND THE OTHER TWO ARE STILL COLUMN HEADS, rather than being dropped: the
  // fix must not take the heading out of the tree along with the button.
  expect(heads.length).toBe(2);
  for (const button of buttons) {
    expect((button.getAttribute("aria-label") ?? button.textContent ?? "").trim()).not.toBe("");
  }
});
