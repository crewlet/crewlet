/**
 * What the Filter menu OFFERS, and what each row writes.
 *
 * Every case here is a narrowing that, written wrong, answers a question
 * nobody asked: a key the grammar does not have matches nothing and reads as a
 * board somebody emptied, a value spelled in the company's words rather than
 * the engine's is a clause that refuses, and a row offered where the screen
 * has already answered the question is a control that does nothing at all.
 *
 * The menu is a component with props in and one callback out — it holds no
 * router and no socket — so these render it directly and read the calls. What
 * the SCREEN does with the key it hands back is `ItemsView`'s, and
 * `grammar.test.tsx` walks that.
 *
 * THE PANEL IS PORTALLED, so everything below reaches through the document
 * rather than through the mount's own container.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { FilterMenu, type FilterMenuProps } from "./FilterMenu.tsx";
import {
  buildItemsParams,
  NO_FILTERS,
  SCOPE_GROUPS,
  STATUSES,
  type TrackerFilters,
} from "~/lib/work.ts";
import type { WorkFieldDef, WorkProjectTag, WorkTypeDef } from "~/protocol/index.ts";

afterEach(cleanup);

const SEATS = [
  { handle: "ada", name: "Ada Okonkwo" },
  { handle: "rui", name: "Rui Santos" },
];

const TYPES: WorkTypeDef[] = [
  { slug: "task", name: "Task" },
  { slug: "bug", name: "Bug" },
  // AN ARCHIVED TYPE IS NOT A TYPE ANY MORE: its values left the table, so a
  // filter over one matches nothing for ever.
  { slug: "spike", name: "Spike", archived: true },
];

const TAGS: WorkProjectTag[] = [
  { slug: "platform", label: "Platform" },
  { slug: "gone", label: "Gone", archived: true },
];

/** A dropdown, a checkbox, a people field and one that takes a typed value. */
const FIELDS: WorkFieldDef[] = [
  {
    id: "f-1",
    slug: "area",
    name: "Area",
    type: "dropdown",
    config: {
      options: [
        { id: "o-1", slug: "api", name: "API" },
        { id: "o-2", slug: "retired", name: "Retired", archived: true },
      ],
    },
  },
  { id: "f-2", slug: "urgent", name: "Urgent", type: "checkbox" },
  { id: "f-3", slug: "reviewer", name: "Reviewer", type: "people" },
  { id: "f-4", slug: "points", name: "Points", type: "number" },
  { id: "f-5", slug: "legacy", name: "Legacy", type: "text", archived: true },
];

