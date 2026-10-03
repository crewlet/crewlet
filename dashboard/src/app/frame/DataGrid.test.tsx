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

import { Profiler, useState } from "react";
import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";

import { DataGrid, fitColumns } from "./DataGrid.tsx";
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

/**
 * A mark that draws nothing for a value it has no mark for, in the shape every
 * one of them takes: a COMPONENT that returns null, so the grid holds an
 * element and the DOM holds no child. `PriorityMark` is the real one (null for
 * `none` and an absent priority — see `components/work.tsx`), and this stands
 * in for it so the case says what it is about rather than importing a tracker
 * into the frame's own suite.
 */
function Nothing() {
  return null;
}

// THE ROUTER IS REAL, not a stub: `DataGrid` reads `sort=` and `cols=` off
// the URL through `useParam`, and a grid without one throws before it renders
// a row.
function grid(onRowActivate?: (row: Row) => void) {
  return render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        rowHref={(r) => `#/live/turns/${r.id}`}
        onRowActivate={onRowActivate}
        columns={[
          { key: "id", header: "Id", cell: (r) => r.id },
          {
            key: "who",
            header: "Who",
            // The shape every linking cell has.
            cell: (r) => <a href={`#/agents/seats/${r.who}`}>{r.who}</a>,
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
  expect(links[0]?.getAttribute("href")).toBe("#/live/turns/one");
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

// THE ROW THE RAIL IS OPEN ON IS MARKED, in every grid, by comparing the row's
// link with the peek's page. Four screens spelled the mark for themselves and
// the rest never did, so beside a peek on Nodes, Secrets, a seat's turns or a
// spend table nothing said which row the rail described. The mark is said as
// well as painted: the row's link carries `aria-current`.
describe("the peeked row", () => {
  afterEach(() => {
    location.hash = "#/";
  });

  const marked = (container: HTMLElement) =>
    [...container.querySelectorAll(".grid-row.selected")].map((row) =>
      row.querySelector("a.row-link")?.getAttribute("href"),
    );

  test("is the row whose link is the peek's page, and it is announced as current", () => {
    location.hash = "#/live/turns?peek=turn:two";
    const { container } = grid();
    expect(marked(container)).toEqual(["#/live/turns/two"]);
    const links = [...container.querySelectorAll("a.row-link")];
    expect(links.map((a) => a.getAttribute("aria-current"))).toEqual([null, "true"]);
  });

  test("is no row when the peek is an object no row links to", () => {
    location.hash = "#/live/turns?peek=seat:two";
    const { container } = grid();
    expect(marked(container)).toEqual([]);
    expect(container.querySelector("[aria-current]")).toBeNull();
  });

  test("is no row when no peek is open", () => {
    location.hash = "#/live/turns";
    const { container } = grid();
    expect(marked(container)).toEqual([]);
  });
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

// A CELL CARRIES ITS COLUMN'S NAME, SO A PHONE CAN LABEL IT.
//
// Below the drawer breakpoint the column heads go and each cell is drawn as a
// labelled line — a nine-column table in a 390px card clips six of them with
// nothing to scroll, because the wrap is `overflow: clip` and a scroller would
// resolve every `minmax(0, 1fr)` track to zero. The label comes from
// `data-label` via `::before`, so the attribute is the whole mechanism.
//
// jsdom computes no layout and applies no media query, so what is asserted
// here is the ATTRIBUTE; the rules that read it are asserted in
// styles/frame.test.ts.
test("a cell carries its column's name, and only when that name is a word", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          // A COLUMN THAT HAS BOTH DRAWS ITS HEAD. `label` is the word a head
          // that cannot hold one supplies, never an override of one that can —
          // two names for one column is how a table and its cards start
          // disagreeing about what a value is.
          { key: "id", header: "Id", label: "Identifier", cell: (r) => r.id },
          // A GLYPH HEAD AND AN EMPTY ONE cannot supply the word — `attr()`
          // reads text — so the column says it separately. `label` is what a
          // twenty-pixel column has instead of a head, and it is drawn ONLY
          // here: the head row keeps the glyph.
          { key: "mark", header: <span>·</span>, label: "Mark", cell: () => <span>·</span> },
          { key: "act", header: "", label: "Actions", cell: () => <span>+</span> },
          // AND A COLUMN WITH NEITHER draws nothing rather than an empty line
          // per row. `source.test.ts` is what stops one reaching a screen.
          { key: "none", header: "", cell: () => <span>-</span> },
        ]}
      />
    </Router>,
  );
  const cells = [...container.querySelectorAll<HTMLElement>(".grid-row > .grid-cell")];
  expect(cells.length).toBe(ROWS.length * 4);
  const label = (key: string): (string | null)[] =>
    cells
      .filter((_, at) => at % 4 === ["id", "mark", "act", "none"].indexOf(key))
      .map((c) => c.getAttribute("data-label"));
  expect(label("id")).toEqual(ROWS.map(() => "Id"));
  // THE HEAD WINS WHERE IT IS A WORD, so a column never repeats itself, and
  // `label` fills in only where the head cannot.
  expect(label("mark")).toEqual(ROWS.map(() => "Mark"));
  expect(label("act")).toEqual(ROWS.map(() => "Actions"));
  expect(label("none")).toEqual(ROWS.map(() => null));

  // AND THE HEAD ROW IS UNTOUCHED BY ANY OF IT: `label` is the card's word,
  // not a second head, so a glyph column still draws its glyph up there.
  const heads = [...container.querySelectorAll<HTMLElement>(".grid-head > *")];
  expect(heads.map((h) => h.textContent)).toEqual(["Id", "·", "", ""]);
});

// A CELL WITH NO VALUE HAS NO CHILD NODES, WHICH IS WHAT THE CARD DROPS IT ON.
//
// A column draws no value on a row that has none — `PriorityMark` renders null
// for `none`, which a task nobody prioritised carries, and every mark whose
// rule is "nothing is drawn for no value" does the same. In the table that is an
// empty track under a head, which is correct and is what keeps the row's
// columns lined up. Below 860px the head is gone and the label is the CELL's
// own, so the card opened with `PRIORITY` on a line by itself, on every
// row that had none.
//
// `.grid-cell:empty { display: none }` in frame.css is the fix, because a
// container cannot ask a child that drew nothing whether it did and the
// element has to stay in the DOM regardless — the wide layout's tracks are
// positional, so dropping it would move every later value one column left.
// What THIS file owes that rule is its precondition: an empty cell really is
// childless, which a mark wrapped in an always-rendered span would defeat
// silently. The rule that reads it is asserted in styles/frame.test.ts, since
// jsdom applies no media query and computes no layout.
test("a cell with no value is childless, and keeps its column's name", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          { key: "id", header: "Id", cell: (r) => r.id },
          // THE MARK THAT DRAWS NOTHING FOR THE DEFAULT, in the shape every
          // one of them takes: a component that returns null.
          { key: "prio", header: "", label: "Priority", cell: () => <Nothing /> },
          // AND A MARKED ABSENCE IS CONTENT. An em dash is a value somebody
          // reads — "nobody holds this" — so its line stays.
          { key: "who", header: "Who", cell: () => <span>—</span> },
        ]}
      />
    </Router>,
  );
  const cells = [...container.querySelectorAll<HTMLElement>(".grid-row > .grid-cell")];
  const prio = cells.filter((_, at) => at % 3 === 1);
  const who = cells.filter((_, at) => at % 3 === 2);
  expect(prio).toHaveLength(ROWS.length);
  for (const cell of prio) {
    expect(cell.childNodes).toHaveLength(0);
    expect(cell.matches(":empty")).toBe(true);
  }
  for (const cell of who) expect(cell.matches(":empty")).toBe(false);

  // AND THE COLUMN STILL SAYS ITS NAME, because the table layout is untouched:
  // the head keeps its column and the cell keeps the word the card would draw
  // on a row that HAS a priority.
  expect(prio.map((c) => c.getAttribute("data-label"))).toEqual(ROWS.map(() => "Priority"));
  const heads = [...container.querySelectorAll<HTMLElement>(".grid-head > *")];
  expect(heads.map((h) => h.textContent)).toEqual(["Id", "", "Who"]);
});

