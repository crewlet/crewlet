/**
 * The session's own wiring to the engine: a turn is read again wherever the
 * engine resumes it without a stream to hear it on. A page action this tab
 * applied resumes the turn only once the page has reported, and a refused
 * decision (stale, expired) resumes it with that result.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, expect, test, vi } from "vitest";
import type { AssistantProposalCard } from "~/contract/assistant.ts";
import {
  createAssistantStreamParser,
  type AssistantFrameOf,
  type AssistantNarrowFrame,
} from "~/protocol/assistantWire.ts";
import { EVERY_QUESTION, session as floors } from "~/protocol/session.ts";
import { repoRoot } from "~/test/engineFiles.ts";

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

const { approve, clearRefusal, decisionFailure, send, sendRefusalOf, resetSessionForTest } =
  await import("./session.ts");
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

test("an approval carries the screen it was made from, which the resumed turn runs on", async () => {
  const { setFramePage, resetPageContextForTest } = await import("../context.ts");
  setFramePage({
    screen: "brand-setup",
    title: "Brand › Set up",
    path: ["brand", "setup"],
    params: {},
    query: {},
  });
  try {
    await approve(proposal);
    const sent = call.mock.calls.find(
      ([route]) => route === "POST /assistant/proposals/{id}/approve",
    );
    expect(sent?.[1]).toMatchObject({
      digest: proposal.digest,
      page_context: { screen: "brand-setup" },
    });
  } finally {
    resetPageContextForTest();
  }
});

test("with no screen published, an approval carries none", async () => {
  await approve(proposal);
  const sent = call.mock.calls.find(
    ([route]) => route === "POST /assistant/proposals/{id}/approve",
  );
  expect(sent?.[1]).not.toHaveProperty("page_context");
});

test("a refused question is said in the engine's words wherever a fixed one could contradict it", () => {
  // THE ASSISTANT'S OWN REASON carries no Retry-After, and its sentence says
  // waiting won't help: no fixed "try again in a moment" is drawn over it.
  const own = sendRefusalOf(
    new RestError(503, {
      error: "assistant_unavailable",
      message: "The assistant can't answer here, and waiting won't change that.",
    }),
  );
  expect(own).toMatchObject({
    sentence: "The assistant can't answer here, and waiting won't change that.",
    retryAfterSeconds: null,
  });

  // A CONDITION OF THE NODE is told in the engine's words for the person, in
  // step with its wait.
  const full =
    "There's no room left to save this kind of change. An administrator needs to make room for it.";
  expect(
    sendRefusalOf(
      new RestError(503, {
        error: "unavailable",
        message: "This can't be done here, and waiting won't change that.",
        detail: full,
      }),
    ),
  ).toMatchObject({ sentence: full, retryAfterSeconds: null });
  const behind =
    "The node handling this change hasn't caught up with the latest changes yet. Try again in a moment.";
  expect(
    sendRefusalOf(
      new RestError(
        503,
        {
          error: "unavailable",
          message: "Can't answer right now. Try again in a moment, or ask another node.",
          detail: behind,
        },
        2,
      ),
    ),
  ).toMatchObject({ sentence: behind, retryAfterSeconds: 2 });

  // A QUESTION WHOSE OUTCOME IS UNKNOWN keeps the code's sentence: its detail
  // is written for a program, which sends the same key again.
  expect(
    sendRefusalOf(
      new RestError(
        503,
        {
          error: "unavailable",
          message: "Can't answer right now. Try again in a moment, or ask another node.",
          detail: "Send it again with the same Idempotency-Key.",
          outcome: "unknown",
          op_id: "op-1",
        },
        2,
      ),
    ).sentence,
  ).toBe("Can't answer right now. Try again in a moment, or ask another node.");
});

test("a decision a condition of the node refused is said in the engine's words for it", () => {
  // THE CONDITION'S OWN WORDS on the card, which say who clears it: the code's
  // fixed sentence asked for a wait whatever the condition was.
  const full =
    "There's no room left to save this kind of change. An administrator needs to make room for it.";
  expect(
    decisionFailure(
      new RestError(503, {
        error: "unavailable",
        message: "This can't be done here, and waiting won't change that.",
        detail: full,
      }),
    ),
  ).toEqual({ refusal: { sentence: full, hint: null }, unknown: false });

  // A DECISION NOBODY CAN CONFIRM is still the card's unknown, approved again
  // under the same key.
  expect(
    decisionFailure(
      new RestError(
        503,
        {
          error: "unavailable",
          message: "Can't answer right now. Try again in a moment, or ask another node.",
          detail: "Couldn't confirm this change was saved.",
          outcome: "unknown",
          op_id: "op-1",
        },
        2,
      ),
    ).unknown,
  ).toBe(true);
});

test("a Clear a condition of the node refused is said in the engine's words for it", () => {
  const full =
    "There's no room left to save this kind of change. An administrator needs to make room for it.";
  expect(
    clearRefusal(
      new RestError(503, {
        error: "unavailable",
        message: "This can't be done here, and waiting won't change that.",
        detail: full,
      }),
    ),
  ).toBe(`The conversation could not be cleared. ${full}`);
  expect(clearRefusal(new Error("offline"))).toBe(
    "The conversation could not be cleared. Try again from Conversations.",
  );
});

/** The frames of a stream the engine's own route wrote, as the client parses them. */
function routeFrames(fixture: string): AssistantNarrowFrame[] {
  const frames: AssistantNarrowFrame[] = [];
  const parser = createAssistantStreamParser({
    frame: (f) => frames.push(f),
    fault: (f) => {
      throw new Error(`the route's fixture does not parse: ${JSON.stringify(f)}`);
    },
  });
  parser.push(
    readFileSync(join(repoRoot(), "internal/api/assistantapi/testdata", fixture), "utf8"),
  );
  return frames;
}

// A WRITE THE ASSISTANT MADE MOVES THE PAGE BEHIND THE DRAWER: the engine's
// own invalidation, as its route writes it, raises the tab's floor for the
// domain it names and asks every live question again. The engine named the
// stream (`CREWLET_TRACKER_LOG`) where the floors are kept by domain, so no
// assistant write ever moved a screen. Mutation: key the floor by anything
// but the frame's domain, or name no questions, and this fails.
test("the engine's invalidation raises the floor it names and asks every question again", async () => {
  const invalidation = routeFrames("write_turn.sse").find(
    (f): f is AssistantFrameOf<"action"> =>
      f.kind === "action" && f.action.name === "context.invalidate",
  )!;
  expect(invalidation).toBeDefined();
  await send("File a task to rotate the keys", null);
  const heard: [string | null, readonly string[]][] = [];
  const stop = floors.onWritten((domain, refreshes) => heard.push([domain, refreshes]));
  try {
    streams[0]!.frame({ ...invalidation, turn_id: "t1" });
    // A write whose answer named no position still asks again, with no floor.
    streams[0]!.frame({
      ...invalidation,
      turn_id: "t1",
      seq: invalidation.seq + 1,
      action: { ...invalidation.action, invalidate: { domain: "", position: "" } },
    });
  } finally {
    stop();
  }
  expect(heard).toEqual([
    ["tracker", [EVERY_QUESTION]],
    [null, [EVERY_QUESTION]],
  ]);
  expect(floors.floor("tracker")).toBe(invalidation.action.invalidate?.position);
});
