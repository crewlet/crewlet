/**
 * Who set each property, and — more importantly — when the honest answer is
 * nobody.
 *
 * The failure this guards is silent in both directions: a key naming no field
 * renders no line, and a rail that borrowed the oldest entry in the window
 * would render a confident sentence about a fact it cannot see.
 */

import { describe, expect, test } from "vitest";

import { CHANGE_FIELDS, attribution } from "./attribution.ts";
import type { WorkChange } from "~/protocol/index.ts";

function change(over: Partial<WorkChange>): WorkChange {
  return {
    id: over.id ?? "c1",
    kind: over.kind ?? "fields",
    at: over.at ?? "2026-03-01T09:00:00Z",
    log_seq: over.log_seq ?? 1,
    ...over,
  };
}

describe("the latest change to each property", () => {
  test("names the actor, the kind and the turn", () => {
    const who = attribution([
      change({
        actor: "ada",
        actor_kind: "agent",
        turn_id: "t-9",
        at: "2026-03-01T12:00:00Z",
        fields: { status: { from: "todo", to: "in_progress" } },
      }),
    ]);
    expect(who.get("status")).toEqual({
      actor: "ada",
      actorKind: "agent",
      turnId: "t-9",
      at: "2026-03-01T12:00:00Z",
    });
  });

  test("the newest change wins, and the input order is what decides", () => {
    // The server orders `log_seq DESC` — a position on one stream rather than
    // a clock, which is the ordering that holds when two nodes wrote in the
    // same second. So the FIRST entry naming a field is the answer.
    const who = attribution([
      change({ log_seq: 9, actor: "bo", fields: { assignee: { to: "bo" } } }),
      change({ log_seq: 4, actor: "ada", fields: { assignee: { to: "ada" } } }),
    ]);
    expect(who.get("assignee")?.actor).toBe("bo");
  });

  test("one change attributes every field it moved", () => {
    const who = attribution([
      change({ actor: "ada", fields: { due: {}, points: {}, sprint: {} } }),
    ]);
    expect([...who.keys()].sort()).toEqual(["due", "points", "sprint"]);
  });

  test("a create attributes what it set, which is why a fresh task has lines", () => {
    const who = attribution([
      change({ kind: "created", actor: "founder", actor_kind: "operator", fields: { title: {} } }),
    ]);
    expect(who.get("title")?.actor).toBe("founder");
  });
});

describe("when there is no honest answer", () => {
  test("a property no visible change names carries nothing", () => {
    // NOT the oldest entry in the window. `history` is the newest fifty
    // changes, so a property last moved before that is a fact about the page
    // size rather than about anybody.
    const who = attribution([change({ actor: "ada", fields: { status: {} } })]);
    expect(who.has("points")).toBe(false);
  });

  test("no history at all is an empty answer rather than a throw", () => {
    expect(attribution(undefined).size).toBe(0);
    expect(attribution([]).size).toBe(0);
  });

  test("a change with no actor attributes nothing", () => {
    // The line's whole content is who, and "set by" with nobody after it is
    // worse than silence.
    const who = attribution([change({ fields: { status: {} } })]);
    expect(who.size).toBe(0);
  });

  test("an actor kind the wire does not spell is dropped, not passed through", () => {
    const who = attribution([
      change({ actor: "ada", actor_kind: "robot", fields: { status: {} } }),
    ]);
    expect(who.get("status")?.actorKind).toBe(undefined);
  });
});

test("every field name is one the engine writes, spelled its way", () => {
  // The rail labels it "Estimate" and the task row calls the number
  // `estimate_minutes`; the history bag calls it `estimate`. Held against
  // `tracker.TaskDeltas` by `internal/tracker/attribution_test.go` — this half
  // only pins the shape so a rename here is visible in the diff.
  expect([...CHANGE_FIELDS]).toEqual([
    "title",
    "status",
    "assignee",
    "priority",
    "project",
    "type",
    "tags",
    "sprint",
    "due",
    "start",
    "estimate",
    "points",
  ]);
});