// A SHRINK COLUMN CANNOT STARVE THE ONES THE LIST IS FOR.
//
// `max-content` is not "shrink to content" — it is GROW to content, with no
// ceiling, and grid resolves it before it gives anything to a `minmax(0, 1fr)`.
// So one long value in a narrow column takes whatever it likes and every
// flexible column collapses to ZERO.
//
// Measured against a running engine, and it is the flexible columns that go:
// the schedules grid in an 822px content column drew Wakes at 502px with Name
// and Task at 0px, and still ran to 1139px inside a box that clips. The work
// table at 1000px drew Assignee at 220px with TITLE at zero — a tracker whose
// title column is invisible.
//
// jsdom computes no layout, so what is asserted is the TEMPLATE: the string
// the wrap is handed, which is where the decision is.
test("a shrink column is capped and a flexible one is not", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          { key: "id", header: "Id", shrink: true, cell: (r) => r.id },
          { key: "title", header: "Title", cell: (r) => r.id },
          // AN EXPLICIT WIDTH STILL WINS, because a caller that named a number
          // has already answered this question.
          { key: "fixed", header: "Fixed", shrink: true, width: "7rem", cell: (r) => r.id },
        ]}
      />
    </Router>,
  );
  const wrap = container.querySelector<HTMLElement>(".grid-wrap");
  const template = wrap?.style.gridTemplateColumns ?? "";

  // THE CAP IS A FRACTION rather than a pixel count: it has to mean the same
  // thing on a phone and on a wide screen, where a pixel cap is a desktop's
  // proportions pinned onto a laptop.
  expect(template).toMatch(/fit-content\(\d+%\)/);
  // AND `fit-content`, not `max-content`: a column under the cap keeps sizing
  // to its own content and only one that would take more gives way.
  expect(template).not.toMatch(/max-content/);
  // The flexible column keeps its floor of zero — it is the one that may give
  // way, and a floor here would make the grid overflow instead.
  expect(template).toContain("minmax(0, 1fr)");
  expect(template).toContain("7rem");
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