/** The menu, rendered and opened. Returns the one callback it writes through. */
function openMenu(over: Partial<FilterMenuProps> = {}) {
  const onSet = vi.fn();
  render(
    <FilterMenu
      filters={NO_FILTERS}
      shape="list"
      onSet={onSet}
      seats={SEATS}
      types={TYPES}
      tags={TAGS}
      fields={FIELDS}
      {...over}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  return onSet;
}

/**
 * The URL keys the open panel PRINTS, in the order it offers them.
 *
 * The key is on the row on purpose — this product's address bar is a surface
 * people read — so it is also the honest thing to assert a row by: "Status" is
 * a column head and a Display option one bar over, where `status=` is only
 * ever this.
 */
const offered = (): string[] =>
  [...document.querySelectorAll(".work-menu-key")].map((el) => el.textContent ?? "");

/** One row of the panel, found by the word a reader sees on it. */
function row(label: string): HTMLElement {
  const found = [...document.querySelectorAll<HTMLElement>(".work-menu-row")].find(
    (el) => el.querySelector(".truncate")?.textContent === label,
  );
  if (!found) throw new Error(`no "${label}" row: ${offered().join(" ") || "(no keys)"}`);
  return found;
}

/** Pick a field, then one of its values. */
function pick(field: string, value: string): void {
  fireEvent.click(row(field));
  fireEvent.click(row(value));
}

// ---------------------------------------------------------------------------
// What it offers
// ---------------------------------------------------------------------------

// EVERY ROW NAMES THE KEY IT WRITES, and that key is the grammar's own
// spelling. The company's own fields come LAST and in the catalogue's order,
// because that order is a decision somebody made — re-sorted here it would be
// thrown away — and each is `f.<slug>`, which is what the engine parses.
test("the menu offers the grammar's own keys, the company's fields last", () => {
  openMenu();
  expect(offered()).toEqual([
    "status=",
    "type=",
    "priority=",
    "assignee=",
    "tag=",
    "due=",
    "blocked=",
    "removed=",
    "f.area=",
    "f.urgent=",
    "f.reviewer=",
    "f.points=",
  ]);
  // AND AN ARCHIVED FIELD IS NOT OFFERED: its values left the value table, so
  // a filter over one matches nothing for ever.
  expect(offered()).not.toContain("f.legacy=");
});

// A FILTER THE MENU DOES NOT OFFER IS STILL A FILTER. `unit=` arrives from an
// item's own "Filed into" line rather than from a control — naming a team from
// a picker would need a list of unit KEYS, which the anonymous org projection
// this client reads does not carry — so the menu has no row for it, and its
// chip is how a reader sees it and takes it off.
test("a filter the menu does not offer has no row here", () => {
  openMenu();
  expect(offered()).not.toContain("unit=");
  expect(offered()).not.toContain("q=");
  // AND NEITHER THE SCOPE NOR THE ARRANGEMENT, which are the bar's and the
  // Display menu's: a second control for one key is two controls that disagree
  // the first time either writes.
  expect(offered()).not.toContain("scope=");
  expect(offered()).not.toContain("group_by=");
  expect(offered()).not.toContain("sort=");
  expect(offered()).not.toContain("shape=");
});

// NOT WHERE THE HOST HAS ALREADY ANSWERED IT. `#/me`'s Assigned tab IS one
// person's work, so an Assignee row there is a second answer to a question the
// screen has answered — and choosing it writes a key the lock overwrites on
// the way to the wire, which reads as a control that does nothing.
test("the locked assignee is not offered, and without a lock it is", () => {
  openMenu({ lockedAssignee: "rui" });
  expect(offered()).not.toContain("assignee=");
  cleanup();
  openMenu();
  expect(offered()).toContain("assignee=");
});

// THE CALENDAR SPENDS THE `due` KEY ITSELF — the grammar has exactly one, and
// the grid's window is written into it — so a due filter there would be a chip
// whose narrowing is overwritten on the way to the wire, which is how a reader
// concludes their filter matched everything.
test("the due filter is not offered on the calendar, whose own window is that key", () => {
  openMenu({ shape: "calendar" });
  expect(offered()).not.toContain("due=");
  cleanup();
  openMenu({ shape: "board" });
  expect(offered()).toContain("due=");
});

// A TAG SET BELONGS TO A PROJECT. On the company-wide list there is no set to
// offer, and a free-text tag box would offer every misspelling.
test("tags are offered only where the project's own set is known", () => {
  openMenu({ tags: undefined });
  expect(offered()).not.toContain("tag=");
  cleanup();
  openMenu({ tags: [] });
  expect(offered()).not.toContain("tag=");
});

// ---------------------------------------------------------------------------
// What a row writes
// ---------------------------------------------------------------------------

/** One value of one field, as the menu offers it and as the grammar takes it. */
const PICKS: { field: string; option: string; param: string; value: string }[] = [
  { field: "Status", option: "In progress", param: "status", value: "in_progress" },
  { field: "Type", option: "Bug", param: "type", value: "bug" },
  { field: "Priority", option: "High", param: "priority", value: "high" },
  // UNASSIGNED IS A VALUE the grammar spells `none`, and it is the one a lead
  // opens a board to ask for.
  { field: "Assignee", option: "Unassigned", param: "assignee", value: "none" },
  { field: "Assignee", option: "Ada Okonkwo", param: "assignee", value: "ada" },
  { field: "Tag", option: "Platform", param: "tag", value: "platform" },
  // A DUE ALIAS THE ENGINE EXPANDS ITSELF, never a window this control
  // composed: `internal/tracker/dates.go` resolves the word on the chip.
  { field: "Due", option: "Overdue", param: "due", value: "overdue" },
  { field: "Area", option: "API", param: "f.area", value: "api" },
  { field: "Urgent", option: "Yes", param: "f.urgent", value: "true" },
  { field: "Reviewer", option: "Rui Santos", param: "f.reviewer", value: "rui" },
  // `null` AND `not_null` ARE ON EVERY TYPE, because "is this set at all" is a
  // question about the ROW rather than about the value.
  { field: "Area", option: "Is set", param: "f.area", value: "not_null" },
  { field: "Points", option: "Is not set", param: "f.points", value: "null" },
];

// EACH ROW WRITES ITS OWN KEY AND NOTHING ELSE. One gesture is one key: a menu
// that moved a second would be a control whose effect the chips cannot state,
// and there would be no chip to take that second narrowing off with.
test("every value the menu offers writes exactly its own key", () => {
  for (const { field, option, param, value } of PICKS) {
    const onSet = openMenu();
    if (field === "Points") {
      // A TYPED FIELD'S ROW-LEVEL VALUES are the two set questions; the rest
      // is the box below.
      fireEvent.click(row(field));
      fireEvent.click(row(option));
    } else {
      pick(field, option);
    }
    expect(onSet.mock.calls, `${field} → ${option}`).toEqual([[param, value]]);
    cleanup();
  }
});

// A SWITCH HAS NO SECOND STAGE: there is one thing to say about it and the row
// says it, so the panel closes on the answer rather than showing a list of two.
test("a switch is one row, and pressing it again takes it off", () => {
  const on = openMenu();
  fireEvent.click(row("Blocked"));
  expect(on.mock.calls).toEqual([["blocked", "true"]]);
  cleanup();

  const off = openMenu({ filters: { ...NO_FILTERS, blocked: true } });
  fireEvent.click(row("Blocked"));
  expect(off.mock.calls).toEqual([["blocked", ""]]);
  cleanup();

  const trash = openMenu();
  fireEvent.click(row("Removed items"));
  expect(trash.mock.calls).toEqual([["removed", "true"]]);
});

// CHOOSING WHAT IS ALREADY CHOSEN TAKES IT OFF, which is the only way back to
// "any" without leaving the panel — and it is what the pressed state promises.
test("choosing the value already on clears the key, and says so before it is pressed", () => {
  const onSet = openMenu({ filters: { ...NO_FILTERS, status: "in_progress" } });
  fireEvent.click(row("Status"));
  expect(row("In progress").getAttribute("aria-pressed")).toBe("true");
  expect(row("To do").getAttribute("aria-pressed")).toBe("false");
  fireEvent.click(row("In progress"));
  expect(onSet.mock.calls).toEqual([["status", ""]]);
});

// A FIELD THAT IS ON SAYS SO ON ITS ROW, so a reader hunting an empty board
// does not have to open each one to find out which is narrowing.
test("a field with a value is marked in the list", () => {
  openMenu({ filters: { ...NO_FILTERS, priority: "high", fields: { "f.area": "api" } } });
  expect(row("Priority").textContent).toContain("on");
  expect(row("Area").textContent).toContain("on");
  expect(row("Status").textContent).not.toContain("on");
});

// A FIELD WITH NO DECLARED VALUES TAKES THE GRAMMAR VERBATIM, and the
// placeholder is where that grammar is stated: the operators a type admits are
// a table in `internal/tracker/coerce.go`, and an operator control here would
// be a second copy of it — where a wrong operator is not an error but a clause
// that matches nothing.
test("a typed field submits what was typed, under its own key", () => {
  const onSet = openMenu();
  fireEvent.click(row("Points"));
  const box = screen.getByLabelText("Points filter") as HTMLInputElement;
  expect(box.getAttribute("placeholder")).toBe("12, or gte:3, or range:3..8");
  fireEvent.change(box, { target: { value: "  gte:3  " } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  // TRIMMED, because a value with a space around it is a clause the engine
  // compares literally.
  expect(onSet.mock.calls).toEqual([["f.points", "gte:3"]]);
});

// AND THE WAY OFF A TYPED FILTER IS A CONTROL RATHER THAN AN EMPTY SUBMIT: the
// box opens holding what is applied, so clearing it by hand and pressing Apply
// is a gesture a reader has to invent.
test("a typed field that is on offers the control that removes it", () => {
  const onSet = openMenu({ filters: { ...NO_FILTERS, fields: { "f.points": "gte:3" } } });
  fireEvent.click(row("Points"));
  expect((screen.getByLabelText("Points filter") as HTMLInputElement).value).toBe("gte:3");
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  expect(onSet.mock.calls).toEqual([["f.points", ""]]);
  cleanup();
  // WITH NOTHING APPLIED THERE IS NOTHING TO REMOVE, so the control is not
  // drawn: a Remove over a filter that is off reports a state nobody can reach.
  openMenu();
  fireEvent.click(row("Points"));
  expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
});

// "IS IT SET AT ALL" IS A QUESTION ABOUT THE ROW, so it is on every type — the
// engine says so beside its own per-type operator table. A typed field's
// placeholder states the comparisons its TYPE admits and cannot state this
// one, so the two rows are offered under the box: without them, "which tasks
// have no estimate" was reachable only by a reader who happened to know the
// word to type.
test("a typed field offers the two set questions beside its box", () => {
  const onSet = openMenu();
  fireEvent.click(row("Points"));
  expect(screen.getByLabelText("Points filter")).toBeTruthy();
  expect(
    [...document.querySelectorAll(".work-menu-row .truncate")].map((el) => el.textContent),
  ).toEqual(["Is set", "Is not set"]);
  fireEvent.click(row("Is set"));
  expect(onSet.mock.calls).toEqual([["f.points", "not_null"]]);
  cleanup();

  // AND THE ONE THAT IS ON IS THE PRESSED ROW, pressing it again taking it
  // off — the same promise the listed fields' rows make.
  const off = openMenu({ filters: { ...NO_FILTERS, fields: { "f.points": "null" } } });
  fireEvent.click(row("Points"));
  expect(row("Is not set").getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(row("Is not set"));
  expect(off.mock.calls).toEqual([["f.points", ""]]);
});

// AN ARCHIVED OPTION IS NOT OFFERED, for the reason an archived field is not:
// a value nothing can hold any more is a filter that matches nothing. The SLUG
// is what the grammar compares and the NAME is what the company calls it.
test("an archived option and an archived type are left out of their lists", () => {
  openMenu();
  fireEvent.click(row("Area"));
  expect(
    [...document.querySelectorAll(".work-menu-row .truncate")].map((el) => el.textContent),
  ).toEqual(["API", "Is set", "Is not set"]);
  cleanup();

  openMenu();
  fireEvent.click(row("Type"));
  expect(
    [...document.querySelectorAll(".work-menu-row .truncate")].map((el) => el.textContent),
  ).toEqual(["Task", "Bug"]);
});

// A PEOPLE FIELD'S VALUES ARE THE ROSTER, which the panel already holds for
// the assignee row — so it is a list rather than a box where a reader guesses
// at a handle.
test("a people field is offered as the roster, plus the two set questions", () => {
  openMenu();
  fireEvent.click(row("Reviewer"));
  expect(
    [...document.querySelectorAll(".work-menu-row .truncate")].map((el) => el.textContent),
  ).toEqual(["Ada Okonkwo", "Rui Santos", "Is set", "Is not set"]);
});

// ---------------------------------------------------------------------------
// The status filter and the scope segment are two keys
// ---------------------------------------------------------------------------

// THE MENU OFFERS THE ENGINE'S SIX AND WRITES `status=`, NEVER `status_group=`.
// The segment in the bar owns that second key — [SCOPE_GROUPS] is its
// spelling — so a menu writing it would be two controls for one wire
// parameter with a precedence rule nothing on screen could state.
test("the status rows are the engine's six, and none of them writes the scope's key", () => {
  const onSet = openMenu();
  fireEvent.click(row("Status"));
  expect(
    [...document.querySelectorAll(".work-menu-row .truncate")].map((el) => el.textContent),
  ).toEqual(STATUSES.map((s) => s.label));
  fireEvent.click(row("Done"));
  expect(onSet.mock.calls).toEqual([["status", "done"]]);
});

// AND BOTH KEYS REACH THE WIRE, neither overwriting the other: asking for
// finished work by status is a narrowing, and which HALF of the company is on
// screen is the segment's. A menu that offered only the statuses the current
// segment admits would make "Status is Done" unreachable without first
// changing a control that is one press away and visible.
test("a status the menu wrote and the segment's own group are two parameters", () => {
  const filters: TrackerFilters = { ...NO_FILTERS, status: "done", scope: "closed" };
  const params = buildItemsParams({ container: "workspace", shape: "list", view: {}, filters });
  expect(params.status).toBe("done");
  expect(params.status_group).toBe(SCOPE_GROUPS.closed);
  // THE SEGMENT THAT INCLUDES FINISHED WORK CARRIES `show_closed` with it —
  // the group predicate is ANDed unconditionally otherwise.
  expect(params.show_closed).toBe("true");
});

// ---------------------------------------------------------------------------
// The panel itself
// ---------------------------------------------------------------------------

// THE PANEL CLOSES ON THE ANSWER AND OPENS BACK ON THE FIELD LIST. A menu that
// reopened on the values of whichever field was last touched would greet the
// next reader with the second stage of a question they had already answered —
// and the back control is the field name, so there would be nothing saying
// which question that was.
test("the panel closes on an answer and reopens at the field list", () => {
  openMenu();
  pick("Status", "To do");
  expect(document.querySelector(".work-menu")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  expect(screen.getByLabelText("Find a field")).toBeTruthy();
  expect(offered()).toContain("status=");
});

// THE FIND BOX NARROWS THE LIST, which is what makes a menu with no ceiling
// usable: a company's own fields are rows beside the standard ones, and there
// is no limit on how many it declares.
test("finding a field narrows the list, and says so when nothing matches", () => {
  openMenu();
  const find = screen.getByLabelText("Find a field");
  fireEvent.change(find, { target: { value: "rev" } });
  expect(offered()).toEqual(["f.reviewer="]);
  fireEvent.change(find, { target: { value: "nothing by that name" } });
  expect(offered()).toEqual([]);
  expect(screen.getByText("No field by that name.")).toBeTruthy();
});
