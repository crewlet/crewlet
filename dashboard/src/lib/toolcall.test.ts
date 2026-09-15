/**
 * The copyable call this read-only product offers in place of an edit button.
 *
 * Every write in this engine is attributed to somebody, so a button in a
 * browser would write as "the dashboard", which is nobody. What a screen can
 * honestly offer is the call an assistant would make, with this object's ids
 * already in it — and the failure mode is that the call is subtly wrong and
 * nobody notices, because nothing here sends it.
 *
 * The notice case in particular had no caller at all: `callsFor` knew how to
 * mark an inbox entry and the Inbox screen rendered a sentence about
 * `mark_inbox` instead of the call itself.
 */

import { describe, expect, test } from "vitest";

import { callText, callsFor } from "./toolcall.ts";

describe("what a notice offers", () => {
  const calls = callsFor({ kind: "notice", id: "rec-7", version: 412 });

  test("both marks, read first", () => {
    // Read is what a reader most often means; the snooze is the deliberate
    // one and sits under it.
    expect(calls.map((c) => c.label)).toEqual(["Mark it read", "Put it off until a time you name"]);
    expect(calls.every((c) => c.tool === "mark_inbox")).toBe(true);
  });

  test("the record and its position travel together", () => {
    // A POSITION FROM A RECREATED STREAM COMPARES AS CURRENT, which is why
    // `mark_inbox` takes both and why a call carrying only the id would be
    // accepted and wrong.
    for (const call of calls) {
      const entries = (call.args.read ?? call.args.snoozed) as { position: number }[];
      expect(entries[0]).toMatchObject({ record_id: "rec-7", position: 412 });
    }
  });

  test("a snooze names when it comes back", () => {
    // `until` is what makes it a snooze rather than a second spelling of
    // unread. It is a placeholder rather than a date this screen picked:
    // when to come back is the person's decision.
    const snooze = calls[1]?.args.snoozed as { until?: string }[];
    expect(snooze[0]?.until).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  });

  test("it copies as the assistant is asked for it", () => {
    expect(callText(calls[0]!)).toBe('mark_inbox {"read":[{"record_id":"rec-7","position":412}]}');
  });
});

test("an object with no id offers nothing", () => {
  // A screen that has not loaded yet would otherwise offer a call naming the
  // empty string, which an assistant would send.
  expect(callsFor({ kind: "item", id: "" })).toEqual([]);
  expect(callsFor({ kind: "notice", id: "" })).toEqual([]);
});

test("a placeholder is prose, never an empty string", () => {
  // `"body": ""` copied unedited lands an empty comment; `"body": "…"` is a
  // prompt to write something.
  const comment = callsFor({ kind: "item", id: "ENG-42" }).find(
    (c) => c.tool === "comment_on_work_item",
  );
  expect(comment?.args.body).toBe("…");
});

test("anything destructive is last and says so", () => {
  const calls = callsFor({ kind: "item", id: "ENG-42" });
  const destructive = calls.filter((c) => c.destructive);
  expect(destructive.length).toBeGreaterThan(0);
  // A block that opened on "remove this" would be a delete button wearing a
  // disclosure.
  expect(calls[calls.length - 1]?.destructive).toBe(true);
});