// AND A SORTABLE HEAD WHOSE HEAD IS NOT A WORD IS STILL NAMED.
//
// The rule above takes the unsortable glyph heads out of the button role. What
// it cannot do is stop a SORTABLE column being declared with a glyph or an
// empty head — an ordinary thing to want, since such a column already carries
// its word in `label` for the card layout — and that renders a control whose
// entire accessible name is the sort arrow.
test("a sortable glyph head takes its name from the column's label", () => {
  render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          { key: "mark", header: "", label: "Priority", sortValue: (r) => r.id, cell: () => "·" },
        ]}
      />
    </Router>,
  );
  expect(screen.getByRole("button", { name: "Priority" })).toBeTruthy();
});

// A BAND INSIDE A BAND, because a second grouping is a heading under a
// heading. A grid that knew only `rows` drew one EMPTY band per group over a
// twice-grouped answer, with every row of it nowhere on the screen — which is
// what the work table did for as long as its Display menu offered a second
// axis.
test("a band's own bands are drawn, and it counts what is under them", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        bands={[
          {
            key: "todo",
            label: "To do",
            total: 5,
            rows: [],
            bands: [{ key: "ada", label: "Ada", total: 2, rows: ROWS }],
          },
        ]}
        rowKey={(r) => r.id}
        columns={[{ key: "id", header: "Id", cell: (r) => r.id }]}
      />
    </Router>,
  );
  const heads = [...container.querySelectorAll(".grid-band-head")].map((el) => el.textContent);
  expect(heads).toEqual(["To do2 of 5", "Ada2"]);
  // THE ROWS ARE UNDER THE SUB-BAND, and they are the grid's rows: a cursor
  // that could not reach them would step over half a screen.
  expect(container.querySelectorAll(".grid-row").length).toBe(2);
  expect([...container.querySelectorAll(".grid-row")].map((el) => el.textContent)).toEqual([
    "one",
    "two",
  ]);
});

// AND A GROUPED ANSWER WITH NO ROWS ON THIS PAGE IS NOT AN EMPTY ANSWER. A
// band carries the engine's count over its whole group and a bounded slice of
// rows, so "Nothing matches" drawn over a band that says three exist is a
// second and false answer on the same screen.
test("a band with no rows on this page still draws its heading", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        bands={[{ key: "ada", label: "Ada", total: 3, rows: [] }]}
        rowKey={(r) => r.id}
        columns={[{ key: "id", header: "Id", cell: (r) => r.id }]}
        empty={{ title: "Nothing matches" }}
      />
    </Router>,
  );
  expect(container.querySelector(".grid-band-head")?.textContent).toBe("Ada0 of 3");
  expect(screen.queryByText("Nothing matches")).toBeNull();
});

