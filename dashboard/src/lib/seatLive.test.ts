import { describe, expect, test } from "vitest";
import {
  countsOf,
  doingLine,
  matches,
  pauseNoteOf,
  placedRow,
  problemsOf,
  seatLiveOf,
  statusFilterOf,
  summaryOf,
  withWord,
  type SeatLive,
} from "./seatLive.ts";
import type { NameOf, Seat } from "./seats.ts";
import type { AgentRow, ExecutorEntry } from "~/protocol/index.ts";

const NOW = Date.parse("2026-06-14T12:00:00Z");
const nameOf: NameOf = (key) => key;

function row(over: Record<string, unknown>): AgentRow {
  return { id: "qa", role: "QA", handle: "qa", activity: "idle", ...over } as unknown as AgentRow;
}

function seat(over: Partial<Seat> = {}): Seat {
  return {
    key: "qa",
    handle: "qa",
    name: "QA Engineer",
    kind: "agent",
    availability: "",
    ...over,
  } as unknown as Seat;
}

function live(r: AgentRow | undefined, over: Partial<Seat> = {}, ask: ExecutorEntry | null = null) {
  return seatLiveOf(seat(over), r, { now: NOW, nameOf, controllable: true, ask });
}

// A PAUSE THE SEAT HAS NOT REACHED YET IS SAID: the engine lets a running turn
// finish, so a paused seat reads `working` until it does, and `needs` while its
// executor asks. Read as the state alone, it was "Working" beside Resume.
//
// Mutation: drop either arm, or read a pause on an idle seat as one still
// pending, and this fails.
test("a pause a running turn has not reached is a note beside the state", () => {
  expect(pauseNoteOf(row({ activity: "working", paused: { by: "ana" } }))).toBe("pausing");
  expect(pauseNoteOf(row({ activity: "needs", paused: { by: "ana" } }))).toBe("held");
  expect(pauseNoteOf(row({ activity: "stopped", stopped_reason: "paused", paused: {} }))).toBe(
    null,
  );
  expect(pauseNoteOf(row({ activity: "working" }))).toBe(null);
  const s = live(row({ activity: "working", paused: { by: "ana" } }));
  expect(s.word).toBe("Working");
  expect(s.paused).toBe(true);
  expect(s.pauseNote).toBe("pausing");
});

describe("a seat's problems are what a person would act on, worst first", () => {
  test("a failed last turn, a model limit, a spent budget and no node", () => {
    const problems = problemsOf(
      row({
        activity: "stopped",
        stopped_reason: "budget",
        last_turn: { outcome: "failed" },
        last_error: { kind: "provider_auth", detail: "" },
        provider_limit: { parked: true, resets_at: "2026-06-14T13:35:00Z" },
      }),
      NOW,
    );
    expect(problems.map((p) => p.key)).toEqual(["failed", "limit", "budget"]);
    expect(problems[1]!.sentence).toContain("in 1h 35m");
  });

  test("a model on its fallback is information, and a near ceiling a warning", () => {
    const problems = problemsOf(
      row({
        provider_limit: { parked: false },
        budget: { windows: [{ state: "near", used: 93, limit: 100, period: "day" }] },
      }),
      NOW,
    );
    expect(problems.map((p) => [p.key, p.tone])).toEqual([
      ["fallback", "info"],
      ["near", "warning"],
    ]);
    expect(problems[1]!.tag).toBe("Budget 93%");
  });

  test("a person's seat has none, and no live row means none", () => {
    expect(live(undefined, { kind: "human" }).problems).toEqual([]);
    expect(problemsOf(undefined, NOW)).toEqual([]);
  });
});

// THE CHIPS COUNT AGENTS, and a pause and a fault's stop never share a count;
// a seat with no row yet is counted on its own, never as idle.
test("the counts are agents by state, a pause apart from a stop", () => {
  const seats = [
    live(row({ activity: "working" })),
    live(row({ activity: "needs" })),
    live(row({ activity: "stopped", stopped_reason: "paused" })),
    live(row({ activity: "stopped", stopped_reason: "unplaced" })),
    live(row({ activity: "idle" })),
    live(undefined),
    live(undefined, { kind: "human" }),
  ];
  expect(countsOf(seats)).toEqual({
    all: 6,
    working: 1,
    needs: 1,
    paused: 1,
    stopped: 1,
    idle: 1,
    offline: 1,
    problems: 1,
  });
});

