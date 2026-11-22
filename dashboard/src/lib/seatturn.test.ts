/**
 * A seat on several turns at once: each turn is read with its own call, a list
 * of running turns draws each, and a seat on one turn reads as it always did.
 */

import { expect, test } from "vitest";

import { callOf, callOfTurn, eachTurn, narrowTo, turnOf } from "./seatturn.ts";
import { stateLine, workingLongestFirst } from "./seats.ts";
import { runningShortList, seatOnTurn, turnIdOf } from "./turns.ts";
import type { AgentRow, LiveCall, LiveTurn } from "~/protocol/index.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

const turn = (id: string, minute: number, over: Partial<LiveTurn> = {}): LiveTurn => ({
  turn_id: id,
  started_at: `2026-11-21T10:${String(minute).padStart(2, "0")}:00Z`,
  stage: "phase",
  ...over,
});

const call = (turnId: string, phase: string, over: Partial<LiveCall> = {}): LiveCall =>
  ({
    turn_id: turnId,
    phase,
    iteration: 0,
    in_progress: true,
    updated_at: "2026-11-21T10:30:00Z",
    versions: ZERO_VERSIONS,
    ...over,
  }) as LiveCall;

/** A working seat on turns, with calls. */
const seat = (turns: LiveTurn[], calls: LiveCall[]): AgentRow =>
  ({
    id: "swe",
    agent_id: "a-swe",
    role: "SWE",
    handle: "swe",
    activity: "working",
    turns,
    live_calls: calls,
  }) as AgentRow;

const TWO = seat(
  [
    turn("t-a", 1, { work_item: { backend: "native", id: "i-1", key: "ENG-1", project: "ENG" } }),
    turn("t-b", 5, { work_item: { backend: "native", id: "i-2", key: "ENG-2", project: "ENG" } }),
  ],
  [call("t-a", "execute"), call("t-b", "review")],
);

// TWO TURNS OF ONE SEAT ARE TWO ROWS, each with its own turn and call, in
// start order: every list of running turns draws both, keyed apart.
//
// Mutation: read a seat as its first turn alone in workingLongestFirst, and
// the second turn is never listed.
test("a seat on two turns is two running rows, each with its own call", () => {
  const rows = workingLongestFirst([TWO]);
  expect(rows.map(turnIdOf)).toEqual(["t-a", "t-b"]);
  expect(rows.map((r) => callOf(r)?.phase)).toEqual(["execute", "review"]);
  const { working, shown } = runningShortList([TWO]);
  expect(working).toHaveLength(2);
  expect(shown.map(turnIdOf)).toEqual(["t-a", "t-b"]);
});

// A PAGE ABOUT ONE TURN READS THE SEAT NARROWED TO IT: its stage, its call
// and its item, never the seat's other turn's.
//
// Mutation: have seatOnTurn return the seat unnarrowed, and t-b's page reads
// t-a's phase.
test("a seat is read through the one turn a page is about", () => {
  const on = seatOnTurn([TWO], "t-b");
  expect(turnOf(on)?.turn_id).toBe("t-b");
  expect(callOf(on)?.phase).toBe("review");
  expect(callOfTurn(TWO, "t-a")?.phase).toBe("execute");
  expect(narrowTo(TWO, "t-missing")).toBeNull();
});

// A SEAT'S ONE LINE SAYS IT IS ON SEVERAL TURNS, reading its first, so a card
// with room for one line still shows there is a second.
test("a seat's line names how many turns it is on", () => {
  expect(stateLine(TWO, { now: 0 })).toBe("Executing ENG-1 · 2 turns");
  const [first] = eachTurn(TWO);
  expect(stateLine(first, { now: 0 })).toBe("Executing ENG-1");
});

// AT ONE TURN NOTHING CHANGES: the row is its own only row, and a call whose
// turn ended (a failed phase's, kept on screen) is still the seat's call.
//
// Mutation: narrow a one-turn seat to a copy, or drop a call whose turn the
// seat no longer holds, and a seat at the default cap reads differently.
test("a seat on one turn, or none, reads as it always did", () => {
  const one = seat([turn("t-a", 1)], [call("t-a", "execute")]);
  expect(eachTurn(one)).toEqual([one]);
  expect(eachTurn(one)[0]).toBe(one);
  const frozen = seat([], [call("t-old", "execute", { in_progress: false, failed: true })]);
  expect(callOf(frozen)?.turn_id).toBe("t-old");
  expect(turnOf(frozen)).toBeNull();
});
