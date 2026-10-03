/**
 * A FILTER IS A CHIP; AN ARRANGEMENT IS A CONTROL THAT SAYS WHAT IT IS SET
 * TO — walked over every key.
 *
 * The design states the rule once and `lib/work.ts` declares where each key
 * lives ([URL_HOMES]). This is the gate that holds the two against the SCREEN:
 * every key the work list puts on the address has a home, every home is a key
 * the list actually writes, and each one is drawn by exactly one control —
 * never both, never neither.
 *
 * Both halves fail silently without it. A key with no home is a narrowing a
 * reader can reach only by editing the address, with nothing on screen saying
 * it is on: `unit=` was exactly that for its chip's whole life, and the Clear
 * control that claimed to remove every filter walked straight past it. A key
 * with two is two controls for one fact, which disagree the first time either
 * one writes.
 *
 * DERIVED, never listed. The key set is read out of `ItemsView.tsx` — the one
 * component these three screens are — the way `internal/clientsource` reads a
 * declaration out of this tree for the engine's own gates, because a list of
 * twenty strings written here is a list that goes stale. Everything else comes
 * from the declarations: [NO_FILTERS] is the runtime witness of
 * [TrackerFilters], [filterChips] says which of them is a chip, and
 * [GRID_SHAPES] is where the per-shape column keys come from. A key added
 * later with nowhere to live fails here rather than shipping invisible.
 */