test("a chip keeps the agents in its state, and only All keeps a person", () => {
  const paused = live(row({ activity: "stopped", stopped_reason: "paused" }));
  const person = live(undefined, { kind: "human" });
  expect(matches(paused, "stopped")).toBe(true);
  expect(matches(paused, "idle")).toBe(false);
  expect(matches(person, "all")).toBe(true);
  expect(matches(person, "idle")).toBe(false);
  expect(statusFilterOf("needs")).toBe("needs");
  expect(statusFilterOf("nonsense")).toBe("all");
});

// WHAT NEEDS A PERSON LEADS, and each count keeps its words together, so a
// cut line keeps what matters and a line never starts with a separator.
test("a team's summary leads with what needs a person", () => {
  const seats: SeatLive[] = [
    live(row({ activity: "idle" })),
    live(row({ activity: "working" })),
    live(row({ activity: "needs" })),
  ];
  expect(summaryOf(seats, "No agents").replaceAll(" ", " ")).toBe(
    "1 needs you · 1 working · 1 idle",
  );
  expect(summaryOf([], "No agents")).toBe("No agents");
  expect(summaryOf(seats, "x")).not.toMatch(/ ·/);
});

test("what a seat is doing names a held call, a question, or a pause's reason", () => {
  const held = { held_call: { tool: "merge_pull_request", server: "github" } } as ExecutorEntry;
  expect(doingLine(live(row({ activity: "needs" }), {}, held))).toBe(
    "Asks to call merge_pull_request on github",
  );
  const asked = { question: "Which branch?" } as ExecutorEntry;
  expect(doingLine(live(row({ activity: "needs" }), {}, asked))).toBe("Asks: Which branch?");
  const paused = live(
    row({ activity: "stopped", stopped_reason: "paused", paused: { by: "ana", reason: "Audit" } }),
  );
  expect(doingLine(paused)).toContain("“Audit”");
});

// A STATE'S WORD IS SAID ONCE beside a line about it: the engine's line for a
// turn no phase names already begins with "Working", and the panel and the
// card's title read "Working · Working on ENG-7". Only a whole word counts, so
// a line that merely starts with the same letters keeps the word. Mutation:
// always prefix the word, or compare without the word boundary.
test("a state's word is joined to its line once", () => {
  expect(withWord("Working", "Executing ENG-7")).toBe("Working · Executing ENG-7");
  expect(withWord("Working", "Working on ENG-7")).toBe("Working on ENG-7");
  expect(withWord("Working", "Working")).toBe("Working");
  expect(withWord("Idle", "Idler queue")).toBe("Idle · Idler queue");
  expect(withWord("Working", "")).toBe("Working");
});

// A SEAT THIS TAB JUST ADDED IS NOT STOPPED: the engine calls it unplaced until a
// node has applied the revision, and read as its standing it was Stopped with a
// red No node, one more in the problem counts, for a reader who had only added
// it. Only that word, and only that seat. Mutation: read the row whatever it says.
test("a seat this tab just added has no state until it is placed", () => {
  const unplaced = row({ activity: "stopped", stopped_reason: "unplaced" });
  const fresh = new Set(["dev"]);
  expect(placedRow(unplaced, "dev", fresh)).toBeUndefined();
  expect(placedRow(unplaced, "ops", fresh)).toBe(unplaced);
  const idle = row({ activity: "idle" });
  expect(placedRow(idle, "dev", fresh)).toBe(idle);
  const seen = live(placedRow(unplaced, "dev", fresh));
  expect(seen.word).toBe("No state yet");
  expect(seen.problems).toEqual([]);
  expect(live(unplaced).problems.map((p) => p.tag)).toEqual(["No node"]);
});
