// @vitest-environment node

/**
 * The turn's state machine, row by row.
 *
 * EVERY ROW OF THE TABLE IN `turn.ts` IS A CASE HERE, driven through [reduce]
 * alone: a frame or a gesture in, a phase out. The drawer draws whatever the
 * state says, so a row that moves the wrong way is a drawer that says the
 * wrong thing, and these cases are where that is caught.
 *
 * AND THE ENGINE'S OWN STREAM, replayed: the golden every Go encoder test
 * writes (`internal/assistant/wire/testdata/frames.sse`) is parsed by the
 * client's parser and reduced here, so a frame the engine starts writing
 * differently moves these cases rather than a fixture typed to agree.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";

import { repoRoot } from "~/test/engineFiles.ts";
import type {
  AssistantCardEvent,
  AssistantEventKind,
  AssistantFrame,
  AssistantProposalCard,
} from "~/contract/assistant.ts";
import {
  createAssistantStreamParser,
  type AssistantNarrowFrame,
} from "~/protocol/assistantWire.ts";
import {
  EMPTY,
  activityOf,
  openTurn,
  reduce,
  type ConversationView,
  type TurnEvent,
  type TurnPhase,
} from "./turn.ts";

const TURN = "11111111-1111-4111-8111-111111111111";
const CONV = "22222222-2222-4222-8222-222222222222";

/** A frame of `kind` at `seq` on the test turn, its payload under its kind. */
function frame<K extends AssistantEventKind>(
  kind: K,
  seq: number,
  payload: NonNullable<AssistantFrame[K]>,
): TurnEvent {
  return {
    type: "frame",
    frame: {
      v: 1,
      conversation_id: CONV,
      turn_id: TURN,
      seq,
      at: "2026-10-24T09:41:00Z",
      kind,
      [kind]: payload,
    } as unknown as AssistantNarrowFrame,
  };
}

const STEP = {
  step_id: "33333333-3333-4333-8333-333333333333",
  index: 1,
  tool: "get_spend",
  label: "Read token spend by agent",
  request: { method: "GET", route: "/tokens/breakdown" },
  started_at: "2026-10-24T09:41:00Z",
} as const;

function proposal(state: AssistantProposalCard["state"], scope: "page" | "server" = "server") {
  return {
    card_id: "44444444-4444-4444-8444-444444444444",
    message_id: "55555555-5555-4555-8555-555555555555",
    card: {
      type: "proposal",
      proposal: {
        proposal_id: "66666666-6666-4666-8666-666666666666",
        scope,
        tool: "create_work_item",
        title: "Create task",
        changes: [],
        digest: `sha256:${"a".repeat(64)}`,
        expires_at: "2026-10-24T10:41:00Z",
        state,
        source_revision: "worklet-tasks@00000001",
        approve_label: "Create task",
      },
    },
  } satisfies AssistantCardEvent;
}

const PROPOSAL_ID = "66666666-6666-4666-8666-666666666666";

/** A conversation after `events`, from the home. */
function after(...events: TurnEvent[]): ConversationView {
  return events.reduce(reduce, EMPTY);
}

/** The turn under test is sent and accepted: `thinking`. */
const SENT: TurnEvent[] = [
  {
    type: "send",
    key: "k1",
    text: "Who spent most?",
    page: null,
    attachments: [],
    mode: "ask",
    at: "2026-10-24T09:41:00Z",
  },
  { type: "accepted", key: "k1", turnId: TURN, afterSeq: 0 },
];

function phase(view: ConversationView): TurnPhase {
  return view.turns.at(-1)!.phase;
}

