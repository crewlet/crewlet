/**
 * What a start gesture's answer does to the screen.
 *
 * THE HEADLINE INVARIANT IS THE UNKNOWN ONE: a create whose outcome nobody can
 * establish must not send the reader into a room that may not exist. The
 * answer to a 504 still carries a channel id — the engine echoes what it tried
 * to write — so following it is one line of plausible code away, and the room
 * screen would then tell somebody their brand-new room was not found.
 *
 * The others are the same shape of mistake one step smaller: a durable record
 * this node has not applied drawn as success, and an address somebody else
 * holds drawn as a fault rather than as a name to change.
 */

import { describe, expect, test } from "vitest";

import { calloutFor, reportFor } from "./start.ts";
import type { WriteResult } from "./writes.ts";
import type { ChatChannel, ChatDirectAnswer } from "~/protocol/index.ts";

const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 42 };

const channel: ChatChannel = {
  v: 1,
  id: "room-7",
  kind: "public",
  name: "launch",
  created_at: "2026-01-01T00:00:00Z",
};

function answered(
  outcome: "applied" | "pending" | "unknown",
  over: Partial<ChatDirectAnswer> = {},
): WriteResult<ChatDirectAnswer> {
  return {
    outcome,
    answer: { outcome, op_id: "op-1", position, revision: 3, channel, ...over },
    detail: "",
    code: "",
  };
}

function refused(code: string, detail = ""): WriteResult<ChatDirectAnswer> {
  return { outcome: "refused", answer: null, detail, code };
}

describe("an outcome nobody can establish", () => {
  test("never carries a room to go to, whatever id the answer echoed", () => {
    // THE ONE THAT MATTERS. The engine answers 504 with the record it tried to
    // write, id and all — so a screen that reads `channel.id` off the answer
    // would navigate to a room that may never have landed.
    expect(reportFor("channel", answered("unknown")).go).toBe("");
    expect(reportFor("direct", answered("unknown")).go).toBe("");
    expect(reportFor("join", answered("unknown")).go).toBe("");
  });

  test("is the only outcome that asks to be repeated, and says how", () => {
    const create = reportFor("channel", answered("unknown"));
    expect(create.again).toBe(true);
    expect(create.settled).toBe(false);
    // THE OPERATION ID IS THE ENGINE'S ON A CREATE, so "retry under the same
    // id" is advice a person cannot follow. What makes the repeat safe is the
    // arbitration itself: the name comes back taken if the first one landed.
    expect(create.note).toMatch(/same name/i);
    expect(reportFor("direct", answered("unknown")).note).toMatch(/same people/i);
    expect(reportFor("join", answered("unknown")).note).toMatch(/membership is a set/i);
  });
});

describe("a record this node has not applied", () => {
  test("does not send the reader to a room this node cannot read yet", () => {
    // The room IS durable — every other node will have it — but this node is
    // the one that would serve the read, and it has not applied the record.
    const report = reportFor("channel", answered("pending"));
    expect(report.go).toBe("");
    expect(report.note).toMatch(/has not applied it yet/i);
  });

  test("is drawn as information and never as success", () => {
    expect(reportFor("channel", answered("pending")).tone).toBe("pending");
    expect(calloutFor("pending")).toBe("info");
    // A green tick beside "not applied yet" is the browser half of the lie
    // the durable-versus-applied split exists to prevent.
    expect(calloutFor("pending")).not.toBe("success");
  });
});

describe("a record that applied", () => {
  test("takes the reader into the room it made", () => {
    expect(reportFor("channel", answered("applied")).go).toBe("room-7");
    expect(reportFor("join", answered("applied")).go).toBe("room-7");
  });

  test("opening a conversation that already exists goes to it rather than erroring", () => {
    // A direct conversation's id is derived from its participants, so the
    // loser of the race is handed the room the winner made. `created: false`
    // is the ordinary answer — not a collision to report.
    const report = reportFor("direct", answered("applied", { created: false }));
    expect(report.go).toBe("room-7");
    expect(report.tone).toBe("success");
    expect(report.settled).toBe(true);
  });

  test("a gesture that makes no room navigates nowhere", () => {
    // A leave takes somebody OUT of the room the answer names; following the
    // id would put them back in front of it.
    expect(reportFor("leave", answered("applied")).go).toBe("");
    expect(reportFor("topic", answered("applied")).go).toBe("");
  });
});

describe("a refusal", () => {
  test("a name somebody else holds is a name to change, not a failure", () => {
    const report = reportFor(
      "channel",
      refused("name_taken", "chat: invalid: name: #launch is already a room"),
    );
    expect(report.taken).toBe(true);
    expect(report.tone).toBe("warning");
    expect(report.tone).not.toBe("danger");
    expect(report.go).toBe("");
    // THE ENGINE'S OWN SENTENCE SURVIVES: its refusals name the field and the
    // rule, and that is the whole of their value to a person.
    expect(report.note).toContain("#launch is already a room");
  });

  test("every other refusal keeps the engine's sentence and is drawn as one", () => {
    const report = reportFor(
      "join",
      refused(
        "forbidden",
        "chat: forbidden: room-3 is a private room, and its membership is the only way into it",
      ),
    );
    expect(report.tone).toBe("danger");
    expect(report.taken).toBe(false);
    expect(report.note).toContain("membership is the only way into it");
  });

  test("a refusal with nothing to say still says something", () => {
    // The unclassified failures are exactly the ones with no text: their
    // reason carries a database path and reaches the node's log instead.
    expect(reportFor("channel", refused("write_failed")).note).not.toBe("");
    expect(reportFor("direct", refused("not_found")).note).toMatch(/no such room/i);
  });
});