import { answered, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";

import { ItemsView } from "../ItemsView.tsx";
// THE SCREEN'S OWN SOURCE, which is this gate's subject — see `vite-env.d.ts`
// for why it arrives as a `?raw` import rather than through `node:fs`.
import SOURCE from "../ItemsView.tsx?raw";
import { colsParam, columnChoices, GRID_SHAPES } from "../shapes/Grid.tsx";
import { Router } from "~/app/router.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { pick } from "~/testing.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { filterChips, NO_FILTERS, URL_HOMES, type TrackerFilters } from "~/lib/work.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  // THE AGENTS PUSH, which the list reads for the turn running on each card:
  // no seat is working in these cases, so the push is empty.
  return {
    ...actual,
    useClient: vi.fn(),
    useConnection: vi.fn(),
    useOrg: vi.fn(),
    useAgents: () => [],
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

// ---------------------------------------------------------------------------
// The keys the screen writes, read from the screen
// ---------------------------------------------------------------------------

/**
 * Every URL key this screen reads or writes.
 *
 * `useParam` IS THE WHOLE GRAMMAR of it — a key nothing reads is a key nothing
 * can act on — plus what is read off the whole query rather than one key at a
 * time: the family of a company's own fields, because that set is the
 * company's and there is no key for a build to ask for, and the column
 * narrowing (`group`), because it is THREE-VALUED and `useParam` cannot say so
 * (an absent key and a present empty one are different columns). Both
 * spellings of the column key arrive through [colsParam], so they are
 * recognised in that form rather than as literals the screen does not contain.
 */
function screenKeys(): string[] {
  const out = new Set<string>();
  for (const [, literal, shape] of SOURCE.matchAll(
    /useParam\(\s*(?:"([^"]+)"|colsParam\("([^"]+)"\))/g,
  )) {
    if (literal) out.add(literal);
    if (shape) out.add(colsParam(shape as (typeof GRID_SHAPES)[number]));
  }
  for (const [, prefix] of SOURCE.matchAll(/key\.startsWith\("([^"]+)"\)/g)) {
    if (prefix) out.add(prefix);
  }
  for (const [, key] of SOURCE.matchAll(/route\.query\.(?:has|get)\("([^"]+)"\)/g)) {
    if (key) out.add(key);
  }
  return [...out];
}

/** The family a key belongs to, where it is one of the two: `f.x` → `f.`. */
function family(key: string): string {
  const prefix = Object.keys(URL_HOMES).find((home) => home.endsWith(".") && key.startsWith(home));
  return prefix ?? key;
}

// ---------------------------------------------------------------------------
// The walk
// ---------------------------------------------------------------------------

// EVERY KEY HAS EXACTLY ONE HOME, and every home is a key. Two directions,
// because each catches the opposite mistake: a key added with no control is
// invisible, and a home left behind by a key that went away is a rule about
// nothing, which reads exactly like a rule that holds.
test("every key the work list writes is drawn by exactly one control", () => {
  const keys = screenKeys();
  // THE GATE CERTIFIES NOTHING IF IT FOUND NOTHING — the `clientsource`
  // precaution, because a regex that stops matching is a green suite.
  expect(keys.length).toBeGreaterThan(15);

  const homed = new Set(keys.map(family));
  for (const key of homed) {
    expect(
      URL_HOMES[key],
      `${key} has no home: it is on the address and no control owns it`,
    ).toBeTruthy();
  }
  for (const key of Object.keys(URL_HOMES)) {
    expect([...homed], `${key} is homed and the screen never writes it`).toContain(key);
  }
});

/**
 * Every filter set at once, which is what the chip half is derived from.
 *
 * A FIXTURE, not a second declaration: the case below holds its keys against
 * [NO_FILTERS], so a field added to [TrackerFilters] lands here and has to be
 * given a value and a home rather than passing unnoticed.
 */
const ALL_SET: TrackerFilters = {
  q: "billing",
  status: "todo",
  type: "task",
  priority: "high",
  assignee: "ada",
  tag: "platform",
  unit: "eng",
  scope: "all",
  groupBy: "status",
  groupBy2: "priority",
  group: "todo",
  sort: "-updated",
  blocked: true,
  due: "overdue",
  removed: true,
  fields: { "f.area": "api" },
};

// THE CHIPS ARE EXACTLY THE KEYS HOMED AS CHIPS. Derived from `filterChips`
// itself rather than from a list here, so a narrowing that gained a chip
// without a home — or kept a home after losing its chip — is a failure rather
// than a screen with a control nobody can find.
test("every chip-homed key draws a chip, and nothing else does", () => {
  expect(Object.keys(ALL_SET).sort()).toEqual(Object.keys(NO_FILTERS).sort());

  const drawn = new Set(filterChips(ALL_SET).map((chip) => family(chip.param)));
  const homed = Object.entries(URL_HOMES)
    .filter(([, home]) => home === "chip")
    .map(([key]) => key);
  expect([...drawn].sort()).toEqual(homed.sort());
});

// AND THE FIELDS THAT DRAW NO CHIP ARE THE FOUR THAT ARE NOT NARROWINGS: the
// segment, and the three arrangements. Each is set on its own here, because a
// field that draws no chip only because another one did is a field nobody is
// actually testing.
test("the filters with no chip are the segment and the arrangement", () => {
  const silent = Object.keys(NO_FILTERS).filter(
    (field) =>
      filterChips({ ...NO_FILTERS, [field]: ALL_SET[field as keyof TrackerFilters] }).length === 0,
  );
  expect(silent).toEqual(["scope", "groupBy", "groupBy2", "sort"]);
  // THE ADDRESS SPELLS TWO OF THEM DIFFERENTLY, which is why [URL_HOMES] is
  // keyed on the URL and this is the one place the two namings meet.
  expect(URL_HOMES.scope).toBe("bar");
  expect(URL_HOMES.group_by).toBe("arrangement");
  expect(URL_HOMES.group_by2).toBe("arrangement");
  expect(URL_HOMES.sort).toBe("arrangement");
});

// ---------------------------------------------------------------------------
// The arrangement half, on the screen
// ---------------------------------------------------------------------------

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>> = {}) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  } as never);
  return query;
}

const task: WorkSummary = {
  id: "1",
  key: "ENG-1",
  project: "ENG",
  title: "Ship the thing",
  type: "task",
  status: "todo",
  due: "2031-04-20T00:00:00Z",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
};

const rows = { work_items: { items: [task], groups: [], total_hint: 1, complete: true } };

/**
 * The list, UNDER A VIEWER as the frame mounts every screen: a board card's
 * drag and a row's inline status are writes, and they ask who is reading. A
 * case lets the stubbed socket's answers land, and renders what they leave,
 * with [answered].
 *
 * NOT A POLL. A `findBy` or a `waitFor` re-ran its query on every change to
 * the page and every fifty milliseconds against a one-second deadline, each
 * time a query by role over the whole list, which computes the name of every
 * button on it — and on a loaded machine the deadline passed before the list
 * had drawn. The answers are promises, so `act` runs them, and the renders
 * they cause, to the end, however long that takes; and the flush is refused
 * once the case that asks has ended, so a case still running after its time
 * ran out opens no `act` beside the next one.
 */
function mountList(): void {
  render(
    <Router>
      <ViewerProvider>
        <ItemsView />
      </ViewerProvider>
    </Router>,
  );
}

/**
 * The list's first row — what is drawn: the shape tabs and the Display menu's
 * trigger.
 */