describe("one row per move of the state machine", () => {
  test("Send: a question is sending until the engine accepts it", () => {
    const view = after(SENT[0]!);
    expect(phase(view)).toBe("sending");
    expect(view.turns[0]!.id).toBe("");
    expect(activityOf(view)).toBe("working");
  });

  test("202: accepted, the turn is thinking under the engine's turn id", () => {
    const view = after(...SENT);
    expect(phase(view)).toBe("thinking");
    expect(view.turns[0]!.id).toBe(TURN);
  });

  test("a refused send is refused, with the engine's words, and ends the turn", () => {
    const view = after(SENT[0]!, {
      type: "refused",
      key: "k1",
      refusal: {
        code: "assistant_busy",
        sentence: "Question limit reached.",
        hint: null,
        retryAfterSeconds: 30,
        remedy: null,
      },
    });
    expect(phase(view)).toBe("refused");
    expect(view.turns[0]!.refusal?.retryAfterSeconds).toBe(30);
    expect(openTurn(view)).toBeNull();
  });

  test("thinking, step running: working", () => {
    expect(phase(after(...SENT, frame("step", 1, { ...STEP, state: "running" })))).toBe("working");
  });

  test("working, step done: still working, the step ticked", () => {
    const view = after(
      ...SENT,
      frame("step", 1, { ...STEP, state: "running" }),
      frame("step", 2, { ...STEP, state: "done", duration_ms: 400 }),
    );
    expect(phase(view)).toBe("working");
    expect(view.turns[0]!.steps).toHaveLength(1);
    expect(view.turns[0]!.steps[0]!.state).toBe("done");
  });

  test("thinking, a delta: working, the answer streaming", () => {
    const view = after(...SENT, frame("delta", 1, { message_id: "m1", text: "Agent SWE" }));
    expect(phase(view)).toBe("working");
    expect(view.turns[0]!.replies).toEqual([{ id: "m1", text: "Agent SWE" }]);
  });

  test("a delta with restart replaces what it drew rather than appending", () => {
    const view = after(
      ...SENT,
      frame("delta", 1, { message_id: "m1", text: "Agent SWE" }),
      frame("delta", 2, { message_id: "m1", text: "Agent CTO", restart: true }),
    );
    expect(view.turns[0]!.replies[0]!.text).toBe("Agent CTO");
  });

  test("thinking or working, a proposal card: awaiting approval", () => {
    const view = after(...SENT, frame("card", 1, proposal("proposed")));
    expect(phase(view)).toBe("awaiting_approval");
    expect(activityOf(view)).toBe("waiting");
  });

  test("a page action the screen declared needs no approval: applying", () => {
    const view = after(
      ...SENT,
      frame("action", 1, { action_id: "a1", name: "work.filters.apply", confirm: "none" }),
    );
    expect(phase(view)).toBe("applying");
  });

  test("the reserved invalidate action moves no phase", () => {
    const view = after(
      ...SENT,
      frame("action", 1, {
        action_id: "a2",
        name: "context.invalidate",
        invalidate: { domain: "tracker", position: "42" },
      }),
    );
    expect(phase(view)).toBe("thinking");
    expect(view.turns[0]!.actions).toHaveLength(1);
  });

  test("awaiting approval, Approve: applying", () => {
    const view = after(...SENT, frame("card", 1, proposal("proposed")), {
      type: "approve",
      proposalId: PROPOSAL_ID,
    });
    expect(phase(view)).toBe("applying");
  });

  test("awaiting approval, the page moved: still waiting, the page card marked stale", () => {
    const view = after(...SENT, frame("card", 1, proposal("proposed", "page")), {
      type: "page_moved",
      revision: "worklet-tasks@00000002",
      revisions: {},
    });
    expect(phase(view)).toBe("awaiting_approval");
    expect(view.turns[0]!.proposals[PROPOSAL_ID]?.pageMoved).toBe(true);
  });

  test("a server proposal is not stale because the page moved", () => {
    const view = after(...SENT, frame("card", 1, proposal("proposed", "server")), {
      type: "page_moved",
      revision: "worklet-tasks@00000002",
      revisions: {},
    });
    expect(view.turns[0]!.proposals[PROPOSAL_ID]).toBeUndefined();
  });

  test("applying, the change settled: working, the card at its state", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed")),
      { type: "approve", proposalId: PROPOSAL_ID },
      { type: "approval_settled", proposalId: PROPOSAL_ID, state: "applied" },
    );
    expect(phase(view)).toBe("working");
    const card = view.turns[0]!.cards[0]!;
    expect(card.card.proposal?.state).toBe("applied");
  });

  test("applying, refused or stale: working again, the failure kept on the card", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed")),
      { type: "approve", proposalId: PROPOSAL_ID },
      {
        type: "approval_failed",
        proposalId: PROPOSAL_ID,
        refusal: { sentence: "Not permitted.", hint: "Ask an admin." },
        unknown: false,
      },
    );
    expect(phase(view)).toBe("awaiting_approval");
    expect(view.turns[0]!.proposals[PROPOSAL_ID]?.refusal).toEqual({
      sentence: "Not permitted.",
      hint: "Ask an admin.",
    });
    // THE NEXT PRESS CLEARS IT: the refusal was about the decision before.
    const again = reduce(view, { type: "approve", proposalId: PROPOSAL_ID });
    expect(again.turns[0]!.proposals[PROPOSAL_ID]?.refusal).toBeNull();
  });

  test("applying, the page applied it: working, the card applied with the page's receipt", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed", "page")),
      { type: "approve", proposalId: PROPOSAL_ID },
      frame("action", 2, { action_id: "a3", name: "work.filter.apply", proposal_id: PROPOSAL_ID }),
      {
        type: "action_local",
        actionId: "a3",
        local: { state: "applied", words: "Applied to Worklet › Tasks.", undoable: false },
      },
    );
    expect(phase(view)).toBe("working");
    const settled = view.turns[0]!.cards[0]!.card.proposal;
    expect(settled?.state).toBe("applied");
    expect(settled?.receipt?.summary).toBe("Applied to Worklet › Tasks.");
  });

  test("applying, the page refused it: working, the card failed in the screen's own words", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed", "page")),
      { type: "approve", proposalId: PROPOSAL_ID },
      frame("action", 2, { action_id: "a3", name: "work.filter.apply", proposal_id: PROPOSAL_ID }),
      {
        type: "action_local",
        actionId: "a3",
        local: { state: "blocked", words: "That filter is what the page is.", undoable: false },
      },
    );
    expect(phase(view)).toBe("working");
    expect(view.turns[0]!.cards[0]!.card.proposal?.state).toBe("failed");
    expect(view.turns[0]!.proposals[PROPOSAL_ID]?.failure).toBe("That filter is what the page is.");
  });

  test("applying, another tab took it: the card is left for the engine to move", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed", "page")),
      { type: "approve", proposalId: PROPOSAL_ID },
      frame("action", 2, { action_id: "a3", name: "work.filter.apply", proposal_id: PROPOSAL_ID }),
      {
        type: "action_local",
        actionId: "a3",
        local: { state: "elsewhere", words: "Applied in another tab of Crewlet.", undoable: false },
      },
    );
    expect(phase(view)).toBe("applying");
    expect(view.turns[0]!.cards[0]!.card.proposal?.state).toBe("proposed");
  });

  test("a view change with no card, applied: working, its receipt kept for the thread", () => {
    const view = after(
      ...SENT,
      frame("action", 1, { action_id: "a1", name: "spend.window.apply", confirm: "none" }),
      {
        type: "action_local",
        actionId: "a1",
        local: { state: "applied", words: "Applied to Spend.", undoable: true },
      },
    );
    expect(phase(view)).toBe("working");
    expect(view.turns[0]!.actionStates.a1).toMatchObject({ state: "applied", undoable: true });
  });

  test("applying, unknown: unconfirmed, and the turn stays open for a retry under the same key", () => {
    const view = after(
      ...SENT,
      frame("card", 1, proposal("proposed")),
      { type: "approve", proposalId: PROPOSAL_ID },
      {
        type: "approval_failed",
        proposalId: PROPOSAL_ID,
        refusal: { sentence: "Could not confirm.", hint: null },
        unknown: true,
      },
    );
    expect(phase(view)).toBe("unconfirmed");
    expect(openTurn(view)).not.toBeNull();
  });

  test("any open, done: the done's own state", () => {
    for (const [state, want] of [
      ["completed", "completed"],
      ["stopped", "stopped"],
      ["failed", "failed"],
      ["interrupted", "interrupted"],
      ["expired", "expired"],
      ["budget_exhausted", "budget_exhausted"],
      ["superseded", "completed"],
      ["waiting_for_approval", "awaiting_approval"],
    ] as const) {
      const view = after(...SENT, frame("done", 1, { state, duration_ms: 5000, steps: 2 }));
      expect(phase(view), state).toBe(want);
    }
  });

  test("any open, Stop: stopping until the engine answers, then stopped", () => {
    const stopping = after(...SENT, { type: "stopping", turnId: TURN });
    expect(stopping.turns[0]!.stopping).toBe(true);
    expect(phase(stopping)).toBe("thinking");
    const stopped = reduce(stopping, { type: "stopped", turnId: TURN, state: "stopped" });
    expect(phase(stopped)).toBe("stopped");
    expect(stopped.turns[0]!.stopping).toBe(false);
  });

  test("a stop that failed is not a stopped turn", () => {
    const view = after(
      ...SENT,
      { type: "stopping", turnId: TURN },
      { type: "stop_failed", turnId: TURN },
    );
    expect(view.turns[0]!.stopping).toBe(false);
    expect(phase(view)).toBe("thinking");
  });

  test("an error frame fails an open turn and keeps the engine's sentence", () => {
    const view = after(
      ...SENT,
      frame("error", 1, {
        code: "model_unavailable",
        message: "No model answered.",
        retryable: true,
      }),
    );
    expect(phase(view)).toBe("failed");
    expect(view.turns[0]!.error?.message).toBe("No model answered.");
  });

  test("an ended turn is not moved by a late status", () => {
    const view = after(
      ...SENT,
      frame("done", 1, { state: "completed", duration_ms: 1, steps: 0 }),
      frame("status", 2, { state: "working", steps_done: 1 }),
    );
    expect(phase(view)).toBe("completed");
  });

  test("the page a question carried is the one the engine says it used, and none when unused", () => {
    const used = after(
      ...SENT,
      frame("page_context", 1, {
        used: true,
        trimmed: false,
        page: { title: "Worklet › Tasks", screen: "worklet-tasks" },
      }),
    );
    expect(used.turns[0]!.page?.title).toBe("Worklet › Tasks");
    const unused = after(...SENT, frame("page_context", 1, { used: false, trimmed: false }));
    expect(unused.turns[0]!.page).toBeNull();
  });
});

