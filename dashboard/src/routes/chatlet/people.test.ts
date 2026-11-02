/**
 * Who chat names, and how.
 *
 * ONE RULE WORTH A TEST ABOVE THE OTHERS: a handle the chart does not hold is
 * still drawn as somebody. A message outlives the seat that wrote it, and a
 * row that dropped its author, or borrowed a colleague's face for it, would be
 * a transcript that lies about who spoke.
 */

import { describe, expect, test } from "vitest";

import { chatPeople, handlesOf, matchesPerson } from "./people.ts";
import type { Seat } from "~/lib/seats.ts";
import type { AgentRow } from "~/protocol/index.ts";

/** A seat as the org index reports one, cut to what this module reads. */
const seat = (handle: string, name: string, kind: "agent" | "human", team = "") =>
  ({
    handle,
    name,
    kind,
    avatar: kind === "agent" ? { character: "hexlet", color: "blue" } : null,
    unit: team ? { name: team } : null,
  }) as unknown as Seat;

/** A roster row, cut to what the state mappers read. */
const row = (id: string, activity: string, extra: Partial<AgentRow> = {}) =>
  ({ id, activity, ...extra }) as unknown as AgentRow;

describe("who is listed", () => {
  test("a seat the engine reports no handle for is not listed at all", () => {
    // A handle is DERIVED by the engine and this client deliberately does not
    // re-implement that rule, so a seat without one cannot be named, and a
    // row whose value is "" would be a participant the write path drops.
    const people = chatPeople(
      { seats: [seat("", "Nameless", "agent"), seat("bo", "Bo", "human")] },
      [],
    );
    expect(people.everyone.map((person) => person.handle)).toEqual(["bo"]);
  });

  test("people come before agents, and each group is alphabetical", () => {
    const people = chatPeople(
      {
        seats: [
          seat("zoe", "Zoe", "human"),
          seat("ops", "Ops", "agent"),
          seat("ann", "Ann", "human"),
          seat("bot", "Bot", "agent"),
        ],
      },
      [],
    );
    expect(people.everyone.map((person) => person.name)).toEqual(["Ann", "Zoe", "Bot", "Ops"]);
  });

  test("an agent carries its drawing and its team, a person carries no drawing", () => {
    const people = chatPeople(
      { seats: [seat("ops", "Ops", "agent", "Platform"), seat("ann", "Ann", "human", "Founders")] },
      [],
    );
    expect(people.person("ops")).toMatchObject({
      avatar: { character: "hexlet" },
      team: "Platform",
    });
    expect(people.person("ann")).toMatchObject({ avatar: null, team: "Founders" });
  });
});

describe("what a seat is doing", () => {
  test("a working agent says what it is working on and wears the working badge", () => {
    const people = chatPeople({ seats: [seat("ops", "Ops", "agent")] }, [
      row("ops", "working", {
        current_phase: "execute",
        live_call: { work_item: { id: "w1", key: "ENG-4" } },
      } as Partial<AgentRow>),
    ]);
    expect(people.person("ops")).toMatchObject({
      state: "working",
      status: "working",
      doing: "Executing ENG-4",
    });
  });

  test("an idle agent wears no badge and says nothing that would tick", () => {
    const people = chatPeople({ seats: [seat("ops", "Ops", "agent")] }, [row("ops", "idle")]);
    expect(people.person("ops")).toMatchObject({ state: "idle", status: undefined, doing: "" });
  });

  test("a paused agent is told apart from a stopped one and wears the stop badge", () => {
    const people = chatPeople({ seats: [seat("ops", "Ops", "agent")] }, [
      row("ops", "stopped", { stopped_reason: "paused" } as Partial<AgentRow>),
    ]);
    expect(people.person("ops")).toMatchObject({ state: "paused", status: "stopped" });
  });

  test("a person is not run by the engine, so a roster row under their handle is not read", () => {
    const people = chatPeople({ seats: [seat("ann", "Ann", "human")] }, [row("ann", "working")]);
    expect(people.person("ann")).toMatchObject({ state: "offline", status: undefined });
  });
});

describe("a handle nobody answers to", () => {
  test("is drawn under the handle, with the kind the message recorded", () => {
    const people = chatPeople({ seats: [] }, []);
    expect(people.person("gone", "human")).toMatchObject({
      name: "gone",
      kind: "human",
      known: false,
    });
    // An operator credential is drawn as a person, never as an agent.
    expect(people.person("ci", "operator").kind).toBe("human");
    // With nothing recorded, the kit's own default: the engine runs agents.
    expect(people.person("old-seat").kind).toBe("agent");
    expect(people.nameOf("old-seat")).toBe("old-seat");
  });
});

describe("finding somebody", () => {
  const people = chatPeople(
    { seats: [seat("sbe", "Senior Backend Engineer", "agent", "Platform")] },
    [],
  );
  const sbe = people.person("sbe");

  test.each([
    ["backend", true],
    ["SBE", true],
    ["platform", true],
    ["  ", true],
    ["design", false],
  ])("%j matches: %s", (query, expected) => {
    expect(matchesPerson(sbe, query)).toBe(expected);
  });
});

describe("what is sent", () => {
  test("a handle chosen twice is one participant", () => {
    // The id of a direct conversation is derived from the SET of handles, so
    // a duplicate changes nothing at the engine and would only be a second
    // row in a request.
    expect(handlesOf(["bo", "bo", " cas ", ""])).toEqual(["bo", "cas"]);
  });
});
