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
import { afterEach, expect, test } from "vitest";

import { DataGrid } from "./DataGrid.tsx";
import { Router } from "~/app/router.tsx";

afterEach(cleanup);

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
          { key: "id", label: "Id", cell: (r) => r.id },
          {
            key: "who",
            label: "Who",
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
        columns={[{ key: "id", label: "Id", cell: (r) => r.id }]}
      />
    </Router>,
  );
  expect(screen.queryAllByRole("link")).toHaveLength(0);
});