// THE SORT KEY AND THE COLUMN KEY ARE TWO QUESTIONS.
//
// `sort=` is a fact about the QUESTION — on a server-sorted list the key goes
// to the engine, which orders the whole set the same way whatever draws it —
// and `cols=` is a fact about the DRAWING. The work screen is where they come
// apart: its list and its table are one grid with two column sets, so the
// order is shared between them and the column arrangement is not. `name` alone
// could not say that, because it keys both.
test("colsName keys the columns without moving the sort key", () => {
  location.hash = "#/x?cols.list=who";
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        colsName="list"
        rowKey={(r) => r.id}
        columns={[
          { key: "id", header: "Id", sortValue: (r) => r.id, cell: (r) => r.id },
          { key: "who", header: "Who", sortValue: (r) => r.who, cell: (r) => r.who },
        ]}
      />
    </Router>,
  );
  // THE COLUMN NARROWING CAME OFF `cols.list=`, which a bare `cols=` would not
  // have answered.
  expect([...container.querySelectorAll(".grid-th")].map((el) => el.textContent)).toEqual(["Who"]);
  // AND THE SORT IS STILL THE BARE KEY, which is the half `name` would have
  // moved with it.
  fireEvent.click(screen.getByRole("button", { name: "Who" }));
  expect(location.hash).toContain("sort=who");
  expect(location.hash).not.toContain("sort.list=");
  location.hash = "#/";
});

// A ROW IS DRAWN WHEN SOMETHING IT DRAWS CHANGED, AND AT NO OTHER TIME.
//
// Every row of a grid used to be built inline in the grid's own render, so
// anything that rendered the grid drew every row: the keyboard changing hands
// (a store every grid subscribed to, announced on each mount and each pointer
// press), a `j` that moved one cursor, and every render of the screen holding
// the grid. Measured on two hundred-row grids under the development build,
// that was 143–197 ms for a pointer press and 169–268 ms for a screen's own
// render, to change no row. These cases count the rows a CELL draws, which is
// the work a row's render does, rather than the grid's renders.

/** Two grids of `n` rows under one screen, whose cells count their renders. */
function countedScreen(n: number) {
  // ROWS DRAWN, and COMMITS of anything under the screen: a grid rendering
  // with no row drawn is still a render nobody asked for.
  const drawn = { rows: 0, commits: 0 };
  function Counted({ text }: { text: string }) {
    drawn.rows += 1;
    return <>{text}</>;
  }
  const upper = Array.from({ length: n }, (_, i) => ({ id: `u${i}`, who: `ceo-${i}` }));
  const lower = Array.from({ length: n }, (_, i) => ({ id: `l${i}`, who: `cto-${i}` }));
  // ONE COLUMN LIST, held as every screen holds its own (memoised): a new
  // list is a new value to a row, and rightly, because a column closing over
  // something new may draw something new.
  const columns = [{ key: "who", header: "Who", cell: (r: Row) => <Counted text={r.who} /> }];
  const seen: string[] = [];
  let rerender: () => void = () => {};
  function Screen() {
    const [, setN] = useState(0);
    rerender = () => setN((x) => x + 1);
    return (
      <>
        {/* AN INLINE HANDLER, which is what every screen hands a grid. */}
        <DataGrid<Row>
          rows={upper}
          name="upper"
          rowKey={(r) => r.id}
          onRowActivate={(r) => seen.push(r.id)}
          columns={columns}
        />
        <DataGrid<Row>
          rows={lower}
          name="lower"
          rowKey={(r) => r.id}
          onRowActivate={(r) => seen.push(r.id)}
          columns={columns}
        />
      </>
    );
  }
  const view = render(
    <Router>
      <Profiler
        id="screen"
        onRender={() => {
          drawn.commits += 1;
        }}
      >
        <Screen />
      </Profiler>
    </Router>,
  );
  return { drawn, seen, view, rerender: () => act(() => rerender()) };
}

test("a grid draws each row once when it mounts, beside another grid", () => {
  const { drawn } = countedScreen(20);
  expect(drawn.rows).toBe(40);
});

test("the keyboard changing hands draws no row, and the next key reaches the new grid", () => {
  const { drawn, seen, view } = countedScreen(20);
  const wraps = view.container.querySelectorAll<HTMLElement>(".grid-wrap");
  drawn.rows = 0;
  drawn.commits = 0;
  fireEvent.pointerDown(wraps[1]!);
  // NOTHING AT ALL, not merely no row: no grid draws who holds the keyboard.
  expect(drawn.commits).toBe(0);
  expect(drawn.rows).toBe(0);
  // AND IT CHANGED HANDS: whose keystroke this is was asked at the keystroke.
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "Enter" });
  expect(seen).toEqual(["l0"]);
});

test("a cursor step draws the rows it leaves and lands on, and no other", () => {
  const { drawn, view } = countedScreen(20);
  drawn.rows = 0;
  fireEvent.keyDown(window, { key: "j" });
  expect(drawn.rows).toBe(1);
  fireEvent.keyDown(window, { key: "j" });
  expect(drawn.rows).toBe(3);
  const cursor = view.container.querySelectorAll(".grid-row.cursor");
  expect(cursor).toHaveLength(1);
  expect([...view.container.querySelectorAll(".grid-row")].indexOf(cursor[0]!)).toBe(1);
});

