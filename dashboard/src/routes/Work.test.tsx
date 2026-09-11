/**
 * What the board screen derives, and why deriving it from `items` alone was a
 * lie rather than a gap.
 *
 * A grouped answer replaces `items` with `groups` — returning both would be
 * the same rows twice — so every number the header shows and the empty panel's
 * own guard had to learn about the second shape. They did not: a fully
 * populated board reported 0 shown, 0 in progress and 0 blocked, and drew "No
 * work has been filed yet" directly underneath its own columns.
 */

import { expect, test } from "vitest";
import { shownRows } from "./Work.tsx";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

const row = (id: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id,
  key: "ENG-" + id,
  project: "ENG",
  title: id,
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

test("a flat answer's rows are its items", () => {
  const items = [row("a"), row("b")];
  expect(shownRows(items, [])).toEqual(items);
});

test("a grouped answer's rows are its columns' rows, and items is empty", () => {
  const groups: WorkGroup[] = [
    { key: "todo", count: 40, rows: [row("a"), row("b")] },
    { key: "in_progress", count: 7, rows: [row("c")] },
  ];
  // THE SERVER SENDS NO `items` HERE — that is the whole shape of a grouped
  // answer — so a screen reading `items` sees an empty board.
  expect(shownRows([], groups).map((r) => r.id)).toEqual(["a", "b", "c"]);
});

test("a subgroup's rows are not counted twice", () => {
  const groups: WorkGroup[] = [
    {
      key: "todo",
      count: 2,
      rows: [row("a"), row("b")],
      subgroups: [{ key: "api", count: 1, rows: [row("a")] }],
    },
  ];
  expect(shownRows([], groups).map((r) => r.id)).toEqual(["a", "b"]);
});