describe("a resumed stream never draws a frame twice", () => {
  test("a frame at or below the seq a turn has is dropped", () => {
    const delta = frame("delta", 1, { message_id: "m1", text: "once" });
    const view = after(...SENT, delta, delta, frame("delta", 1, { message_id: "m1", text: "x" }));
    expect(view.turns[0]!.replies[0]!.text).toBe("once");
    expect(view.turns[0]!.seq).toBe(1);
  });

  test("the seq a turn was accepted after is where it starts", () => {
    const view = after(
      SENT[0]!,
      { type: "accepted", key: "k1", turnId: TURN, afterSeq: 4 },
      frame("delta", 3, { message_id: "m1", text: "old" }),
    );
    expect(view.turns[0]!.replies).toEqual([]);
  });

  test("a snapshot replaces the turn's steps, replies and cards whole", () => {
    const view = after(
      ...SENT,
      frame("delta", 1, { message_id: "m1", text: "partial" }),
      frame("snapshot", 5, {
        messages: [
          {
            id: "q",
            turn_id: TURN,
            role: "user",
            text: "Who?",
            created_at: "2026-10-24T09:41:00Z",
          },
          {
            id: "m1",
            turn_id: TURN,
            role: "assistant",
            text: "Whole answer.",
            created_at: "2026-10-24T09:41:01Z",
          },
        ],
        steps: [{ ...STEP, state: "done" }],
        cards: [],
      }),
    );
    expect(view.turns[0]!.replies).toEqual([{ id: "m1", text: "Whole answer." }]);
    expect(view.turns[0]!.steps).toHaveLength(1);
  });

  test("a frame for a turn the drawer does not hold changes nothing", () => {
    const view = after(...SENT);
    const stray = reduce(view, {
      type: "frame",
      frame: {
        ...(frame("delta", 9, { message_id: "m", text: "x" }) as { frame: AssistantNarrowFrame })
          .frame,
        turn_id: "other",
      },
    });
    expect(stray).toBe(view);
  });
});