function shapeRow(): HTMLElement {
  const row = document.querySelector<HTMLElement>(".work-tabs");
  if (!row) throw new Error("the list draws no shape row");
  return row;
}

/** The list's own bar — what cuts the answer: the filters, the scope and the arrangement. */
function workBar(): HTMLElement {
  const bar = document.querySelector<HTMLElement>(".work-bar");
  if (!bar) throw new Error("the list draws no bar");
  return bar;
}

/**
 * The list at one address, with one item on it.
 *
 * IT HOLDS THE BAR rather than the row, because the row is not on every
 * shape: a calendar draws the month it is in, and an item due in another one
 * is not on the grid at all — where the toolbar these cases are about is drawn
 * whatever the answer holds.
 */
async function listAt(
  hash: string,
  answers: Partial<Record<QueryName, unknown>> = rows,
): Promise<void> {
  location.hash = hash;
  serving(answers);
  mountList();
  await answered();
  expect(workBar()).toBeTruthy();
}

/** The Display menu, opened from the list's own first row. */
async function openDisplay() {
  fireEvent.click(within(shapeRow()).getByRole("button", { name: "Display" }));
  await answered();
}

/** An optional column of the list's set — the one a tick can turn on. */
function optionalColumn(): string {
  const choice = columnChoices("list", true).find((c) => c.optional);
  if (!choice) throw new Error("the list set has no optional column to tick");
  return choice.label;
}

/**
 * How each arrangement key is written, and what the address should then carry.
 *
 * ONE ENTRY PER ARRANGEMENT KEY, held against [URL_HOMES] below in both
 * directions: a key that gains an arrangement home and has no way of being
 * pressed here fails, rather than being claimed by a control nobody drove.
 */
const ARRANGEMENT: Record<
  string,
  {
    at: string;
    answers?: Partial<Record<QueryName, unknown>>;
    press: () => Promise<void>;
    writes: string;
  }
> = {
  shape: {
    at: "#/work",
    // THE FIRST ROW'S OWN TABS, never the Display menu: the shape is the one
    // arrangement a team switches all day.
    press: async () => {
      const shapes = within(shapeRow()).getByRole("group", { name: "Draw as" });
      fireEvent.click(within(shapes).getByRole("button", { name: "Board" }));
    },
    writes: "shape=board",
  },
  group_by: {
    at: "#/work",
    press: async () => {
      pick(within(workBar()).getByRole("combobox", { name: "Group by" }), "Assignee");
    },
    writes: "group_by=assignee",
  },
  group_by2: {
    // A SECOND AXIS IS OFFERED ONLY BESIDE A FIRST, which is the engine's own
    // refusal rather than a rule of the menu's.
    at: "#/work?group_by=status",
    press: async () => {
      await openDisplay();
      pick(screen.getByRole("combobox", { name: "Then by" }), "Priority");
    },
    writes: "group_by2=priority",
  },
  sort: {
    at: "#/work",
    press: async () => {
      pick(within(workBar()).getByRole("combobox", { name: "Sort" }), "Recently updated");
    },
    writes: "sort=-updated",
  },
  "cols.": {
    at: "#/work",
    press: async () => {
      await openDisplay();
      fireEvent.click(screen.getByLabelText(optionalColumn()));
    },
    // THE ACTIVE SHAPE'S OWN KEY, which is the family's whole point.
    writes: `${colsParam("list")}=`,
  },
  hide: {
    // A LANE'S OWN ⋯ MENU puts it away; the Display menu brings it back.
    at: "#/work?shape=board",
    answers: {
      work_items: {
        items: [],
        groups: [{ key: "todo", count: 1, rows: [task] }],
        total_hint: 1,
        complete: true,
      },
    },
    press: async () => {
      fireEvent.click(screen.getByRole("button", { name: "To do lane options" }));
      await answered();
      fireEvent.click(screen.getByRole("menuitem", { name: "Hide this lane" }));
    },
    writes: "hide=todo",
  },
  card_hide: {
    // THE BOARD'S DISPLAY MENU, which is where a card's facts are chosen.
    at: "#/work?shape=board",
    press: async () => {
      await openDisplay();
      fireEvent.click(screen.getByLabelText("Labels"));
    },
    writes: "card_hide=labels",
  },
};

const ARRANGEMENT_HOMED = Object.entries(URL_HOMES)
  .filter(([, home]) => home === "arrangement")
  .map(([key]) => key);

test("every arrangement key is one this suite presses", () => {
  expect(Object.keys(ARRANGEMENT).sort()).toEqual([...ARRANGEMENT_HOMED].sort());
});

