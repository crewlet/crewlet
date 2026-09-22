/**
 * What is narrowing the rows, as the line that says so and takes it off.
 *
 * A chip is the only place a reader is told a filter is on, so every failure
 * here is silent by construction: a chip that names the wrong key removes
 * somebody else's narrowing, a chip drawn for a narrowing the reader cannot
 * remove is a control reporting a state nobody can reach, and a Clear that
 * misses one key leaves a list narrowed by something that has just been said
 * to be gone.
 *
 * TWO HALVES, as the two files under test are: `filterChips` decides WHICH
 * chips and what each says (it is pure, and `lib/work.test.ts` holds its
 * wording), this component decides how they are drawn and what a press hands
 * back, and the SCREEN owns Clear — so the last section mounts the list.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { FilterChips } from "./FilterChips.tsx";
import { ItemsView, type ItemsHost } from "../ItemsView.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { filterChips, NO_FILTERS, URL_HOMES, type TrackerFilters } from "~/lib/work.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** What the company calls each value, which is what a chip has to say. */
const LABELS = {
  statuses: [{ status: "in_progress" as const, label: "Doing", group: "active", description: "" }],
  types: [{ slug: "bug", name: "Defect" }],
  tags: [{ slug: "platform", label: "Platform" }],
  fields: [{ id: "f-1", slug: "area", name: "Area", type: "dropdown" }],
  seatName: (handle: string) => (handle === "ada" ? "Ada Okonkwo" : handle),
  unitName: (key: string) => (key === "eng" ? "Engineering" : ""),
};

/** The chips of one set of filters, drawn. */
function drawChips(filters: Partial<TrackerFilters>) {
  const onRemove = vi.fn();
  const onClear = vi.fn();
  const { container } = render(
    <FilterChips
      chips={filterChips({ ...NO_FILTERS, ...filters }, LABELS)}
      onRemove={onRemove}
      onClear={onClear}
    />,
  );
  return { container, onRemove, onClear };
}

// ---------------------------------------------------------------------------
// One chip per narrowing, in the company's own words
// ---------------------------------------------------------------------------

// A CHIP SAYS WHAT THE COMPANY CALLS THE VALUE, never the token on the
// address: `in_progress` where the team says Doing, or `ada` where the chart
// says Ada Okonkwo, is a chip that reads as a filter which failed to resolve.
test("every applied filter is one chip, worded the way the company words it", () => {
  const { container } = drawChips({
    q: "billing",
    status: "in_progress",
    type: "bug",
    priority: "high",
    assignee: "ada",
    tag: "platform",
    unit: "eng",
    due: "overdue",
    blocked: true,
    removed: true,
    fields: { "f.area": "api" },
  });
  const text = container.textContent ?? "";
  for (const said of [
    "Text",
    "contains",
    "billing",
    // THE PROJECT'S OWN LABEL for a status, through `statusLabel`.
    "Doing",
    // AND THE COMPANY'S OWN NAME for a type and for a tag.
    "Defect",
    "Platform",
    "High",
    "Ada Okonkwo",
    // THE UNIT ARM: a key out of the address resolves against nothing on this
    // side of the wire, so the chip takes the name the ANSWER gave it.
    "Engineering",
    "Overdue",
    // THE SWITCHES HAVE NO VALUE TO READ, so the field name is the whole chip.
    "Blocked",
    "Removed items",
    "Area",
  ]) {
    expect(text, `${said} is not on the chip row`).toContain(said);
  }
  // AND NONE OF THE RAW TOKENS the address carries.
  for (const token of ["in_progress", "ada", "platform", "eng"]) {
    expect(text, `${token} is printed raw`).not.toContain(token);
  }
});

// UNASSIGNED IS A VALUE the grammar spells `none`, and it is the one a lead
// opens a board to ask for — printed raw it reads as a filter that failed to
// resolve somebody's name.
test("the unassigned filter is a word rather than the grammar's token", () => {
  const { container } = drawChips({ assignee: "none" });
  expect(container.textContent).toContain("Unassigned");
  expect(container.textContent).not.toContain("none");
});