describe("the engine's golden stream, reduced", () => {
  const golden = readFileSync(
    join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
    "utf8",
  );
  const frames: AssistantNarrowFrame[] = [];
  createAssistantStreamParser({ frame: (f) => frames.push(f), fault: () => {} }).push(golden);
  const turns = [...new Set(frames.map((f) => f.turn_id))];

  test("a whole turn ends in the done frame's state with its steps, replies and cards", () => {
    const first = turns[0]!;
    let view = after(
      {
        type: "send",
        key: "g1",
        text: "Fix the flaky sign-in test",
        page: null,
        attachments: [],
        mode: "ask",
        at: "2026-10-24T09:41:00Z",
      },
      { type: "accepted", key: "g1", turnId: first, afterSeq: 0 },
    );
    for (const f of frames.filter((x) => x.turn_id === first))
      view = reduce(view, { type: "frame", frame: f });
    const turn = view.turns[0]!;
    const done = frames.find((f) => f.turn_id === first && f.kind === "done");
    expect(done?.kind).toBe("done");
    expect(turn.phase).toBe(
      done?.kind === "done"
        ? done.done.state === "waiting_for_approval"
          ? "awaiting_approval"
          : done.done.state
        : "",
    );
    expect(turn.steps.length).toBeGreaterThan(0);
    expect(turn.replies.length).toBeGreaterThan(0);
    expect(turn.cards.length).toBe(
      frames.filter((f) => f.turn_id === first && f.kind === "card").length,
    );
    expect(view.title).not.toBe("");
  });

  test("replayed twice, a turn reads exactly as once", () => {
    const first = turns[0]!;
    const start = after(
      {
        type: "send",
        key: "g1",
        text: "q",
        page: null,
        attachments: [],
        mode: "ask",
        at: "2026-10-24T09:41:00Z",
      },
      { type: "accepted", key: "g1", turnId: first, afterSeq: 0 },
    );
    const own = frames.filter((x) => x.turn_id === first);
    const once = own.reduce((v, f) => reduce(v, { type: "frame", frame: f }), start);
    const twice = [...own, ...own].reduce((v, f) => reduce(v, { type: "frame", frame: f }), start);
    expect(twice).toEqual(once);
  });

  test("a resume that could not be filled draws the snapshot, then the error, then ends", () => {
    const second = turns[1]!;
    let view = after(
      {
        type: "send",
        key: "g2",
        text: "q",
        page: null,
        attachments: [],
        mode: "ask",
        at: "2026-10-24T09:42:00Z",
      },
      { type: "accepted", key: "g2", turnId: second, afterSeq: 0 },
    );
    for (const f of frames.filter((x) => x.turn_id === second))
      view = reduce(view, { type: "frame", frame: f });
    const turn = view.turns[0]!;
    expect(turn.error).not.toBeNull();
    expect(openTurn(view)).toBeNull();
  });
});

