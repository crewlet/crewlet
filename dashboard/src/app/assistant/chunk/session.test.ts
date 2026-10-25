/**
 * The session's own wiring to the engine: a page action this tab applied is
 * followed by a fresh read of its turn, since the turn resumes only once the
 * page has reported.
 */

import { afterEach, expect, test, vi } from "vitest";
import type { AssistantNarrowFrame } from "~/protocol/assistantWire.ts";

const { call, openTurnStream, streams } = vi.hoisted(() => {
  const streams: { url: string; lastEventId?: string; frame: (f: unknown) => void }[] = [];
  return {
    streams,
    call: vi.fn(async (route: string, ..._rest: unknown[]) => {
      switch (route) {
        case "POST /assistant/conversations":
          return { conversation: { id: "c1", title: "" } };
        case "POST /assistant/conversations/{id}/turns":
          return {
            turn_id: "t1",
            stream_url: "/assistant/conversations/c1/stream?turn=t1",
            after_seq: 0,
          };
        default:
          return { proposal: { state: "declined" }, turn_id: "t", stream_url: null };
      }
    }),
    openTurnStream: vi.fn(
      (
        url: string,
        handlers: { frame: (f: unknown) => void },
        options: { lastEventId?: string } = {},
      ) => {
        streams.push({ url, lastEventId: options.lastEventId, frame: handlers.frame });
        return { close: () => {} };
      },
    ),
  };
});

vi.mock("~/protocol/assistant.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("~/protocol/assistant.ts")>()),
  call,
  openTurnStream,
}));

vi.mock("./pageActions.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./pageActions.ts")>()),
  takeAction: vi.fn(async () => {}),
}));

const { send, resetSessionForTest } = await import("./session.ts");

afterEach(() => {
  call.mockClear();
  openTurnStream.mockClear();
  streams.length = 0;
  resetSessionForTest();
});

test("after a page action this tab applied, the turn is read again from its last frame", async () => {
  await send("Filter my work to the engineer", null);
  expect(streams).toHaveLength(1);
  const action = {
    v: 1,
    conversation_id: "c1",
    turn_id: "t1",
    seq: 3,
    at: "2026-10-25T09:00:42Z",
    kind: "action",
    action: {
      action_id: "a1",
      name: "work.filter.apply",
      source_revision: "work-tasks@1",
      payload: { assignee: "engineer" },
      confirm: "none",
    },
  } as unknown as AssistantNarrowFrame;
  streams[0]!.frame(action);
  await vi.waitFor(() => expect(streams).toHaveLength(2));
  expect(streams[1]!.url).toBe("/assistant/conversations/c1/stream?turn=t1");
  expect(streams[1]!.lastEventId).toMatch(/^t1/);
});