test("a screen rendering with the same rows and columns draws no row", () => {
  const { drawn, seen, rerender } = countedScreen(20);
  drawn.rows = 0;
  rerender();
  expect(drawn.rows).toBe(0);
  // AND THE CLICK IS STILL THE LATEST HANDLER'S, through the ref the rows share.
  fireEvent.click(screen.getAllByRole("button")[0]!);
  expect(seen).toEqual(["u0"]);
});

// THE ORDER IS THE COLUMN'S, AND IT IS WORKED OUT ONCE.
//
// The grid sorted its whole answer twice a render — once for the cursor's
// walk, once to draw it — and read both rows' values on every comparison, and
// it did so on every render, because the parsed `sort=` was a new object each
// time and every memo keyed on it missed. A screen rendering for any reason
// re-sorted its grids. These cases hold the order itself, and that a row's
// value is read once per order and not at all by a render that changes none.

interface Ranked {
  id: string;
  rank: string | null;
}

/** One grid sorted by `rank`, whose sort value counts its reads. */
function rankedGrid(rows: Ranked[], sort: string, bands = false) {
  location.hash = `#/?sort=${sort}`;
  const reads = { count: 0 };
  const columns = [
    {
      key: "rank",
      header: "Rank",
      sortValue: (r: Ranked) => {
        reads.count += 1;
        return r.rank;
      },
      cell: (r: Ranked) => r.id,
    },
  ];
  let rerender: () => void = () => {};
  function Screen() {
    const [, setN] = useState(0);
    rerender = () => setN((x) => x + 1);
    return (
      <DataGrid<Ranked>
        {...(bands
          ? {
              bands: [
                { key: "first", label: "First", rows: rows.slice(0, 3) },
                { key: "rest", label: "Rest", rows: rows.slice(3) },
              ],
            }
          : { rows })}
        rowKey={(r) => r.id}
        columns={columns}
      />
    );
  }
  const view = render(
    <Router>
      <Screen />
    </Router>,
  );
  const order = () =>
    [...view.container.querySelectorAll(".grid-row")].map((row) => row.textContent ?? "");
  return { reads, order, rerender: () => act(() => rerender()) };
}

const RANKED: Ranked[] = [
  { id: "b", rank: "b" },
  { id: "a1", rank: "a" },
  { id: "none", rank: null },
  { id: "c", rank: "c" },
  { id: "a2", rank: "a" },
];

test("a grid orders by its column, the absent last both ways and equals as they came", () => {
  expect(rankedGrid(RANKED, "rank").order()).toEqual(["a1", "a2", "b", "c", "none"]);
  cleanup();
  expect(rankedGrid(RANKED, "-rank").order()).toEqual(["c", "b", "a1", "a2", "none"]);
  cleanup();
  // AND A BAND ORDERS ITS OWN ROWS, under its own head.
  expect(rankedGrid(RANKED, "rank", true).order()).toEqual(["a1", "b", "none", "a2", "c"]);
  location.hash = "#/";
});

test("a row's sort value is read once per order, and not by a render that changes none", () => {
  const { reads, rerender } = rankedGrid(RANKED, "rank");
  expect(reads.count).toBe(RANKED.length);
  reads.count = 0;
  rerender();
  expect(reads.count).toBe(0);
  // A NEW ORDER IS READ AGAIN, once.
  fireEvent.click(screen.getByRole("button", { name: "Rank" }));
  expect(reads.count).toBe(RANKED.length);
  location.hash = "#/";
});

// THE CURSOR IS A ROW, NOT A PLACE. A feed is newest first, so a poll that
// brings one new row slides every row under the cursor down a place: held as an
// index, the highlight moved onto the row above the one the reader had walked
// to, and Enter opened that one.
test("a row arriving above the cursor leaves the cursor on its row, and Enter opens it", () => {
  const opened: string[] = [];
  const columns = [{ key: "who", header: "Who", cell: (r: Row) => r.who }];
  const grid = (rows: Row[]) => (
    <Router>
      <DataGrid<Row>
        rows={rows}
        rowKey={(r) => r.id}
        onRowActivate={(r) => opened.push(r.id)}
        columns={columns}
      />
    </Router>
  );
  const a = { id: "a", who: "ceo" };
  const b = { id: "b", who: "cto" };
  const c = { id: "c", who: "pm" };
  const view = render(grid([a, b, c]));
  fireEvent.keyDown(window, { key: "j" });
  fireEvent.keyDown(window, { key: "j" });
  const cursorRow = () => view.container.querySelector(".grid-row.cursor")?.textContent;
  expect(cursorRow()).toBe("cto");

  view.rerender(grid([{ id: "new", who: "eng" }, a, b, c]));
  expect(cursorRow()).toBe("cto");
  fireEvent.keyDown(window, { key: "Enter" });
  expect(opened).toEqual(["b"]);
  // AND THE WALK GOES ON FROM IT, not from where it used to be.
  fireEvent.keyDown(window, { key: "j" });
  expect(cursorRow()).toBe("pm");

  // A ROW THAT LEAVES is stepped on from the gap it left.
  view.rerender(grid([{ id: "new", who: "eng" }, a, b]));
  expect(cursorRow()).toBeUndefined();
  fireEvent.keyDown(window, { key: "k" });
  expect(cursorRow()).toBe("cto");
});

