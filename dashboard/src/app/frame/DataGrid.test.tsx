/**
 * The dashboard's grid binding: the kit's `DataGrid` (its own suite holds the
 * row link, the tracks, the heads, the bands and the fit) reading its sort and
 * columns off the address, marking the row the peek is open on, and handing
 * the keyboard to one grid at a time.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, test } from "vitest";

import { DataGrid } from "./DataGrid.tsx";
import { Router } from "~/app/router.tsx";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

beforeAll(() => {
  // jsdom implements no scrolling, and the cursor keys scroll the row they
  // land on into view.
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

const COLUMNS = [
  { key: "id", header: "Id", sortValue: (r: Row) => r.id, cell: (r: Row) => r.id },
  { key: "who", header: "Who", sortValue: (r: Row) => r.who, cell: (r: Row) => r.who },
];

// THE ROUTER IS REAL: the binding reads `sort=` and `cols=` off the URL.
function grid() {
  return render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        rowHref={(r) => `#/live/turns/${r.id}`}
        columns={COLUMNS}
      />
    </Router>,
  );
}

const heads = (container: HTMLElement) =>
  [...container.querySelectorAll('[role="columnheader"]')].map((el) => el.textContent);
const order = (container: HTMLElement) =>
  [...container.querySelectorAll(".crewlet-data-grid__row")].map(
    (row) => row.querySelector(".crewlet-data-grid__cell")?.textContent,
  );

// THE ROW THE RAIL IS OPEN ON IS MARKED, by comparing addresses: a row's link
// is its object's page, and the peek names an object whose page is `peekHref`.
describe("the peeked row", () => {
  const marked = (container: HTMLElement) =>
    [...container.querySelectorAll(".crewlet-data-grid__row--selected")].map((row) =>
      row.querySelector("a.crewlet-data-grid__row-link")?.getAttribute("href"),
    );

  test("is the row whose link is the peek's page, and it is announced as current", () => {
    location.hash = "#/live/turns?peek=turn:two";
    const { container } = grid();
    expect(marked(container)).toEqual(["#/live/turns/two"]);
    const links = [...container.querySelectorAll("a.crewlet-data-grid__row-link")];
    expect(links.map((a) => a.getAttribute("aria-current"))).toEqual([null, "true"]);
  });

  test("is no row when the peek is an object no row links to, or when none is open", () => {
    location.hash = "#/live/turns?peek=seat:two";
    const first = grid();
    expect(marked(first.container)).toEqual([]);
    cleanup();
    location.hash = "#/live/turns";
    expect(marked(grid().container)).toEqual([]);
  });
});

// THE SORT AND THE COLUMN SET ARE IN THE URL, so a sorted list can be sent,
// survives a reload and comes back sorted from Back.
describe("the address", () => {
  test("holds the sort a head asks for, and the rows follow it", () => {
    location.hash = "#/x?sort=-id";
    const { container } = grid();
    expect(order(container)).toEqual(["two", "one"]);
    fireEvent.click(screen.getByRole("button", { name: "Who" }));
    expect(location.hash).toContain("sort=who");
  });

  // THE ADDRESS HOLDS ONLY WHAT THE READER CHOSE. Read with the default as
  // its fallback, a press on a column at its own descending default wrote the
  // default back and changed nothing; now the head steps from the order in
  // force and a step back to the default is no key at all.
  test("steps from a default sort and writes no key for the default", () => {
    location.hash = "#/x";
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={COLUMNS} defaultSort="-id" />
      </Router>,
    );
    expect(order(container)).toEqual(["two", "one"]);
    const idHead = () => container.querySelectorAll('[role="columnheader"]')[0]!;
    expect(idHead().getAttribute("aria-sort")).toBe("descending");
    fireEvent.click(screen.getByRole("button", { name: "Id" }));
    expect(location.hash).toContain("sort=id");
    expect(order(container)).toEqual(["one", "two"]);
    fireEvent.click(screen.getByRole("button", { name: "Id" }));
    expect(location.hash).not.toContain("sort=");
    expect(idHead().getAttribute("aria-sort")).toBe("descending");
  });

  test("keys a second grid's sort by its name, and its columns by colsName", () => {
    location.hash = "#/x?cols.list=who";
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} colsName="list" rowKey={(r) => r.id} columns={COLUMNS} />
      </Router>,
    );
    // THE COLUMN SET CAME OFF `cols.list=`, which a bare `cols=` would not have answered.
    expect(heads(container)).toEqual(["Who"]);
    // AND THE SORT IS STILL THE BARE KEY, which `name` would have moved with it.
    fireEvent.click(screen.getByRole("button", { name: "Who" }));
    expect(location.hash).toContain("sort=who");
    expect(location.hash).not.toContain("sort.list=");
    // The way back clears the key.
    fireEvent.click(screen.getByRole("button", { name: "Show every column" }));
    expect(location.hash).not.toContain("cols.list=");
  });
});

/**
 * AND THE KEYBOARD BELONGS TO ONE GRID AT A TIME. `j`, `k` and `enter` are
 * bound on `window`, and several screens carry two grids: unscoped, one `j`
 * moved two cursors and one Enter opened two peeks.
 */
