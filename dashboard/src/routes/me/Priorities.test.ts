/**
 * What the priorities list says about itself — the line that tells a person
 * why the Queue's count and the rows under it are two different numbers.
 */

import { describe, expect, test } from "vitest";
import { listLine } from "./Priorities.tsx";
import type { WorkSummary } from "~/protocol/index.ts";

/** A row assigned to `assignee`, or to nobody. */
function row(key: string, assignee?: string): WorkSummary {
  return {
    id: key,
    key,
    project: "ENG",
    title: key,
    type: "task",
    status: "todo",
    assignee,
  } as WorkSummary;
}

describe("the priorities line", () => {
  test("a list holding colleagues' work says how much of it is the person's, and what the Queue counts", () => {
    expect(
      listLine({
        rows: [row("A", "rui"), row("B", "ada"), row("C", "rui"), row("D", "ada"), row("E")],
        total: 5,
        whose: "ada",
        they: "you",
      }),
    ).toBe(
      "5 open tasks on your list, in the order to work them — 2 of them assigned to you. The Queue counts only the work assigned to you.",
    );
  });

  // A TASK NOBODY HOLDS IS NOT SOMEBODY ELSE'S. The split names only what is
  // the person's, because the rest is not all "held by others".
  test("none of the person's own is said as none, not as somebody else's", () => {
    expect(
      listLine({ rows: [row("A"), row("B", "rui")], total: 2, whose: "ada", they: "you" }),
    ).toBe(
      "2 open tasks on your list, in the order to work them — none assigned to you. The Queue counts only the work assigned to you.",
    );
  });

  test("a list that is all the person's own says so and says nothing about the Queue", () => {
    expect(
      listLine({ rows: [row("A", "ada"), row("B", "ada")], total: 2, whose: "ada", they: "you" }),
    ).toBe("2 open tasks on your list, in the order to work them — all assigned to you.");
    expect(listLine({ rows: [row("A", "ada")], total: 1, whose: "ada", they: "you" })).toBe(
      "1 open task on your list, in the order to work them — it is assigned to you.",
    );
  });

  // THE SPLIT IS OVER WHAT IS DRAWN. Past a page, whose each hidden entry is
  // is not on the wire, so the line says "of the N shown" rather than
  // claiming the whole list.
  test("past a page the split is of the rows shown, under the whole count", () => {
    const rows = Array.from({ length: 20 }, (_, i) => row(`T${i}`, i < 8 ? "ada" : "rui"));
    expect(listLine({ rows, total: 25, whose: "ada", they: "you" })).toBe(
      "25 open tasks on your list, in the order to work them. 8 of the 20 shown are assigned to you. The Queue counts only the work assigned to you.",
    );
  });

  test("on somebody else's day it is their list and their queue", () => {
    expect(
      listLine({ rows: [row("A", "rui"), row("B", "bo")], total: 2, whose: "rui", they: "them" }),
    ).toBe(
      "2 open tasks on their list, in the order to work them — 1 of them assigned to them. The Queue counts only the work assigned to them.",
    );
  });
});
