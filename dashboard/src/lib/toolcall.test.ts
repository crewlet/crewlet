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
  const calls = callsFor({ kind: "notice", id: "rec-7" });

  test("both marks, read first", () => {
    // Read is what a reader most often means; the snooze is the deliberate
    // one and sits under it.
    expect(calls.map((c) => c.label)).toEqual(["Mark it read", "Put it off until a time you name"]);
    expect(calls.every((c) => c.tool === "mark_inbox")).toBe(true);
  });

  test("a mark names the notice and nothing else", () => {
    // A GESTURE, NOT THE LISTS: the call this block used to offer sent a
    // `read` list, and `mark_inbox` replaced every list it was given — so
    // copying it erased every other mark. The engine now takes the record id
    // alone and reads the notice's position itself.
    expect(calls[0]?.args).toEqual({ read: ["rec-7"] });
    expect(Object.keys(calls[1]?.args ?? {})).toEqual(["snooze"]);
  });

  test("a snooze names when it comes back", () => {
    // `until` is what makes it a snooze. It is a placeholder rather than a
    // date this screen picked: when to come back is the person's decision.
    const snooze = calls[1]?.args.snooze as { record_id?: string; until?: string }[];
    expect(snooze[0]?.record_id).toBe("rec-7");
    expect(snooze[0]?.until).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  });

  test("it copies as the assistant is asked for it", () => {
    expect(callText(calls[0]!)).toBe('mark_inbox {"read":["rec-7"]}');
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

// A TASK IN THE TRASH IS OFFERED THE WAY BACK, NOT THE WAY OUT.
//
// The item arm offered `remove_work_item` unconditionally, so a task already
// in the trash was offered the one call in this table that cannot do anything
// to it — and the call that CAN, `restore_work_item`, appeared nowhere at all
// despite being a real operator tool. The screen's whole promise is that the
// block is what to send.
test("a removed item offers the restore instead of the removal", () => {
  const calls = callsFor({ kind: "item", id: "ENG-42", removed: true });
  const names = calls.map((c) => c.tool);
  expect(names).toContain("restore_work_item");
  expect(names).not.toContain("remove_work_item");
  // AND NOTHING HERE IS DESTRUCTIVE. Restoring is the ordinary call for this
  // state; it is the live task's removal that carries the warning.
  expect(calls.some((c) => c.destructive)).toBe(false);
});

// AND A LIVE ONE IS UNCHANGED, which is the half that proves the branch is a
// branch rather than a replacement.
test("a live item still offers the removal, last and marked", () => {
  const calls = callsFor({ kind: "item", id: "ENG-42" });
  const names = calls.map((c) => c.tool);
  expect(names).toContain("remove_work_item");
  expect(names).not.toContain("restore_work_item");
  expect(calls[calls.length - 1]?.destructive).toBe(true);
});