// A CURSOR STEP SCROLLS TO THE ROW IT LANDS ON, found by that row's KEY. A row
// is no longer told its place — a new row above it would draw it again for that
// alone — so the step finds the row by its own id, which is its key, ENCODED:
// an id list is split on whitespace, and a key is whatever a screen's `rowKey`
// returns.
test("a cursor step scrolls the row it lands on into view, whatever its key", () => {
  const scrolled: Element[] = [];
  const original = Element.prototype.scrollIntoView;
  Element.prototype.scrollIntoView = function (this: Element) {
    scrolled.push(this);
  };
  try {
    const rows: Row[] = [
      { id: "a b", who: "ceo" },
      { id: "a%20b", who: "cto" },
    ];
    const { container } = render(
      <Router>
        <DataGrid<Row>
          rows={rows}
          rowKey={(r) => r.id}
          onRowActivate={() => {}}
          columns={[{ key: "who", header: "Who", cell: (r) => r.who }]}
        />
      </Router>,
    );
    const drawn = [...container.querySelectorAll(".grid-row")];
    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "j" });
    expect(scrolled).toEqual([drawn[0], drawn[1]]);
    // AND THE TWO KEYS ARE TWO IDS, neither of which names anything else.
    expect(new Set(drawn.map((row) => row.id)).size).toBe(2);
    expect(drawn.every((row) => !/\s/.test(row.id))).toBe(true);
  } finally {
    Element.prototype.scrollIntoView = original;
  }
});

// ONE ROW MAY STAND IN TWO BANDS. A label board groups by a multi-valued axis —
// a task with two tags is on both tags' columns — so a grid's key names a row
// and not a place in the walk: the cursor, Enter and an element id are each
// about the row WHERE IT STANDS. Asked of the key alone, both copies lit
// together, two elements carried one id, and `j` from the second copy went
// back to the first — the cursor could never pass it.
test("a row standing in two bands is two stops for the cursor, each with its own id", () => {
  const opened: string[] = [];
  const shared = { id: "t-1", who: "ceo" };
  const { container } = render(
    <Router>
      <DataGrid<Row>
        bands={[
          { key: "infra", label: "infra", rows: [shared, { id: "t-2", who: "cto" }] },
          { key: "ui", label: "ui", rows: [shared, { id: "t-3", who: "pm" }] },
        ]}
        rowKey={(r) => r.id}
        onRowActivate={(r) => opened.push(r.id)}
        columns={[{ key: "who", header: "Who", cell: (r) => r.who }]}
      />
    </Router>,
  );
  const drawn = () => [...container.querySelectorAll(".grid-row")];
  const lit = () => drawn().flatMap((row, at) => (row.classList.contains("cursor") ? [at] : []));
  const visited: number[][] = [];
  for (let i = 0; i < 4; i++) {
    fireEvent.keyDown(window, { key: "j" });
    visited.push(lit());
  }
  expect(visited).toEqual([[0], [1], [2], [3]]);
  fireEvent.keyDown(window, { key: "k" });
  expect(lit()).toEqual([2]);
  fireEvent.keyDown(window, { key: "Enter" });
  expect(opened).toEqual(["t-1"]);
  expect(new Set(drawn().map((row) => row.id)).size).toBe(4);
});

// ---------------------------------------------------------------------------
// A grid narrower than its columns
// ---------------------------------------------------------------------------
//
// THE WRAP CLIPS, so a grid that did not fit either lost its last columns at
// the edge or — with every flexible track floored at zero — drew its one
// flexible column at nothing: a work list beside a peek at 1280 showed titles
// 70px wide. A column that is the point of the list takes a floor, and the
// columns that can go go in their declared order, and the grid says which.

