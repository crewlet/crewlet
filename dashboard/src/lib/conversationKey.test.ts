// @vitest-environment node

/**
 * A conversation key as a row prints it: the Memory tab's episodes column, the
 * subtitle of a session's transcript, a turn's page and its phase cards.
 */

import { expect, test } from "vitest";
import { conversationLabel, conversationRole, isScheduleConversation } from "./conversationKey.ts";

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

// A SCHEDULE'S KEY NAMES THE SCHEDULE A TURN WAS A FIRE OF, known by the
// engine's own prefix alone: no surface holds it and nobody replies into it,
// so it is never called the external conversation a turn served — and a key a
// source derives, which starts with its source and a colon, always is.
//
// Mutation: match the prefix anywhere in the key, drop its slash, or word a
// schedule's key as an external conversation, and one of these reads otherwise.
test("a schedule's key is said to be the schedule a turn was a fire of", () => {
  expect(isScheduleConversation("schedule/unit/eng/weekly%20report")).toBe(true);
  expect(isScheduleConversation("schedule:ops")).toBe(false);
  expect(isScheduleConversation("jira:schedule/unit/eng")).toBe(false);
  expect(isScheduleConversation("")).toBe(false);
  expect(conversationRole("schedule/role/4d631f6d-5107-46f0-8043-a1f9d7a74abc/check")).toBe(
    "the schedule this turn was a fire of",
  );
  expect(conversationRole("slack:C9:1718.001")).toBe("the external conversation this turn served");
});
