import { expect, test } from "vitest";
import { appliesTo, controlHeld, controlOutcome } from "./seatControls.tsx";
import type { SeatLive } from "~/lib/seatLive.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";

function s(handle: string, over: Partial<SeatLive> = {}): SeatLive {
  return {
    seat: { handle, name: handle.toUpperCase(), kind: "agent" },
    state: "idle",
    paused: false,
    controllable: true,
    ...over,
  } as unknown as SeatLive;
}

// A CONTROL APPLIES TO THE SEATS IT WOULD CHANGE, and only those the reader
// may control: a paused seat is not paused again, an idle one has no turn to
// stop, and a paused seat whose turn still runs CAN be stopped, which a
// second pause does.
//
// Mutation: let a stop skip a paused seat, or forget the reader's control,
// and this fails.
test("each control applies to the seats it would change, for a reader who may", () => {
  const seats = [
    s("idle"),
    s("working", { state: "working" }),
    s("paused", { state: "paused", paused: true }),
    s("finishing", { state: "working", paused: true }),
    s("theirs", { state: "working", controllable: false }),
  ];
  const handles = (kind: "pause" | "stop" | "resume") =>
    appliesTo(kind, seats).map((x) => x.seat.handle);
  expect(handles("pause")).toEqual(["idle", "working"]);
  expect(handles("stop")).toEqual(["working", "finishing"]);
  expect(handles("resume")).toEqual(["paused", "finishing"]);
});

// MANY SEATS ARE ONE ANSWER: every seat changed, or which were not and why.
// It stopped at the first refusal once, and the reader could not tell which
// of the rest had been sent.
test("a control of many seats says once what came of every one", () => {
  const a = s("a");
  const b = s("b");
  const c = s("c");
  expect(controlOutcome("pause", false, [a, b, c], [])).toEqual({
    variant: "success",
    title: "Paused 3 agents",
  });
  const partial = controlOutcome("pause", true, [a, b], [{ s: c, why: "Not allowed." }]);
  expect(partial.variant).toBe("warning");
  expect(partial.title).toBe("Paused 2 of 3 agents and stopped their turns");
  expect(partial.message).toBe("C: Not allowed.");
  expect(controlOutcome("resume", false, [], [{ s: a, why: "Gone." }]).title).toBe(
    "Nothing was resumed",
  );
});

test("one seat on a turn is told it pauses once its turn finishes", () => {
  const working = s("pm", { state: "working" });
  expect(controlOutcome("pause", false, [working], []).title).toBe(
    "PM pauses once its turn finishes",
  );
  expect(controlOutcome("pause", true, [working], []).title).toBe("Paused PM and stopped its turn");
  expect(controlOutcome("resume", false, [s("qa")], []).title).toBe("Resumed QA");
});

test("a held control says why, in the rule's words for a reader the engine does not admit", () => {
  const can = { can: true, as: "jane", acts: ["pause_seat"] } as WriteAccess;
  const notServed = { can: false, block: "not_served", reason: "x" } as unknown as WriteAccess;
  expect(controlHeld(can, s("a"))).toBeUndefined();
  expect(controlHeld(can, s("a", { controllable: false }))).toBe(
    "Only an admin or a lead above this seat can control it.",
  );
  expect(controlHeld(notServed, s("a"))).toBe("Pausing a seat needs Admin access, or leading it.");
});
