// @vitest-environment node

/**
 * A conversation key as a row prints it: the Memory tab's episodes column and
 * the subtitle of a session's transcript.
 */

import { expect, test } from "vitest";
import { conversationLabel } from "./conversationKey.ts";

// A ROW PRINTS A UUID'S HEAD — the way every narrow row names one — and
// nothing else in a key changes, so `work:ENG-32` stays whole.
//
// Mutation: cut every key at a fixed width, or drop the uuid cut, and a
// Worklet task's key is printed whole or another source's is cut.
test("a conversation key's uuids are cut to their heads, and nothing else is", () => {
  expect(conversationLabel("work:task:4d631f6d-5107-46f0-8043-a1f9d7a74abc")).toBe(
    "work:task:4d631f6d",
  );
  expect(conversationLabel("work:ENG-32")).toBe("work:ENG-32");
  expect(conversationLabel("slack:C9:1718.001")).toBe("slack:C9:1718.001");
  expect(conversationLabel("")).toBe("");
});

// A SCHEDULE'S KEY READS AS ITS SCHEDULE WAS WRITTEN: the engine escapes each
// part so a slash in a name cannot merge two schedules, and a row prints the
// parts decoded — a role schedule's seat id cut like any uuid, a part that
// does not decode shown as sent, and a key of any other source untouched.
//
// Mutation: decode no part, return a schedule's key before its uuids are cut,
// let a part that does not decode throw, or decode a key of another source,
// and one of these reads otherwise.
test("a schedule's conversation key prints its parts as they were written", () => {
  expect(conversationLabel("schedule/unit/eng/weekly%20report%20%E2%80%94%20Q3")).toBe(
    "schedule/unit/eng/weekly report — Q3",
  );
  expect(conversationLabel("schedule/role/4d631f6d-5107-46f0-8043-a1f9d7a74abc/health-check")).toBe(
    "schedule/role/4d631f6d/health-check",
  );
  expect(conversationLabel("schedule/unit/eng/100%25%20done")).toBe("schedule/unit/eng/100% done");
  expect(conversationLabel("schedule/unit/eng/broken%E2%80")).toBe(
    "schedule/unit/eng/broken%E2%80",
  );
  expect(conversationLabel("jira:ENG-42%20x")).toBe("jira:ENG-42%20x");
});
