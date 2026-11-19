/**
 * An item's flow as words (ADR-0162): the engine's state, cause and instants
 * drawn as sent, and Workload's counts read from its buckets.
 */

import { describe, expect, test } from "vitest";
import type { WorkFlow, WorkFlowEntry } from "~/protocol/index.ts";
import {
  badgedFlow,
  flowBadgeText,
  flowEntryWords,
  flowLabel,
  flowSentence,
  flowTone,
  openWords,
  workloadWords,
} from "./flow.ts";

const NOW = Date.parse("2031-04-16T12:00:00Z");

const flow = (over: Partial<WorkFlow>): WorkFlow => ({
  state: "moving",
  cause: "",
  since: null,
  until: null,
  reason: "",
  ...over,
});

describe("a flow badge", () => {
  test("the ordinary moving item and an item not in progress draw none", () => {
    expect(badgedFlow(flow({ state: "moving" }))).toBeNull();
    expect(badgedFlow(null)).toBeNull();
    expect(badgedFlow(flow({ state: "stuck" }))?.state).toBe("stuck");
  });

  test("says when the state ends where the engine knows it, else how long it has held", () => {
    const held = flow({ state: "held", cause: "limit", until: "2031-04-16T13:00:00Z" });
    expect(flowBadgeText(held, NOW)).toMatch(/^Held · until \S/);
    const stuck = flow({ state: "stuck", since: "2031-04-16T03:00:00Z" });
    expect(flowBadgeText(stuck, NOW)).toBe("Stuck · 9h");
    expect(flowBadgeText(flow({ state: "blocked" }), NOW)).toBe("Blocked");
  });

  test("a start that passed reads apart from a due date", () => {
    expect(flowLabel("due")).toBe("Start passed");
    expect(flowTone("due")).toBe("warning");
  });

  test("a state this build does not know reads as its own word, in no tone", () => {
    expect(flowLabel("parked_forever")).toBe("Parked forever");
    expect(flowTone("parked_forever")).toBe("neutral");
  });
});

describe("a flow sentence", () => {
  test("names the cause the engine sent", () => {
    const held = flow({ state: "held", cause: "limit", until: "2031-04-16T13:00:00Z" });
    expect(flowSentence(held, NOW)).toMatch(
      /^Held until .+: its assignee is out of its model limit$/,
    );
    expect(flowSentence(flow({ state: "held", cause: "departed" }), NOW)).toBe(
      "Held: its assignee has left the org chart",
    );
  });

  test("a wait for a start ends with the wait's own words", () => {
    const waiting = flow({
      state: "waiting",
      cause: "start",
      until: "2031-04-20T00:00:00Z",
      reason: "The release's health check",
    });
    expect(flowSentence(waiting, NOW)).toMatch(/^Waiting until .+: The release's health check$/);
  });

  test("a state with no cause says why it holds", () => {
    expect(flowSentence(flow({ state: "due", since: "2031-04-16T09:00:00Z" }), NOW)).toMatch(
      /^Start passed since .+: its start passed and nothing of its assignee's has run on it since$/,
    );
  });
});

describe("Workload's counts", () => {
  const counts = { working: 1, blocked: 1, waiting: 2, held: 1, due: 0, stuck: 1, moving: 1 };

  test("say the load, then each state that keeps work from moving, zeros left out", () => {
    expect(workloadWords({ load: 3, flow: counts })).toEqual([
      "3 to move now",
      "2 waiting",
      "1 held",
      "1 stuck",
    ]);
    expect(workloadWords({ load: 0, flow: { ...counts, waiting: 0, held: 0, stuck: 0 } })).toEqual(
      [],
    );
  });

  test("a picker's line counts open work and the load apart", () => {
    expect(openWords({ open: 4, load: 2 })).toBe("4 open items · 2 to move now");
    // ALL OF IT WAITING: open work, and no load.
    expect(openWords({ open: 3, load: 0 })).toBe("3 open items");
    expect(openWords(undefined)).toBe("nothing open");
  });
});

describe("a flow history line", () => {
  const entry = (over: Partial<WorkFlowEntry>): WorkFlowEntry => ({
    id: "f-1",
    task: "t-1",
    state: "stuck",
    cause: "",
    since: "2031-04-15T09:00:00Z",
    until: null,
    seat: "swe",
    notice: "",
    told: "",
    backfilled: false,
    at: "2031-04-16T09:00:00Z",
    log_seq: 40,
    ...over,
  });

  test("a recorded change says the state and why it holds", () => {
    const { what, why } = flowEntryWords(entry({}), NOW);
    expect(what).toMatch(/^recorded it as stuck since \S/);
    expect(why).toBe("nothing changed on it for its project's window");
  });

  test("a notice names whom it told, and in which role", () => {
    const names = (handle: string) => (handle === "maya" ? "Maya" : handle);
    const told = flowEntryWords(entry({ notice: "lead", told: "maya" }), NOW, names);
    expect(told.what).toMatch(/^told Maya, as its lead, it reads stuck since \S/);
    expect(told.why).toBe("");
  });
});
