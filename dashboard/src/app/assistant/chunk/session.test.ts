/**
 * The session's own wiring to the engine: a turn is read again wherever the
 * engine resumes it without a stream to hear it on. A page action this tab
 * applied resumes the turn only once the page has reported, and a refused
 * decision (stale, expired) resumes it with that result.
 */

import { afterEach, expect, test, vi } from "vitest";
import type { AssistantProposalCard } from "~/contract/assistant.ts";
import type { AssistantNarrowFrame } from "~/protocol/assistantWire.ts";

const { call, openTurnStream, streams, refuseApproval } = vi.hoisted(() => {
  const streams: { url: string; lastEventId?: string; frame: (f: unknown) => void }[] = [];
  const refuseApproval: { error: Error | null } = { error: null };
  return {
    streams,
    refuseApproval,
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
        case "POST /assistant/proposals/{id}/approve":
          if (refuseApproval.error) throw refuseApproval.error;
          return { proposal: { state: "applied" }, turn_id: "t1", stream_url: null };
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
  takeAction: vi.fn(async () => true),
}));

const { approve, send, resetSessionForTest } = await import("./session.ts");
const { RestError } = await import("~/protocol/rest.ts");

afterEach(() => {
  call.mockClear();
  openTurnStream.mockClear();
  streams.length = 0;
  resetSessionForTest();
});

const proposal = {
  proposal_id: "p1",
  digest: `sha256:${"a".repeat(64)}`,
} as AssistantProposalCard;

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
      source_revision: "worklet-tasks@1",
      payload: { assignee: "engineer" },
      confirm: "none",
    },
  } as unknown as AssistantNarrowFrame;
  streams[0]!.frame(action);
  await vi.waitFor(() => expect(streams).toHaveLength(2));
  expect(streams[1]!.url).toBe("/assistant/conversations/c1/stream?turn=t1");
  expect(streams[1]!.lastEventId).toMatch(/^t1/);
});

test("a stale approval reads the turn again, which the engine resumed with that result", async () => {
  await send("Move FAIR-3 to in progress", null);
  expect(streams).toHaveLength(1);
  refuseApproval.error = new RestError(409, { error: "proposal_stale" });
  try {
    await approve(proposal);
  } finally {
    refuseApproval.error = null;
  }
  expect(streams).toHaveLength(2);
  expect(streams[1]!.url).toBe("/assistant/conversations/c1/stream?turn=t1");
});
