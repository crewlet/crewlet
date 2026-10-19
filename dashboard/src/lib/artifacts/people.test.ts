/**
 * A room's members as the chrome draws them: the phrase the People rail and
 * the presence stack print, and the turn an agent offers.
 */

import { expect, test } from "vitest";

import {
  artifactPeople,
  audienceName,
  memberTurn,
  presenceMembers,
  selfWords,
  splitOwnSessions,
} from "./people.ts";
import type { ArtifactPresenceMember } from "~/protocol/index.ts";

function member(over: Partial<ArtifactPresenceMember>): ArtifactPresenceMember {
  return {
    session: "s-1",
    seat: "maya",
    kind: "human",
    activity: "viewing",
    focus: {},
    at: "2026-10-18T10:00:00Z",
    ...over,
  };
}

const room = [
  member({
    session: "a",
    activity: "editing",
    focus: { collection: "review", doc: "r1", field: "score" },
  }),
  member({
    session: "b",
    seat: "pm",
    kind: "agent",
    activity: "viewing",
    turn: "run-7",
    focus: { version: 3 },
  }),
  member({ session: "c", seat: "jane", activity: "idle" }),
];

// THE WHOLE PHRASE, VERB FIRST: uilet prints a member's location in place of
// the activity, so a bare "on r1, score" would lose what they are doing.
test("a member's location says what they are doing and where", () => {
  const { members } = presenceMembers(room, artifactPeople(null), new Map());
  expect(members.map((m) => m.location)).toEqual([
    "Editing a row, a column",
    "Reading version 3",
    undefined,
  ]);
});

// A RAW KEY IS NEVER SAID: a row by its number as drawn, a column by its label.
test("a member on a cell is placed by the row's number and the column's label", () => {
  const { members } = presenceMembers(room, artifactPeople(null), new Map(), {
    row: (_, doc) => (doc === "r1" ? "row 4" : null),
    field: (_, field) => (field === "score" ? "Gate" : null),
  });
  expect(members[0]?.location).toBe("Editing row 4, Gate");
});

test("the reader's own other tab is counted beside You, never drawn as somebody else", () => {
  const { others, ownTabs } = splitOwnSessions(
    [
      { seat: "miles", session: "1" },
      { seat: "maya", session: "2" },
    ],
    "miles",
  );
  expect(others.map((m) => m.seat)).toEqual(["maya"]);
  expect(selfWords(ownTabs)).toBe("You, 2 tabs");
  expect(selfWords(0)).toBe("You");
});

test("only an agent at work offers its turn", () => {
  const roster = new Map(room.map((m) => [m.session, m]));
  const { members } = presenceMembers(room, artifactPeople(null), new Map());
  expect(members.map((m) => memberTurn(roster, m))).toEqual(["", "run-7", ""]);
});

test("a share's audience is named by its seat, its team or the company, a handle's own colon kept", () => {
  const people = {
    ...artifactPeople(null),
    company: "Acme",
    seatName: (handle: string) => (handle === "token:maya" ? "Maya Ortiz" : handle),
    unitName: (key: string) => (key === "ops" ? "Operations" : key),
  };
  expect(audienceName("seat:token:maya", people)).toBe("Maya Ortiz");
  expect(audienceName("unit:ops", people)).toBe("Operations");
  expect(audienceName("company", people)).toBe("everyone in Acme");
});