describe("a reloaded transcript's compliance reads", () => {
  test("are kept on the conversation for its owner to see, and a new conversation has none (D06)", () => {
    const reads = [{ reader: "admin.ada", reason: "Legal hold 42", at: "2026-10-24T10:00:00Z" }];
    const view = after({
      type: "transcript",
      transcript: {
        conversation: {
          id: CONV,
          title: "Spend",
          created_at: "2026-10-24T09:00:00Z",
          updated_at: "2026-10-24T09:05:00Z",
          last_activity_at: "2026-10-24T09:05:00Z",
        },
        messages: [],
        steps: [],
        cards: [],
        open_turn: null,
        audit_reads: reads,
        turns: [],
      },
    });
    expect(view.auditReads).toEqual(reads);
    expect(reduce(view, { type: "reset" }).auditReads).toEqual([]);
  });
});

describe("a reloaded transcript", () => {
  test("is grouped by turn, the page each question carried kept, and the open turn left open", () => {
    const view = after({
      type: "transcript",
      transcript: {
        conversation: {
          id: CONV,
          title: "Spend",
          created_at: "2026-10-24T09:00:00Z",
          updated_at: "2026-10-24T09:05:00Z",
          last_activity_at: "2026-10-24T09:05:00Z",
        },
        messages: [
          {
            id: "q1",
            turn_id: "t1",
            role: "user",
            text: "One?",
            created_at: "2026-10-24T09:00:00Z",
            page: { title: "Worklet › Tasks", screen: "worklet-tasks" },
          },
          {
            id: "a1",
            turn_id: "t1",
            role: "assistant",
            text: "One.",
            created_at: "2026-10-24T09:00:01Z",
          },
          {
            id: "q2",
            turn_id: "t2",
            role: "user",
            text: "Two?",
            created_at: "2026-10-24T09:05:00Z",
          },
        ],
        steps: [],
        cards: [],
        open_turn: {
          turn_id: "t2",
          state: "running",
          stream_url: "/assistant/conversations/x/stream?turn=t2",
          after_seq: 7,
        },
        audit_reads: [],
        turns: [],
      },
    });
    expect(view.turns.map((t) => [t.question, t.phase, t.page?.title ?? null])).toEqual([
      ["One?", "completed", "Worklet › Tasks"],
      ["Two?", "working", null],
    ]);
    expect(view.turns[1]!.seq).toBe(7);
    // ANSWERED WHEN ITS LAST REPLY WAS WRITTEN, and not yet for a turn with none.
    expect(view.turns.map((t) => t.answeredAt)).toEqual(["2026-10-24T09:00:01Z", null]);
  });
});