// A UNIT WITH NO ANSWER TO NAME IT says the key the address holds, which is a
// value a reader can still act on — rather than a chip that says nothing while
// a second read is in flight.
test("a unit the answer could not name says its own key", () => {
  const onRemove = vi.fn();
  render(
    <FilterChips
      chips={filterChips({ ...NO_FILTERS, unit: "eng" }, {})}
      onRemove={onRemove}
      onClear={vi.fn()}
    />,
  );
  expect(screen.getByText("eng")).toBeTruthy();
});

// DRAWN ONLY WHERE SOMETHING IS ON: an unfiltered list has no chip row at all,
// which is the whole difference from a bar of controls drawn whether or not
// they were set.
test("nothing narrowing draws no row, not an empty one", () => {
  const { container } = drawChips({});
  expect(container.innerHTML).toBe("");
  expect(screen.queryByText("Clear")).toBeNull();
});

// ---------------------------------------------------------------------------
// What a press hands back
// ---------------------------------------------------------------------------

// EACH CHIP IS ONE URL KEY and taking it off clears that key and nothing else,
// which is what makes a chip removable without a table of removers beside the
// table of chips.
test("removing a chip hands back the one key it stands for", () => {
  const { onRemove } = drawChips({
    status: "in_progress",
    priority: "high",
    fields: { "f.area": "api" },
  });
  // EVERY CHIP ON THE ROW, one at a time, because a remover that always hands
  // back the FIRST key passes a case that only ever presses the first chip —
  // and what it clears is somebody else's narrowing, silently.
  for (const [named, param] of [
    ["Status is Doing", "status"],
    ["Priority is High", "priority"],
    ["Area is api", "f.area"],
  ] as const) {
    onRemove.mockClear();
    fireEvent.click(screen.getByLabelText(`Remove the ${named} filter`));
    expect(onRemove.mock.calls).toEqual([[param]]);
  }
});

// THE WHOLE SENTENCE NAMES THE CONTROL, because a remove control is announced
// by its own name and "remove" alone names nothing — a row of eight would
// otherwise read as eight identical controls.
test("every remove control is named by the chip it takes off", () => {
  drawChips({ status: "in_progress", assignee: "ada", blocked: true });
  expect(screen.getByLabelText("Remove the Status is Doing filter")).toBeTruthy();
  expect(screen.getByLabelText("Remove the Assignee is Ada Okonkwo filter")).toBeTruthy();
  // A SWITCH HAS NO VALUE, so its sentence is the field name alone rather than
  // a sentence with a hole in it.
  expect(screen.getByLabelText("Remove the Blocked filter")).toBeTruthy();
});

// CLEAR IS NOT A CHIP. It removes every one of them, so drawing it as a pill
// in the same row would put a control that empties the row beside the controls
// that do not.
test("Clear is one control beside the chips rather than another chip", () => {
  const { container, onClear, onRemove } = drawChips({ status: "in_progress" });
  const clear = screen.getByRole("button", { name: "Clear" });
  expect(container.querySelector(".crewlet-tag")?.contains(clear)).not.toBe(true);
  fireEvent.click(clear);
  expect(onClear).toHaveBeenCalledTimes(1);
  expect(onRemove).not.toHaveBeenCalled();
});

// ---------------------------------------------------------------------------
// On the screen: the lock, and what Clear clears
// ---------------------------------------------------------------------------

/** One socket answering each question with a fixture, recording the params. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(
    async (what: string, _params?: Record<string, unknown>) => answers[what as QueryName] ?? {},
  );
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
  updated: "2031-04-16T00:00:00Z",
  version: 1,
};

const answered = { items: [task], groups: [], total_hint: 1, complete: true };

/** A host screen's narrowing: what `#/me`'s Assigned tab hands down. */
const HOST: ItemsHost = {
  assignee: "rui",
  opens: {},
  empty: () => ({ title: "Nothing here", description: "Nothing is assigned." }),
};

const mountList = (host?: ItemsHost) =>
  render(
    <Router>
      <ItemsView host={host} />
    </Router>,
  );

