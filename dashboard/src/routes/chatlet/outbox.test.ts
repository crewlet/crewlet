/**
 * A MESSAGE ANSWERED `pending` IS ASKED AGAIN UNTIL IT IS DECIDED.
 *
 * A post is decided where it is applied (ADR-0046), and `pending` is a node
 * that had not applied it yet: the room may have been archived just ahead of
 * it. So the outbox asks again under the row's own operation key — the ledger
 * answers that key with the verdict and writes nothing twice — and only a
 * DECISION moves the row: landed, or refused. These hold the arithmetic and
 * the transitions; `Room.test.tsx` holds the whole path through a room.
 */

import { describe, expect, test } from "vitest";

import { PENDING_REASK_MS } from "~/contract/chat.ts";
import { RETRY_AFTER_MAX_MS } from "~/contract/retry.ts";
import type { ActResult } from "~/protocol/act.ts";

import { applyReask, reaskDelay, unsuperseded, type Outgoing } from "./outbox.ts";

const NOW = 1_800_000_000_000;

const waiting: Outgoing = {
  opId: "01890000-0000-7000-8000-000000000001",
  channelID: "room-1",
  threadRoot: "",
  body: "anyone there?",
  links: [],
  state: "accepted",
  messageID: "m-said",
  detail: "",
  retryable: false,
  startedAt: NOW - 5_000,
  answer: "pending",
  reasks: 0,
  reaskAt: NOW,
};

const base = { tool: "post_message" as const, opId: waiting.opId };

const answered = (kind: "applied" | "pending"): ActResult => ({
  ...base,
  kind,
  position: "CREWLET_CHAT_LOG@1:41",
  domain: "chat",
  receipt: { message_id: "m-said" },
  caveats: [],
});

const refusal = (code: "conflict" | "unavailable", retryable: boolean): ActResult => ({
  ...base,
  kind: "refused",
  code,
  sentence: "The room is archived.",
  detail: "",
  hint: "",
  hostRefusal: "",
  grants: [],
  reason: "",
  retryable,
  remedy: null,
});

describe("the wait before asking again", () => {
  test("starts at the applier's own wait, doubles, and stops at the dashboard's ceiling", () => {
    expect(reaskDelay(0)).toBe(PENDING_REASK_MS);
    expect(reaskDelay(1)).toBe(PENDING_REASK_MS * 2);
    expect(reaskDelay(2)).toBe(PENDING_REASK_MS * 4);
    expect(reaskDelay(50)).toBe(RETRY_AFTER_MAX_MS);
  });
});

describe("what asking again does to a pending row", () => {
  test("a refusal the applier decided is drawn, and nothing is asked again", () => {
    const row = applyReask(waiting, refusal("conflict", false), NOW);
    expect(row.state).toBe("refused");
    expect(row.detail).toBe("The room is archived.");
    expect(row.messageID).toBe("");
    expect(row.answer).toBe("");
    expect(row.reaskAt).toBe(0);
  });

  test("a post that landed stays accepted and is asked nothing more", () => {
    const row = applyReask(waiting, answered("applied"), NOW);
    expect(row.state).toBe("accepted");
    expect(row.answer).toBe("applied");
    expect(row.messageID).toBe("m-said");
    expect(row.reaskAt).toBe(0);
  });

  test("anything that is no decision leaves it accepted and asks later", () => {
    // STILL PENDING, A NODE THAT REFUSED FOR NOW, AN ANSWER NOBODY CAN VOUCH
    // FOR and A BROWSER THAT COULD NOT SEND: none of them says anything about
    // the message, which the first answer said is durable.
    const unknown: ActResult = { ...base, kind: "unknown", reason: "the answer was lost" };
    for (const result of [answered("pending"), refusal("unavailable", true), unknown, null]) {
      const row = applyReask(waiting, result, NOW);
      expect(row.state).toBe("accepted");
      expect(row.answer).toBe("pending");
      expect(row.reasks).toBe(1);
      expect(row.reaskAt).toBe(NOW + reaskDelay(1));
    }
  });
});

// A ROW IS NEVER DRAWN BESIDE THE MESSAGE IT STANDS FOR. Matched by its id once
// answered, and while still sending by the reader's own words arriving in the
// same place after the press, which the live frame often delivers before the
// answer. Mutation: drop the sending match and "the frame beat the answer"
// goes red; drop the floor and "an older message" goes red.
describe("a transcript draws a row until it carries the message", () => {
  const said = (id: string, seq: number, body: string, over: Record<string, unknown> = {}) =>
    ({
      message: { v: 1, id, channel_id: "room-1", author: "ada", body, ...over },
      channel_seq: seq,
    }) as never;
  const sending: Outgoing = { ...waiting, state: "sending", messageID: "", answer: "", reaskAt: 0 };
  const floor = new Map([[sending.opId, 10]]);

  test("an answered row leaves once its id is held", () => {
    expect(unsuperseded([waiting], [said("m-said", 11, "anyone there?")], "ada", floor)).toEqual(
      [],
    );
    expect(unsuperseded([waiting], [said("m-other", 11, "x")], "ada", floor)).toEqual([waiting]);
  });

  test("the frame beat the answer: the reader's words, after the press, stand for the row", () => {
    expect(unsuperseded([sending], [said("m-11", 11, "anyone there? ")], "ada", floor)).toEqual([]);
  });

  test("an older message with the same words is not this press", () => {
    expect(unsuperseded([sending], [said("m-9", 9, "anyone there?")], "ada", floor)).toEqual([
      sending,
    ]);
  });

  test("somebody else's words, another thread's, or a message another row holds are not it", () => {
    const rows = [sending, { ...waiting, messageID: "m-12" }];
    const held = [
      said("m-11", 11, "anyone there?", { author: "bo" }),
      said("m-13", 13, "anyone there?", { thread_root: "m-1" }),
      said("m-12", 12, "anyone there?"),
    ];
    expect(unsuperseded(rows, held, "ada", floor)).toEqual([sending]);
  });

  test("two presses of the same words are matched one message each", () => {
    const second = { ...sending, opId: "01890000-0000-7000-8000-000000000002" };
    const floors = new Map([
      [sending.opId, 10],
      [second.opId, 10],
    ]);
    expect(
      unsuperseded([sending, second], [said("m-11", 11, "anyone there?")], "ada", floors),
    ).toEqual([second]);
  });
});
