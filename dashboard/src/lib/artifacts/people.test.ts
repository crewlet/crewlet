/**
 * A room's members as the chrome draws them: the phrase the People rail and
 * the presence stack print, and the turn an agent offers.
 */

import { expect, test } from "vitest";

import { artifactPeople, memberTurn, presenceMembers } from "./people.ts";
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
    "Editing on r1, score",
    "Reading on version 3",
    undefined,
  ]);
});

test("only an agent at work offers its turn", () => {
  const roster = new Map(room.map((m) => [m.session, m]));
  const { members } = presenceMembers(room, artifactPeople(null), new Map());
  expect(members.map((m) => memberTurn(roster, m))).toEqual(["", "run-7", ""]);
});