// ONE CASE PER KEY, not one case pressing them all. Each press is a whole list
// screen mounted and a control driven — 100 to 380 ms a key, measured — and a
// single case pressing every key ran past the five-second budget whenever the
// suite shared its cores: a budget that holds one screen was being spent on
// every key the grammar homes there. Apart, each case is one screen, and a key
// whose control stopped writing it names itself.
test.each(ARRANGEMENT_HOMED)("its arrangement control writes %s", async (key) => {
  const entry = ARRANGEMENT[key] as (typeof ARRANGEMENT)[string];
  await listAt(entry.at, entry.answers);
  await entry.press();
  await answered();
  expect(location.hash, `${key} was not written by its control`).toContain(entry.writes);
  // AND NO CHIP SAYS SO, because an arrangement narrows nothing: a chip for
  // one would offer to remove a drawing.
  const chips = document.querySelector(".work-chips")?.textContent ?? "";
  expect(chips, `${key} drew a chip`).not.toContain(key);
});

// AND THE COLUMN FAMILY IS ONE KEY PER GRID SHAPE, so a third grid shape
// brings its own key rather than sharing one — the order `cols=` carries is
// the SET's, and a value written against one set and read against the other
// draws a row nobody arranged.
test("the column family covers every grid shape and nothing else", () => {
  for (const shape of GRID_SHAPES) {
    expect(family(colsParam(shape))).toBe("cols.");
  }
  expect(URL_HOMES["cols."]).toBe("arrangement");
});

// ---------------------------------------------------------------------------
// The three that are neither
// ---------------------------------------------------------------------------

// THE EXCEPTIONS ARE A CLOSED SET, and it is the one the design names. A
// fourth added quietly is how "a filter is a chip and an arrangement says what
// it is set to" stops being true while every case above still passes.
test("exactly three keys are neither a chip nor an arrangement", () => {
  const others = Object.entries(URL_HOMES).filter(
    ([, home]) => home !== "chip" && home !== "arrangement",
  );
  expect(others).toEqual([
    ["scope", "bar"],
    ["view", "strip"],
    ["month", "shape"],
  ]);
});

// THE SCOPE IS THE SWITCH IN THE BAR, and it is always set to something: as a
// chip it would either be permanently present, which is not a chip, or absent
// on its default, which hides the one segment that decides whether finished
// work is on screen at all.
test("the scope is the bar's own switch, with no chip and no row in the Filter menu", async () => {
  await listAt("#/work");
  pick(screen.getByRole("combobox", { name: "Which work" }), /^Closed/);
  await answered();
  expect(location.hash).toContain("scope=closed");
  // NO CHIP FOR IT, on a list where it is the only thing that moved.
  expect(document.querySelector(".work-chip-field")).toBeNull();
  // AND NO SECOND CONTROL FOR IT in the menu that owns the narrowings.
  fireEvent.click(within(workBar()).getByRole("button", { name: "Filter" }));
  await answered();
  expect(screen.getByText("status=")).toBeTruthy();
  expect(screen.queryByText("scope=")).toBeNull();
});

// A SAVED VIEW IS A TAB IN THE STRIP — a pressed button, pinned by this reader: it is a query somebody arranged and put
// somewhere, which is neither a narrowing the reader added nor a way of
// drawing one.
test("the saved view is a tab rather than a chip or a menu row", async () => {
  location.hash = "#/work";
  serving({
    ...rows,
    work_views: {
      views: [
        {
          id: "v-1",
          key: "arranged",
          name: "Arranged",
          type: "list",
          container: { kind: "workspace", id: "" },
          builtin: false,
          pinned: true,
          params: {},
        },
      ],
      complete: true,
    },
  });
  mountList();
  await answered();
  fireEvent.click(screen.getByRole("button", { name: "Arranged" }));
  await answered();
  expect(location.hash).toContain("view=arranged");
  expect(document.querySelector(".work-chip-field")).toBeNull();
});

// AND THE CALENDAR'S WINDOW IS THAT SHAPE'S OWN AXIS, stepped in its own
// header. It is not a narrowing somebody added and not another drawing of one
// answer — it is WHICH answer the shape asks for, which is why it belongs to
// neither control.
test("the month is the calendar's own control", async () => {
  await listAt("#/work?shape=calendar");
  fireEvent.click(screen.getByRole("button", { name: "The month after" }));
  await answered();
  expect(location.hash).toContain("month=");
  expect(document.querySelector(".work-chip-field")).toBeNull();
});