test("a flexible column takes its floor into its track", () => {
  const { container } = render(
    <Router>
      <DataGrid<Row>
        rows={ROWS}
        rowKey={(r) => r.id}
        columns={[
          { key: "id", header: "Id", shrink: true, cell: (r) => r.id },
          { key: "title", header: "Title", floor: "12rem", cell: (r) => r.id },
        ]}
      />
    </Router>,
  );
  expect(container.querySelector<HTMLElement>(".grid-wrap")?.style.gridTemplateColumns).toContain(
    "minmax(12rem, 1fr)",
  );
});

describe("fitColumns", () => {
  const visible = [{ key: "title" }, { key: "due", drop: 2 }, { key: "updated", drop: 1 }];
  const fit = { set: "title,due,updated", width: 500, dropped: [] as string[] };

  test("an overrun drops the column that goes first, and only that one", () => {
    expect(fitColumns({ fit, width: 500, overflows: true, visible })?.dropped).toEqual(["updated"]);
  });

  test("a column with no place in the order never goes", () => {
    const next = fitColumns({ fit, width: 500, overflows: true, visible: [{ key: "title" }] });
    expect(next).toBeNull();
  });

  test("a box that grew gives every column another chance", () => {
    const narrowed = { ...fit, dropped: ["updated", "due"] };
    expect(fitColumns({ fit: narrowed, width: 800, overflows: false, visible })?.dropped).toEqual(
      [],
    );
  });

  test("a fit that stands is left alone", () => {
    expect(fitColumns({ fit, width: 500, overflows: false, visible })).toBeNull();
  });
});

describe("the grid, laid out", () => {
  const COL = 120;
  let boxWidth = 500;
  let observed: (() => void) | null = null;
  const real = globalThis.ResizeObserver;

  beforeEach(() => {
    boxWidth = 500;
    globalThis.ResizeObserver = class {
      constructor(private readonly report: () => void) {}
      observe(): void {
        observed = () => this.report();
      }
      unobserve(): void {}
      disconnect(): void {}
    } as unknown as typeof ResizeObserver;
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.classList.contains("grid-wrap") ? boxWidth : 0;
    });
    // Every head cell is 120px, laid end to end from the wrap's left edge.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const at = this.classList.contains("grid-th")
        ? [...this.parentElement!.children].indexOf(this)
        : -1;
      const right = at >= 0 ? (at + 1) * COL : this.classList.contains("grid-wrap") ? boxWidth : 0;
      return { left: 0, right, top: 0, bottom: 0, width: right, height: 0, x: 0, y: 0 } as DOMRect;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    globalThis.ResizeObserver = real;
    observed = null;
  });

  const columns = [
    { key: "key", header: "Key", shrink: true, cell: (r: Row) => r.id },
    { key: "title", header: "Title", floor: "12rem", cell: (r: Row) => r.id },
    { key: "who", header: "Who", shrink: true, drop: 3, cell: (r: Row) => r.who },
    { key: "due", header: "Due", shrink: true, drop: 2, cell: (r: Row) => r.id },
    { key: "updated", header: "Updated", shrink: true, drop: 1, cell: (r: Row) => r.id },
  ];
  const heads = (container: HTMLElement) =>
    [...container.querySelectorAll(".grid-th")].map((h) => h.textContent);

  test("columns that do not fit give way in order, and the grid names them", () => {
    // Five 120px heads in a 500px box: one has to go, and Updated goes first.
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} />
      </Router>,
    );
    expect(heads(container)).toEqual(["Key", "Title", "Who", "Due"]);
    expect(container.querySelector(".grid-foot")?.textContent).toContain("Hidden to fit: Updated");
    // The cells follow the heads: a dropped column is gone from every row.
    expect(container.querySelector(".grid-row")?.children.length).toBe(4);
  });

  // THE QUIET FAILURE: nothing overruns, but a content column is squeezed
  // below its cap while the columns that could have gone stay on screen.
  test("a content column cut short below its cap counts as not fitting", () => {
    boxWidth = 1000;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      // Every head 60px — well inside a 1000px box, and under its 200px cap.
      const at = this.classList.contains("grid-th")
        ? [...this.parentElement!.children].indexOf(this)
        : -1;
      const right = at >= 0 ? (at + 1) * 60 : this.classList.contains("grid-wrap") ? boxWidth : 0;
      return { left: 0, right, top: 0, bottom: 0, width: at >= 0 ? 60 : right } as DOMRect;
    });
    // The Who column's value is cut short: its box is narrower than its text.
    vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.dataset.value === "cut" ? 90 : 0;
    });
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.classList.contains("grid-wrap")) return boxWidth;
      return this.dataset.value === "cut" ? 40 : 0;
    });
    const cut = columns.map((c) =>
      c.key === "who" ? { ...c, cell: (r: Row) => <span data-value="cut">{r.who}</span> } : c,
    );
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={cut} />
      </Router>,
    );
    // Updated and Due go first, in their order; Who, still cut, goes last.
    expect(heads(container)).toEqual(["Key", "Title"]);
    expect(container.querySelector(".grid-foot")?.textContent).toContain(
      "Hidden to fit: Updated, Due and Who",
    );
  });

  // A SCREEN-READER-ONLY BOX IS ONE PIXEL WIDE ON PURPOSE. Read as a value cut
  // short, the "Unassigned" beside an empty avatar dropped four columns from a
  // work list with nothing squeezed at all.
  test("text for a screen reader alone is not a value cut short", () => {
    boxWidth = 1000;
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      const at = this.classList.contains("grid-th")
        ? [...this.parentElement!.children].indexOf(this)
        : -1;
      const right = at >= 0 ? (at + 1) * 60 : this.classList.contains("grid-wrap") ? boxWidth : 0;
      return { left: 0, right, top: 0, bottom: 0, width: at >= 0 ? 60 : right } as DOMRect;
    });
    vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.classList.contains("sr-only") ? 70 : 0;
    });
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.classList.contains("grid-wrap")) return boxWidth;
      return this.classList.contains("sr-only") ? 1 : 0;
    });
    const spoken = columns.map((c) =>
      c.key === "who"
        ? { ...c, cell: (r: Row) => <span className="sr-only">{`Unassigned ${r.who}`}</span> }
        : c,
    );
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={spoken} />
      </Router>,
    );
    expect(heads(container)).toEqual(["Key", "Title", "Who", "Due", "Updated"]);
  });

  test("a box that narrows drops more, and one that grows gets them back", () => {
    const { container } = render(
      <Router>
        <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} />
      </Router>,
    );
    boxWidth = 300;
    act(() => observed?.());
    expect(heads(container)).toEqual(["Key", "Title"]);
    expect(container.querySelector(".grid-foot")?.textContent).toContain(
      "Hidden to fit: Updated, Due and Who",
    );
    boxWidth = 800;
    act(() => observed?.());
    expect(heads(container)).toEqual(["Key", "Title", "Who", "Due", "Updated"]);
    expect(container.querySelector(".grid-foot")).toBeNull();
  });
});