// A LOCKED NARROWING DRAWS NO CHIP, because a chip is removable and this is
// not: it is what the screen IS. A chip that will not come off is a control
// reporting a state the reader cannot reach — and the key is not even the
// screen's, so one left on the address by hand is inert rather than a second,
// silent narrowing under somebody's name.
test("the locked assignee draws no chip, and an assignee key on the address draws none either", async () => {
  location.hash = "#/me?assignee=ada";
  const query = serving({ work_items: answered });
  mountList(HOST);
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  expect(document.querySelector(".work-chips")).toBeNull();
  // AND THE LOCK IS WHAT REACHED THE WIRE, not the key somebody typed.
  const items = query.mock.calls.filter((c) => c[0] === "work_items");
  expect(items.length).toBeGreaterThan(0);
  for (const call of items) {
    expect((call[1] as Record<string, unknown>).assignee).toBe("rui");
  }
});

/** Every key on the address at once: the filters, and the arrangement. */
const NARROWED =
  "#/work?q=bill&status=todo&type=task&priority=high&assignee=ada&tag=platform" +
  "&unit=eng&due=overdue&blocked=true&removed=true&group=todo&f.area=api&scope=all" +
  "&shape=table&sort=-updated&group_by=status&group_by2=priority&cols.table=key,title&view=arranged";

// CLEAR CLEARS EVERY FILTER KEY — every one of them, which is what the control
// says it does. One left behind is a list still narrowed by something the
// reader has just been told is gone, with its chip gone too: `unit=` was
// exactly that, so a list reached from an item's "Filed into" line could not
// be widened again from the empty state that offered to do it.
test("Clear clears every filter key", async () => {
  location.hash = NARROWED;
  serving({
    work_items: { items: [], groups: [], total_hint: 0, complete: true },
    work_views: {
      views: [
        {
          id: "v-1",
          key: "arranged",
          name: "Arranged",
          type: "table",
          container: { kind: "workspace", id: "" },
          builtin: false,
          params: {},
        },
      ],
      complete: true,
    },
  });
  mountList();
  await waitFor(() => expect(document.querySelector(".work-chips")).toBeTruthy());
  fireEvent.click(within(document.querySelector(".work-chips") as HTMLElement).getByText("Clear"));

  // EVERY KEY THE GRAMMAR CALLS A CHIP, from the declaration rather than from
  // a list written out here: one added later is covered without a second edit.
  const gone = Object.entries(URL_HOMES)
    .filter(([, home]) => home === "chip")
    .map(([key]) => key);
  await waitFor(() => expect(location.hash).not.toContain("status="));
  const query = new URLSearchParams(location.hash.split("?")[1]);
  for (const key of gone) {
    const present = key.endsWith(".")
      ? [...query.keys()].filter((k) => k.startsWith(key))
      : [...query.keys()].filter((k) => k === key);
    expect(present, `${key} survived Clear`).toEqual([]);
  }
  // AND THE SCOPE GOES BACK TO THE VIEW'S rather than to a literal `open`:
  // clearing a narrowing returns the screen to what the view asked for, so the
  // key is dropped rather than written.
  expect(query.has("scope")).toBe(false);
});

// AND THE ARRANGEMENT IS NOT A NARROWING, so Clear leaves every bit of it
// standing: pressed over a board nobody had filtered, a Clear that flattened
// the arrangement removed nothing and threw away what the reader had chosen.
test("Clear leaves the arrangement and the view exactly where they were", async () => {
  location.hash = NARROWED;
  serving({
    work_items: { items: [], groups: [], total_hint: 0, complete: true },
    work_views: {
      views: [
        {
          id: "v-1",
          key: "arranged",
          name: "Arranged",
          type: "table",
          container: { kind: "workspace", id: "" },
          builtin: false,
          params: {},
        },
      ],
      complete: true,
    },
  });
  mountList();
  await waitFor(() => expect(document.querySelector(".work-chips")).toBeTruthy());
  fireEvent.click(within(document.querySelector(".work-chips") as HTMLElement).getByText("Clear"));
  await waitFor(() => expect(location.hash).not.toContain("status="));

  const query = new URLSearchParams(location.hash.split("?")[1]);
  expect(query.get("shape")).toBe("table");
  expect(query.get("sort")).toBe("-updated");
  expect(query.get("group_by")).toBe("status");
  expect(query.get("group_by2")).toBe("priority");
  expect(query.get("cols.table")).toBe("key,title");
  expect(query.get("view")).toBe("arranged");
});