function twoGrids(seen: string[]) {
  return render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        name="upper"
        rowKey={(r) => r.id}
        onRowActivate={(r) => seen.push(`upper:${r.id}`)}
        columns={COLUMNS}
      />
      <DataGrid<Row>
        rows={ROWS}
        name="lower"
        rowKey={(r) => r.id}
        onRowActivate={(r) => seen.push(`lower:${r.id}`)}
        columns={COLUMNS}
      />
    </Router>,
  );
}

test("one keystroke activates one grid, not every grid on the screen", () => {
  const seen: string[] = [];
  twoGrids(seen);
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  // The first grid mounted drives: a reader who has clicked nothing keeps it.
  expect(seen).toEqual(["upper:one"]);
});

test("the grid the reader last touched is the one the keyboard drives", () => {
  const seen: string[] = [];
  const { container } = twoGrids(seen);
  const grids = container.querySelectorAll<HTMLElement>(".crewlet-data-grid");
  expect(grids).toHaveLength(2);
  fireEvent.pointerDown(grids[1]!);
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  // Two `j`s in the lower grid alone; an Enter reaching the upper grid would
  // have activated nothing, its cursor never moved.
  expect(seen).toEqual(["lower:two"]);
});

// A GRID OF LINKS OPENS A ROW FROM THE KEYBOARD TOO: Enter on the cursor row
// follows its link, where the open key used to need a handler the grid of a
// directory no longer has.
test("Enter follows the cursor row's link in a grid whose rows only link", () => {
  location.hash = "#/live/turns";
  grid();
  // THE LINK IS CLICKED, as a pointer would click it; the browser follows it
  // (jsdom implements no navigation, so the click is what is observed).
  const followed: (string | null)[] = [];
  const watch = (event: MouseEvent) => {
    followed.push((event.target as Element).closest("a")?.getAttribute("href") ?? null);
    event.preventDefault();
  };
  document.addEventListener("click", watch);
  try {
    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "Enter" });
  } finally {
    document.removeEventListener("click", watch);
  }
  expect(followed).toEqual(["#/live/turns/one"]);
});

test("an empty grid names its mark, and drives no keystroke", () => {
  const seen: string[] = [];
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={[]}
        rowKey={(r) => r.id}
        onRowActivate={(r) => seen.push(r.id)}
        columns={COLUMNS}
        empty={{ title: "Nothing yet", icon: "inbox" }}
      />
    </Router>,
  );
  expect(screen.getByText("Nothing yet")).toBeTruthy();
  expect(container.querySelector(".crewlet-empty-state svg")).not.toBeNull();
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  expect(seen).toEqual([]);
});
