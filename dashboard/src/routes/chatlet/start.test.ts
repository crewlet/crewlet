/**
 * What a start gesture's answer does to the screen.
 *
 * THE HEADLINE INVARIANT IS THE UNKNOWN ONE: a create whose outcome nobody can
 * establish must not send the reader into a room that may not exist. Whatever
 * id an answer like that carries, following it is one line of plausible code
 * away, and the room screen would then tell somebody their brand-new room was
 * not found.
 *
 * The others are the same shape of mistake one step smaller: a durable record
 * this node has not applied drawn as success, and an address somebody else
 * holds drawn as a fault rather than as a name to change.
 */

import { describe, expect, test } from "vitest";

import { calloutFor, reportFor } from "./start.ts";
import type { ActErrorCode, ActResult } from "~/protocol/act.ts";

const receipt = { channel: "room-7", revision: 3 };

function answered(kind: "applied" | "pending", over: Record<string, unknown> = {}): ActResult {
  return {
    kind,
    tool: "create_channel",
    opId: "r-1",
    position: "CREWLET_CHAT_LOG@1:42",
    domain: "chat",
    receipt: { ...receipt, ...over },
    caveats: [],
  };
}

function lost(reason = "The answer was lost (status 0)."): ActResult {
  return { kind: "unknown", tool: "create_channel", opId: "r-1", reason };
}

function refused(code: ActErrorCode | "", sentence = "", hint = ""): ActResult {
  return {
    kind: "refused",
    tool: "create_channel",
    opId: "r-1",
    code,
    sentence,
    hint,
    grants: [],
    reason: "",
    retryable: false,
    remedy: null,
  };
}

describe("an outcome nobody can establish", () => {
  test("never carries a room to go to", () => {
    // THE ONE THAT MATTERS. A screen that navigated on an answer nobody can
    // vouch for would land a person on a room that may never have existed.
    expect(reportFor("channel", lost()).go).toBe("");
    expect(reportFor("direct", lost()).go).toBe("");
    expect(reportFor("join", lost()).go).toBe("");
  });

  test("is the only outcome that asks to be repeated, and says how", () => {
    const create = reportFor("channel", lost());
    expect(create.again).toBe(true);
    expect(create.settled).toBe(false);
    // A PRESS AGAIN IS A NEW REQUEST, and what makes it safe is the
    // arbitration itself: the name comes back taken if the first one landed.
    expect(create.note).toMatch(/same name/i);
    expect(reportFor("direct", lost()).note).toMatch(/same people/i);
    expect(reportFor("join", lost()).note).toMatch(/changes nothing/i);
  });

  test("says why nobody can tell, in the transport's own words", () => {
    expect(reportFor("channel", lost("The answer was lost (status 502).")).note).toContain(
      "status 502",
    );
  });
});

describe("a record this node has not applied", () => {
  test("does not send the reader to a room this node cannot read yet", () => {
    // The room IS durable — every other node will have it — but this node is
    // the one that would serve the read, and it has not applied the record.
    const report = reportFor("channel", answered("pending"));
    expect(report.go).toBe("");
    expect(report.note).toMatch(/not shown here yet/i);
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
  test("takes the reader into the room its receipt names", () => {
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
    // A leave takes somebody OUT of the room the receipt names; following the
    // id would put them back in front of it.
    expect(reportFor("leave", answered("applied")).go).toBe("");
    expect(reportFor("topic", answered("applied")).go).toBe("");
  });

  test("a receipt that names no room is not a room to go to", () => {
    expect(reportFor("channel", answered("applied", { channel: undefined })).go).toBe("");
  });
});

describe("a refusal", () => {
  test("a name somebody else holds is a name to change, not a failure", () => {
    const report = reportFor(
      "channel",
      refused(
        "exists",
        "Another room already has that name: launch. Open that room, or pick another name.",
      ),
    );
    expect(report.taken).toBe(true);
    expect(report.tone).toBe("warning");
    expect(report.tone).not.toBe("danger");
    expect(report.go).toBe("");
    // THE FIELD IS NAMED in this module's own words, beside the field a
    // person is already looking at.
    expect(report.note).toMatch(/already has that name/i);
  });

  test("every other refusal keeps the engine's sentence and is drawn as one", () => {
    const report = reportFor(
      "join",
      refused(
        "forbidden",
        "join_channel was refused: room-3 is a private room, and its membership is the only way into it.",
      ),
    );
    expect(report.tone).toBe("danger");
    expect(report.taken).toBe(false);
    expect(report.note).toContain("membership is the only way into it");
  });

  test("the engine's remedy travels with its sentence", () => {
    const report = reportFor(
      "channel",
      refused("unavailable", "This node could not make the change just now.", "Try again shortly."),
    );
    expect(report.note).toContain("Try again shortly.");
  });

  test("a room that is not there says it may only be one this reader cannot see", () => {
    // A private room's EXISTENCE is information, so "not found" and "not
    // yours" answer identically — and the transport's own "not there any more"
    // would say the first of the two as though it were known.
    expect(reportFor("direct", refused("not_found", "It is not there any more.")).note).toMatch(
      /no such room, or you can’t read it/i,
    );
  });
});
