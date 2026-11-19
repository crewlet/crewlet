/**
 * An item's flow as words (ADR-0162): the engine's state, cause and instants
 * drawn as sent, on the company's clock, and Workload's counts read from its
 * buckets.
 */

import { afterEach, beforeEach, describe, expect, test } from "vitest";
import type { WorkFlow, WorkFlowEntry } from "~/protocol/index.ts";
import {
  badgedFlow,
  flowBadgeText,
  flowEntryWords,
  flowInstant,
  flowLabel,
  flowSentence,
  flowTone,
  openWords,
  workloadWords,
  type FlowWords,
} from "./flow.ts";
import { pinZoneForTest } from "./prefs.ts";

const NOW = Date.parse("2031-04-16T12:00:00Z");
const UTC: FlowWords = { zone: "" };
const BERLIN: FlowWords = { zone: "Europe/Berlin" };

beforeEach(() => pinZoneForTest("UTC"));
afterEach(() => pinZoneForTest(""));

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
    expect(flowBadgeText(held, NOW, "")).toBe("Held · until 13:00");
    const stuck = flow({ state: "stuck", since: "2031-04-16T03:00:00Z" });
    expect(flowBadgeText(stuck, NOW, "")).toBe("Stuck · 9h");
    expect(flowBadgeText(flow({ state: "blocked" }), NOW, "")).toBe("Blocked");
  });

  test("a start that passed reads apart from a due date", () => {
    expect(flowLabel("due")).toBe("Start passed");
    expect(flowTone("due")).toBe("warning");
  });

  // AN ITEM NOBODY HOLDS SAYS SO, rather than a bare "Held" that reads as
  // somebody holding it, and says since when.
  test("a cause that is the whole story is the badge's word", () => {
    const nobody = flow({ state: "held", cause: "nobody", since: "2031-04-16T10:00:00Z" });
    expect(flowBadgeText(nobody, NOW, "")).toBe("No assignee · 2h");
    expect(flowBadgeText(flow({ state: "held", cause: "departed" }), NOW, "")).toBe(
      "Assignee left",
    );
    expect(flowBadgeText(flow({ state: "held", cause: "paused" }), NOW, "")).toBe("Held");
  });

  test("a state this build does not know reads as its own word, in no tone", () => {
    expect(flowLabel("parked_forever")).toBe("Parked forever");
    expect(flowTone("parked_forever")).toBe("neutral");
  });
});

// THE FLOW'S INSTANTS ARE ON THE COMPANY'S CLOCK, as Worklet's dates are. A
// wait until a start is the company's midnight: read on a UTC reader's clock
// in a company on Berlin's, it was the evening before, beside a Start field
// naming the day itself.
describe("a flow's instants", () => {
  const midnight = "2031-04-21T22:00:00Z"; // Apr 22, 00:00 in Berlin

  test("a wait until a start names the company's day, never the evening before", () => {
    const waiting = flow({ state: "waiting", cause: "start", until: midnight });
    expect(flowBadgeText(waiting, NOW, "Europe/Berlin")).toBe("Waiting · until Apr 22");
    expect(flowSentence(waiting, NOW, BERLIN)).toBe("Waiting until Apr 22: it waits for its start");
  });

  test("a clock time is the company's, and says whose where the reader's clock is another", () => {
    // 23:00 in Berlin, a seat's limit lifting the same evening.
    expect(flowInstant("2031-04-16T21:00:00Z", NOW, "Europe/Berlin")).toMatch(/^23:00 \S/);
    expect(flowInstant("2031-04-16T21:00:00Z", NOW, "")).toBe("21:00");
    expect(flowInstant("2031-04-18T09:30:00Z", NOW, "")).toBe("Apr 18, 09:30");
  });
});

describe("a flow sentence", () => {
  test("names the cause the engine sent", () => {
    const held = flow({ state: "held", cause: "limit", until: "2031-04-16T13:00:00Z" });
    expect(flowSentence(held, NOW, UTC)).toBe(
      "Held until 13:00: its assignee is out of its model limit",
    );
    expect(flowSentence(flow({ state: "held", cause: "departed" }), NOW, UTC)).toBe(
      "Assignee left: its assignee has left the org chart",
    );
  });

  test("a wait for a start ends with the wait's own words", () => {
    const waiting = flow({
      state: "waiting",
      cause: "start",
      until: "2031-04-20T00:00:00Z",
      reason: "The release's health check",
    });
    expect(flowSentence(waiting, NOW, UTC)).toBe(
      "Waiting until Apr 20: The release's health check",
    );
  });

  // A PERSON PICKS AN ITEM UP; an agent takes a turn on it. One clause for
  // both spoke of runs on a person's item.
  test("a start that passed is said in its assignee's terms", () => {
    const due = flow({ state: "due", since: "2031-04-16T09:00:00Z" });
    const chart = {
      seatName: (h: string) => (h === "riley" ? "Riley Ops" : "Agent SWE"),
      seatKind: (h: string) => (h === "riley" ? ("human" as const) : ("agent" as const)),
    };
    expect(flowSentence(due, NOW, { ...UTC, ...chart, assignee: "riley" })).toBe(
      "Start passed since 09:00: its start passed and Riley Ops has not picked it up since",
    );
    expect(flowSentence(due, NOW, { ...UTC, ...chart, assignee: "agent-swe" })).toBe(
      "Start passed since 09:00: its start passed and Agent SWE has not taken a turn on it since",
    );
  });

  // A STUCK ITEM SAYS ITS WINDOW where the screen holds its project.
  test("a stuck item names its project's window where it is known", () => {
    const stuck = flow({ state: "stuck", since: "2031-04-16T11:00:00Z" });
    expect(flowSentence(stuck, NOW, { ...UTC, stuckHours: 1 })).toBe(
      "Stuck since 11:00: nothing changed on it for 1 hour, its project's stuck window",
    );
    expect(flowSentence(stuck, NOW, UTC)).toBe(
      "Stuck since 11:00: nothing changed on it for its project's stuck window",
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
    const { what, why } = flowEntryWords(entry({}), NOW, { ...UTC, stuckHours: 8 });
    expect(what).toBe("recorded it as stuck since Apr 15, 09:00");
    expect(why).toBe("nothing changed on it for 8 hours, its project's stuck window");
  });

  test("a wait it recorded names the company's day", () => {
    const { what } = flowEntryWords(
      entry({ state: "waiting", cause: "start", since: null, until: "2031-04-21T22:00:00Z" }),
      NOW,
      BERLIN,
    );
    expect(what).toBe("recorded it as waiting until Apr 22");
  });

  test("a notice names whom it told, and in which role", () => {
    const names = (handle: string) => (handle === "maya" ? "Maya" : handle);
    const told = flowEntryWords(entry({ notice: "lead", told: "maya" }), NOW, {
      ...UTC,
      seatName: names,
    });
    expect(told.what).toBe("told Maya, as its lead, it reads stuck since Apr 15, 09:00");
    expect(told.why).toBe("");
  });
});