// A COMPACT GRID SAYS SO, AND SAYS WHICH CELLS LEAD. The sheet draws a phone row
// from these two attributes and nothing else (styles/frame.test.ts), so the DOM
// half is that the wrap carries the choice and only the lead columns' cells are
// marked — and that the default, which every other grid takes, carries neither.
test("a compact grid marks its wrap and its lead cells", () => {
  const columns = [
    { key: "id", header: "Id", phoneLead: true, cell: (r: Row) => r.id },
    { key: "mark", header: "Mark", cell: () => <span>·</span> },
  ];
  const compact = render(
    <Router>
      <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} phoneRows="compact" />
    </Router>,
  );
  const wrap = compact.container.querySelector(".grid-wrap")!;
  expect(wrap.getAttribute("data-phone-rows")).toBe("compact");
  const cells = [...wrap.querySelectorAll(".grid-row > .grid-cell")];
  expect(cells.map((c) => c.hasAttribute("data-lead"))).toEqual(ROWS.flatMap(() => [true, false]));
  cleanup();
  const labelled = render(
    <Router>
      <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} />
    </Router>,
  );
  expect(labelled.container.querySelector(".grid-wrap")!.hasAttribute("data-phone-rows")).toBe(
    false,
  );
});

// A FLUSH GRID SAYS SO ON ITS WRAP, and the default carries nothing: the sheet
// draws the card-body form from that attribute alone (styles/frame.test.ts).
test("a flush grid marks its wrap, and the default does not", () => {
  const columns = [{ key: "id", header: "Id", cell: (r: Row) => r.id }];
  const flush = render(
    <Router>
      <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} flush />
    </Router>,
  );
  expect(flush.container.querySelector(".grid-wrap")!.hasAttribute("data-flush")).toBe(true);
  cleanup();
  const framed = render(
    <Router>
      <DataGrid<Row> rows={ROWS} rowKey={(r) => r.id} columns={columns} />
    </Router>,
  );
  expect(framed.container.querySelector(".grid-wrap")!.hasAttribute("data-flush")).toBe(false);
});
