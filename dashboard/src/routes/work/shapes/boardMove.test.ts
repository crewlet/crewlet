/**
 * What a drop asks the engine to do — each rule with the wrong form that
 * would move a DIFFERENT task, or the right task somewhere else.
 */

import { expect, test } from "vitest";

import { keyboardDrop, moveFor, withDrop } from "./boardMove.ts";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

const row = (key: string, project = "ENG", version = 4): WorkSummary => ({
  id: key.toLowerCase(),
  key,
  project,
  title: key,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version,
});

const lanes = (): WorkGroup[] => [
  { key: "todo", count: 3, rows: [row("ENG-1"), row("OPS-1", "OPS"), row("ENG-2")] },
  { key: "in_progress", count: 1, rows: [row("ENG-3")] },
  { key: "done", count: 0, rows: [] },
];

// A RANK IS AN ORDER WITHIN ONE PROJECT, so on the company-wide board a card
// from another project is no neighbour: the drop is placed against the
// nearest card of its own project, or not at all.
test("a neighbour from another project is never named", () => {
  // Dropped above OPS-1 within its own lane: no neighbour it can rank against.
  expect(moveFor({ id: "eng-2", lane: "todo", above: "ops-1" }, lanes(), "status")).toBeNull();
  // Dropped at the bottom of a lane: after the last card of ITS project.
  expect(moveFor({ id: "eng-1", lane: "todo", above: "" }, lanes(), "status")).toEqual({
    item: "ENG-1",
    after: "ENG-2",
    if_match: 4,
  });
});

// A LANE IS A STATUS ONLY ON THE STATUS AXIS. Between two assignee lanes a drop
// would be a reassignment the tool does not make, so it sends nothing.
test("a lane change on another axis sends nothing", () => {
  const byAssignee: WorkGroup[] = [
    { key: "ada", count: 1, rows: [row("ENG-1")] },
    { key: "rui", count: 1, rows: [row("ENG-2")] },
  ];
  expect(moveFor({ id: "eng-1", lane: "rui", above: "" }, byAssignee, "assignee")).toBeNull();
  // AND A REORDER WITHIN ONE OF THEM IS STILL A PLACE.
  const one: WorkGroup[] = [{ key: "ada", count: 2, rows: [row("ENG-1"), row("ENG-2")] }];
  expect(moveFor({ id: "eng-2", lane: "ada", above: "eng-1" }, one, "assignee")).toEqual({
    item: "ENG-2",
    before: "ENG-1",
    if_match: 4,
  });
});

// THE KEYBOARD IS THE SAME GESTURE: up past the neighbour, down below the next,
// and sideways to the top of the lane beside — and nothing off the board's edge.
test("Alt with an arrow is a drop the pointer could have made", () => {
  // OVER THE OTHER PROJECT'S CARD, to its own neighbour — a step onto OPS-1
  // would be a move with nothing to rank against.
  expect(keyboardDrop("eng-2", lanes(), "up")).toEqual({
    id: "eng-2",
    lane: "todo",
    above: "eng-1",
  });
  expect(moveFor(keyboardDrop("eng-2", lanes(), "up")!, lanes(), "status")).toEqual({
    item: "ENG-2",
    before: "ENG-1",
    if_match: 4,
  });
  expect(keyboardDrop("eng-1", lanes(), "down")).toEqual({
    id: "eng-1",
    lane: "todo",
    above: "",
  });
  expect(keyboardDrop("eng-3", lanes(), "right")).toEqual({
    id: "eng-3",
    lane: "done",
    above: "",
  });
  expect(keyboardDrop("eng-1", lanes(), "left")).toBeNull();
  expect(keyboardDrop("eng-1", lanes(), "up")).toBeNull();
});

// THE CARD IS DRAWN WHERE IT WAS DROPPED while the engine decides, and the
// two lanes' counts travel with it: one card left one lane and joined another,
// whatever else either holds. A heading still reading the old tally over a
// card already drawn beneath it is two claims that disagree.
test("an in-flight drop moves the card and the two counts it changes", () => {
  const drawn = withDrop(lanes(), { id: "eng-1", lane: "in_progress", above: "eng-3" });
  expect(drawn[0]!.rows.map((r) => r.key)).toEqual(["OPS-1", "ENG-2"]);
  expect(drawn[1]!.rows.map((r) => r.key)).toEqual(["ENG-1", "ENG-3"]);
  expect(drawn.map((g) => g.count)).toEqual([2, 2, 0]);
  // CLEARING THE DROP — a refusal — is every count as the engine gave it.
  expect(withDrop(lanes(), null)).toEqual(lanes());
});

// A REORDER WITHIN ONE LANE changes no tally at all.
test("an in-flight reorder leaves every count alone", () => {
  const drawn = withDrop(lanes(), { id: "eng-2", lane: "todo", above: "eng-1" });
  expect(drawn[0]!.rows.map((r) => r.key)).toEqual(["ENG-2", "ENG-1", "OPS-1"]);
  expect(drawn.map((g) => g.count)).toEqual([3, 1, 0]);
});

// A CARD IS ITS TASK, NOT ITS KEY. A key another task claimed first stays on
// the task that did not claim it (`key_collision`), so two cards on one board
// can carry it — and a drag that knew its card by the key lifted whichever of
// the two came first and sent the claimant's key, which the engine resolves to
// the claimant. The duplicate moves as itself, and is sent by its id.
test("of two cards under one key, the one dragged is the one moved", () => {
  const claimant = { ...row("ENG-7"), id: "t-claimant" };
  const duplicate = { ...row("ENG-7"), id: "t-duplicate", key_collision: true };
  const board: WorkGroup[] = [
    { key: "todo", count: 2, rows: [claimant, duplicate] },
    { key: "in_progress", count: 0, rows: [] },
  ];
  expect(moveFor({ id: "t-duplicate", lane: "in_progress", above: "" }, board, "status")).toEqual({
    item: "t-duplicate",
    status: "in_progress",
    if_match: 4,
  });
  // AND PLACED ABOVE THE CLAIMANT, it names the claimant by the key it holds.
  expect(
    moveFor({ id: "t-duplicate", lane: "todo", above: "t-claimant" }, board, "status"),
  ).toEqual({
    item: "t-duplicate",
    before: "ENG-7",
    if_match: 4,
  });
  const drawn = withDrop(board, { id: "t-duplicate", lane: "in_progress", above: "" });
  expect(drawn[0]!.rows.map((r) => r.id)).toEqual(["t-claimant"]);
  expect(drawn[1]!.rows.map((r) => r.id)).toEqual(["t-duplicate"]);
});
