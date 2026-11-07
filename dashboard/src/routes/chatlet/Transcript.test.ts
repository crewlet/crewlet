/**
 * What the notice under a room's transcript says about messages that arrived
 * while the reader was reading back, and when the New line glows.
 */

import { renderHook } from "@testing-library/react";
import { expect, test } from "vitest";

import type { ChatMessageView } from "~/protocol/index.ts";

import { arrivalsAnnouncement, arrivalsLabel, edgeOf, useGlowOnce } from "./Transcript.tsx";

// MESSAGES ARRIVE, THEY DO NOT FINISH: the kit's default words are for work
// that finished (a turn, a phase), and the room read "Show 3 new messages that
// finished while you were reading". Mutation: word the label as the kit does.
test("the notice says how many messages arrived, in a room's words", () => {
  expect(arrivalsLabel(1)).toBe("Show 1 new message");
  expect(arrivalsLabel(3)).toBe("Show 3 new messages");
  expect(arrivalsAnnouncement(3)).toBe("3 new messages arrived while you were reading.");
});

// THE NEW LINE GLOWS ONCE PER ENTRY: the windowed list mounts the line again as
// it scrolls back into view, and that mount stays still. A new first unread
// glows again. Mutation: return `line !== ""` and the remount glows.
test("the New line glows the first time it is drawn, not when the window draws it back", () => {
  const glowed = { current: "" };
  const first = renderHook(({ line }) => useGlowOnce(glowed, line), {
    initialProps: { line: "m1" },
  });
  expect(first.result.current).toBe(true);
  first.rerender({ line: "m1" });
  expect(first.result.current).toBe(true);
  first.unmount();

  const back = renderHook(() => useGlowOnce(glowed, "m1"));
  expect(back.result.current).toBe(false);
  expect(renderHook(() => useGlowOnce(glowed, "")).result.current).toBe(false);
  expect(renderHook(() => useGlowOnce(glowed, "m9")).result.current).toBe(true);
});

// HOME AND END ARE THE HELD EDGES, not the drawn ones: the kit picks from the
// rows in the page, which a windowed list cuts short. A room event is no stop.
// Mutation: drop the event filter and End lands on the join line.
test("Home and End land on the first held and the newest message, past room events", () => {
  const view = (id: string, event?: string) =>
    ({ message: { id, ...(event ? { event } : {}) } }) as unknown as ChatMessageView;
  const rows = [view("e0", "joined"), view("m1"), view("m2"), view("e3", "joined")];
  expect(edgeOf(rows, "Home")).toBe("m1");
  expect(edgeOf(rows, "End")).toBe("m2");
  expect(edgeOf(rows, "ArrowUp")).toBe("");
  expect(edgeOf([], "End")).toBe("");
});
