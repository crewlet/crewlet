/**
 * Which projects nest under the person sidebar's Work row: the reader's own,
 * at most six, alphabetical — and never the company's whole list, which is
 * one row further, "All projects".
 */

// @vitest-environment node

import { describe, expect, test } from "vitest";
import type { WorkProjectRow } from "~/protocol/index.ts";
import { YOUR_PROJECTS, yourProjects } from "./projects.ts";

function project(key: string, over: Partial<WorkProjectRow> = {}): WorkProjectRow {
  return {
    key,
    name: `${key} project`,
    unit: { resolved: true },
    lead: {},
    task_counts: { todo: 0, active: 0, done: 0, closed: 0 },
    version: 1,
    ...over,
  } as WorkProjectRow;
}

const NONE = new Set<string>();

describe("the reader's projects", () => {
  // EACH OF THE THREE WAYS IN, alone: a project nobody here leads, owned by
  // another unit, holding none of the reader's work is not theirs.
  test("a project is yours by lead, by line, by unit or by the work you hold", () => {
    const projects = [
      project("LEAD", { lead: { handle: "ada" } }),
      project("LINE", { lead: { handle: "rui" } }),
      project("UNIT", { unit: { resolved: true, name: "Platform" } }),
      project("HELD"),
      project("ELSE", { lead: { handle: "cy" }, unit: { resolved: true, name: "Sales" } }),
    ];
    const mine = yourProjects({
      projects,
      handle: "ada",
      units: ["Engineering", "platform"],
      line: ["rui"],
      holding: new Set(["HELD"]),
    });
    expect(mine.map((p) => p.key).sort()).toEqual(["HELD", "LEAD", "LINE", "UNIT"]);
  });

  // AT MOST SIX, IN THE ORDER OF THEIR NAMES — never by activity, which would
  // move the row a reader is reaching for between the decision and the press.
  test("at most six, alphabetical by name", () => {
    const projects = ["H", "C", "A", "G", "E", "B", "F", "D"].map((key) =>
      project(key, { lead: { handle: "ada" }, name: `${key} name` }),
    );
    const mine = yourProjects({ projects, handle: "ada", units: [], line: [], holding: NONE });
    expect(mine.map((p) => p.key)).toEqual(["A", "B", "C", "D", "E", "F"]);
    expect(mine).toHaveLength(YOUR_PROJECTS);
  });

  // AN ARCHIVED PROJECT IS NOT A PLACE ANYBODY GOES, whoever leads it.
  test("an archived project is never one of yours", () => {
    const mine = yourProjects({
      projects: [project("OLD", { lead: { handle: "ada" }, archived: true })],
      handle: "ada",
      units: [],
      line: [],
      holding: new Set(["OLD"]),
    });
    expect(mine).toEqual([]);
  });

  // A KEY NO SEAT NAMES has no lead, no unit and no line: only the work it
  // holds can make a project its own, and an empty handle leads nothing.
  test("a reader with no seat is in no project by lead", () => {
    const mine = yourProjects({
      projects: [project("ANON", { lead: {} }), project("LED", { lead: { handle: "" } })],
      handle: "",
      units: [],
      line: [],
      holding: NONE,
    });
    expect(mine).toEqual([]);
  });
});