// A TURN'S FOOT SAYS WHEN IT ANSWERED, not when it was asked: a turn that
// waited minutes on an approval read "7m ago" the moment it finished.
// Mutation: leave answeredAt to the question's time, or ignore the done frame.
describe("when a turn answered", () => {
  test("is its done frame's time", () => {
    const view = after(...SENT, {
      type: "frame",
      frame: {
        v: 1,
        conversation_id: CONV,
        turn_id: TURN,
        seq: 9,
        at: "2026-10-24T09:48:00Z",
        kind: "done",
        done: { state: "completed", duration_ms: 420_000, steps: 3 },
      } as unknown as AssistantNarrowFrame,
    });
    const turn = view.turns[0]!;
    expect(turn.answeredAt).toBe("2026-10-24T09:48:00Z");
    expect(turn.answeredAt).not.toBe(turn.askedAt);
  });
});

describe("a reloaded transcript's ended turns", () => {
  test("a turn stopped or failed before it wrote a word keeps its outcome", () => {
    const view = after({
      type: "transcript",
      transcript: {
        conversation: {
          id: CONV,
          title: "awdwad",
          created_at: "2026-10-24T09:00:00Z",
          updated_at: "2026-10-24T09:05:00Z",
          last_activity_at: "2026-10-24T09:05:00Z",
        },
        messages: [
          {
            id: "q1",
            turn_id: "t1",
            role: "user",
            text: "awdwad",
            created_at: "2026-10-24T09:00:00Z",
          },
          {
            id: "q2",
            turn_id: "t2",
            role: "user",
            text: "Add a role",
            created_at: "2026-10-24T09:01:00Z",
          },
          {
            id: "q3",
            turn_id: "t3",
            role: "user",
            text: "Agents?",
            created_at: "2026-10-24T09:02:00Z",
          },
        ],
        steps: [],
        cards: [],
        open_turn: null,
        audit_reads: [],
        turns: [
          { turn_id: "t1", state: "stopped" },
          {
            turn_id: "t2",
            state: "failed",
            error: {
              code: "model_timeout",
              message: "Ask Crewlet's model took too long to answer.",
              retryable: false,
            },
          },
          { turn_id: "t3", state: "completed" },
        ],
      },
    });
    expect(view.turns.map((t) => [t.question, t.phase, t.error?.code ?? null])).toEqual([
      ["awdwad", "stopped", null],
      ["Add a role", "failed", "model_timeout"],
      ["Agents?", "completed", null],
    ]);
  });
});

describe("a reloaded transcript's steps", () => {
  const step = (id: string, turn: string | undefined, index: number) => ({
    step_id: id,
    ...(turn === undefined ? {} : { turn_id: turn }),
    index,
    tool: "list_agents",
    label: "Read the agents",
    state: "done" as const,
    started_at: "2026-10-24T09:00:00Z",
  });

  test("are drawn under the turn each names, and one naming none under no turn", () => {
    const view = after({
      type: "transcript",
      transcript: {
        conversation: {
          id: CONV,
          title: "",
          created_at: "2026-10-24T09:00:00Z",
          updated_at: "2026-10-24T09:05:00Z",
          last_activity_at: "2026-10-24T09:05:00Z",
        },
        messages: [
          {
            id: "q1",
            turn_id: "t1",
            role: "user",
            text: "One?",
            created_at: "2026-10-24T09:00:00Z",
          },
          {
            id: "q2",
            turn_id: "t2",
            role: "user",
            text: "Two?",
            created_at: "2026-10-24T09:05:00Z",
          },
        ],
        steps: [
          step("s1", "t1", 1),
          step("s2", "t2", 1),
          step("s3", "t2", 2),
          step("s4", undefined, 1),
        ],
        cards: [],
        open_turn: null,
        audit_reads: [],
        turns: [],
      },
    });
    expect(view.turns.map((t) => t.steps.map((s) => s.step_id))).toEqual([["s1"], ["s2", "s3"]]);
  });
});
