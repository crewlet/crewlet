/**
 * How a turn's work item is named on a row: the key it recorded, or — for one
 * it recorded none for — its kind and the head of its id, with the whole
 * identity on the title.
 */

import { expect, test } from "vitest";

import { buildHash } from "~/app/router.tsx";
import {
  runningNow,
  runningShortList,
  runningTarget,
  turnIdOf,
  watchHref,
  watchLink,
  workItemLabel,
} from "./turns.ts";
import type { AgentRow, TurnRow } from "~/protocol/index.ts";

const ID = "4d631f6d-8a1b-4c2d-9e3f-0a1b2c3d4e5f";

test("a recorded key is the label", () => {
  expect(workItemLabel({ backend: "native", id: ID, key: "ENG-29", project: "ENG" }).text).toBe(
    "ENG-29",
  );
});

// 43 UNBREAKABLE CHARACTERS took a phone's whole cell and laid the summary
// beside it out one letter per line.
test("a keyless native task is named by its kind and the head of its uuid, whole on the title", () => {
  const label = workItemLabel({ backend: "native", id: ID, key: "", project: "ENG" });
  expect(label.text).toBe("task 4d631f6d");
  expect(label.title).toContain(`native:${ID}`);
});

test("a keyless external item keeps its backend and an id that is not a uuid whole", () => {
  expect(workItemLabel({ backend: "jira", id: "10042", key: "", project: "" }).text).toBe(
    "jira 10042",
  );
});

// ---------------------------------------------------------------------------
// A turn still running
// ---------------------------------------------------------------------------

const row = (over: Partial<TurnRow> = {}) =>
  ({ turn_id: "t-7", complete: false, parked: false, ...over }) as TurnRow;

const seat = (over: Partial<AgentRow> = {}) =>
  ({
    id: "a-swe",
    role: "SWE",
    turn: {
      turn_id: "t-7",
      stage: "phase",
      work_item: { backend: "native", id: ID, key: "ENG-32", project: "ENG" },
    },
    live_call: { turn_id: "t-7", phase: "execute", round_num: 1, max_rounds: 24 },
    ...over,
  }) as unknown as AgentRow;

// THE PUSH IS THE ONE SOURCE THAT KNOWS what a running turn is doing and what
// it is on; a settled row, and a row no seat is running, get nothing from it.
test("a running turn is what the seat running it is doing, on the item it is charged to", () => {
  expect(runningNow(row(), [seat()])).toEqual({
    words: "running — executing · round 2 of 24",
    item: { backend: "native", id: ID, key: "ENG-32", project: "ENG" },
  });
});

test("a turn that ended, or that no seat is running, is not running now", () => {
  expect(runningNow(row({ complete: true }), [seat()])).toBeNull();
  expect(runningNow(row({ turn_id: "t-dead" }), [seat()])).toBeNull();
});

test("a turn parked on its coding run says so", () => {
  const parked = seat({ turn: { turn_id: "t-7", stage: "parked" } } as Partial<AgentRow>);
  expect(runningNow(row({ parked: true }), [parked])?.words).toBe("parked — coding run");
});

// ---------------------------------------------------------------------------
// Watching one
// ---------------------------------------------------------------------------

// A WATCH LINK IS THE TURN'S OWN PAGE ON ITS TRANSCRIPT, where the phase it is
// on is open — and both spellings of it are one address.
test("a watch link is the turn's page on its transcript, as a path and as an href", () => {
  expect(watchLink("t-7")).toEqual({
    path: ["live", "turns", "t-7"],
    query: { tab: "transcript" },
  });
  expect(watchHref("t-7")).toBe(buildHash(["live", "turns", "t-7"], { tab: "transcript" }));
});

// THE TURN RECORD FIRST, as `seatOnTurn` reads it, and nothing — never a link
// to `#/live/turns/` — while the overlay names no turn yet.
test("the turn a seat is on is its turn record's, then its call's, else none", () => {
  expect(turnIdOf(seat())).toBe("t-7");
  expect(turnIdOf(seat({ turn: null }))).toBe("t-7");
  expect(
    turnIdOf(
      seat({ turn: { turn_id: "t-8", stage: "phase" } as AgentRow["turn"], live_call: null }),
    ),
  ).toBe("t-8");
  expect(turnIdOf(seat({ turn: null, live_call: null }))).toBe("");
  expect(turnIdOf(undefined)).toBe("");
});

// `g r`: THE running turn when there is exactly one, and every running turn —
// Now running — when there are several or none. A seat that is working but
// names no turn yet has no page to watch, and a seat that is not working is no
// running turn at all.
test("the running target is the one turn's watch link, else Now running", () => {
  const working = (over: Partial<AgentRow> = {}) => seat({ activity: "working", ...over });
  expect(runningTarget([working()])).toEqual(watchLink("t-7"));
  expect(runningTarget([working(), seat({ id: "b", activity: "needs" })])).toEqual(
    watchLink("t-7"),
  );
  const live = { path: ["live"], query: {} };
  expect(runningTarget([])).toEqual(live);
  expect(runningTarget([working(), working({ id: "b" })])).toEqual(live);
  expect(runningTarget([working({ turn: null, live_call: null })])).toEqual(live);
});

// THE SHORT LIST — Home's Live now and the sidebar's Running group — FILTERS
// BEFORE IT CAPS: a seat whose turn has no id is never one of the rows, takes
// none of the four places, and is still counted.
test("the short list of running turns skips a seat with no turn id, then takes four", () => {
  const at = (id: string, turn: string, minute: number) =>
    seat({
      id,
      activity: "working",
      turn: {
        turn_id: turn,
        stage: "phase",
        started_at: `2026-09-21T10:0${minute}:00Z`,
      } as AgentRow["turn"],
      live_call: null,
    });
  const fresh = seat({ id: "fresh", activity: "working", turn: null, live_call: null });
  const roster = [at("e", "t-e", 5), fresh, at("a", "t-a", 1), at("c", "t-c", 3)];
  roster.push(at("b", "t-b", 2), at("d", "t-d", 4), seat({ id: "idle", activity: "idle" }));
  const { working, shown, more } = runningShortList(roster);
  expect(working.map((r) => r.id)).toEqual(["a", "b", "c", "d", "e", "fresh"]);
  expect(shown.map((r) => r.id)).toEqual(["a", "b", "c", "d"]);
  expect(more).toBe(2);
  // UNDER THE CAP the id-less seat is still not a row, and still counted.
  const few = runningShortList([fresh, at("a", "t-a", 1)]);
  expect(few.shown.map((r) => r.id)).toEqual(["a"]);
  expect(few.more).toBe(1);
});
