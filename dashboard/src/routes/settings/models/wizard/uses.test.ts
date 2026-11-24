/**
 * What a change of a model's uses moves, as Review lists it before a write:
 * each seat that moves, from where to where, and nothing else; the helpers a
 * first model takes are only the ones running on nothing.
 */

import { expect, test } from "vitest";
import { CATEGORY_JOBS, noUses, usesForIntent, usesOf, type Uses } from "~/lib/modelUses.ts";
import {
  EVERY_JOB,
  modelEntry,
  modelsData,
  runs,
  seat,
} from "~/routes/settings/models/fixtures.ts";
import { everythingHelpers, seatChangesOf } from "./uses.ts";

const data = modelsData({
  entries: [
    modelEntry({
      key: "default",
      used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("pm", ...EVERY_JOB)],
    }),
  ],
  assistant: { key: "default" },
});

/** `default` runs every job for the engineer and QA, `default-host` for the PM and the designer. */
const split = modelsData({
  entries: [
    modelEntry({
      key: "default",
      used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("qa", ...EVERY_JOB)],
    }),
    modelEntry({
      key: "default-host",
      used_by: [...runs("pm", ...EVERY_JOB), ...runs("designer", ...EVERY_JOB)],
    }),
  ],
  seats: [
    seat("backend-engineer", "Backend Engineer"),
    seat("qa", "Quality Lead"),
    seat("pm", "Product Manager"),
    seat("designer", "Designer"),
  ],
});

test("the changes list names each seat that moves, where from and where to, and nothing else", () => {
  const today = usesOf(split.models.find((m) => m.key === "default")!.entry.used_by);
  expect(seatChangesOf(split, "default", today, today)).toEqual([]);
  // The PM's coding added, QA's routines taken off.
  const next: Uses = { ...today, jobs: { ...today.jobs } };
  next.jobs.executor = [...today.jobs.executor, "pm"].sort();
  for (const job of CATEGORY_JOBS.routines)
    next.jobs[job] = today.jobs[job].filter((h) => h !== "qa");
  expect(seatChangesOf(split, "default", today, next)).toEqual([
    {
      key: "qa",
      subject: "Quality Lead",
      rows: [{ kind: "removed", label: "Routines and evaluations", from: "default" }],
    },
    {
      key: "pm",
      subject: "Product Manager",
      rows: [{ kind: "changed", label: "Coding", from: "default-host", to: "default" }],
    },
  ]);
});

test("the changes list says Ask Crewlet's model moving, and only that", () => {
  const ask = usesForIntent("assistant", [], true);
  const helpers = (row: object) => [{ key: "helpers", subject: "Helpers", rows: [row] }];
  expect(seatChangesOf(data, "big", noUses(), ask)).toEqual(
    helpers({ kind: "changed", label: "Ask Crewlet", from: "default", to: "big" }),
  );
  expect(seatChangesOf({ ...data, assistant: { key: null } }, "big", noUses(), ask)).toEqual(
    helpers({ kind: "added", label: "Ask Crewlet", to: "big" }),
  );
});

test("the changes list says the Advisor moving onto a model, and taken off one", () => {
  const d = { ...data, advisor: { key: "sonnet" } };
  const helpers = (row: object) => [{ key: "helpers", subject: "Helpers", rows: [row] }];
  expect(seatChangesOf(d, "fast", noUses(), { ...noUses(), advisor: true })).toEqual(
    helpers({ kind: "changed", label: "Advisor", from: "sonnet", to: "fast" }),
  );
  expect(seatChangesOf(d, "sonnet", { ...noUses(), advisor: true }, noUses())).toEqual(
    helpers({
      kind: "removed",
      label: "Advisor",
      from: "sonnet",
      note: "Its checks run with no model",
    }),
  );
});

test("everythingHelpers takes a helper with no model or one on a model gone, never one on a model", () => {
  const at = (assistant: string | null, advisor: string | null, harness = "claude-code") =>
    everythingHelpers(
      { ...split, assistant: { key: assistant }, advisor: { key: advisor } },
      harness,
    );
  expect(at(null, null)).toEqual({ assistant: true, advisor: true });
  expect(at("default", "gone")).toEqual({ assistant: false, advisor: true });
  expect(at("gone", "default-host")).toEqual({ assistant: true, advisor: false });
  expect(at(null, null, "opencode")).toEqual({ assistant: false, advisor: false });
});
