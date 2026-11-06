/**
 * What the notice under a room's transcript says about messages that arrived
 * while the reader was reading back.
 */

import { expect, test } from "vitest";

import { arrivalsAnnouncement, arrivalsLabel } from "./Transcript.tsx";

// MESSAGES ARRIVE, THEY DO NOT FINISH: the kit's default words are for work
// that finished (a turn, a phase), and the room read "Show 3 new messages that
// finished while you were reading". Mutation: word the label as the kit does.
test("the notice says how many messages arrived, in a room's words", () => {
  expect(arrivalsLabel(1)).toBe("Show 1 new message");
  expect(arrivalsLabel(3)).toBe("Show 3 new messages");
  expect(arrivalsAnnouncement(3)).toBe("3 new messages arrived while you were reading.");
});
