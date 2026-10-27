/**
 * A turn that stopped or failed says what it left changed, from its own
 * receipts: "Nothing changed." only where no change of the turn applied or
 * may have landed, since a stop ends what is left, not what was done.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { AssistantCardEvent, AssistantProposalCard } from "~/contract/assistant.ts";
import type { AssistantNarrowFrame } from "~/protocol/assistantWire.ts";
import { Router } from "~/app/router.tsx";
import type { CardHandlers } from "./blocks.tsx";
import { Thread } from "./Thread.tsx";
import { EMPTY, reduce, type ConversationView, type TurnEvent } from "./turn.ts";

afterEach(cleanup);

const TURN = "11111111-1111-4111-8111-111111111111";

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

/** The turn, sent and accepted. */
const SENT: TurnEvent[] = [
  {
    type: "send",
    key: "k1",
    text: "Rename it",
    page: null,
    attachments: [],
    mode: "ask",
    at: "2026-10-24T09:41:00Z",
  },
  { type: "accepted", key: "k1", turnId: TURN, afterSeq: 0 },
];

/** A proposal card at `state`, as the engine's card frame carries it. */
function proposal(state: AssistantProposalCard["state"]): TurnEvent {
  const card: AssistantCardEvent = {
    card_id: "c1",
    message_id: "m1",
    card: {
      type: "proposal",
      proposal: {
        proposal_id: "p1",
        scope: "server",
        tool: "update_task",
        title: "Rename the task",
        changes: [],
        digest: `sha256:${"a".repeat(64)}`,
        expires_at: "2026-10-24T10:41:00Z",
        state,
        approve_label: "Rename",
      },
    },
  };
  return {
    type: "frame",
    frame: {
      v: 1,
      conversation_id: "c",
      turn_id: TURN,
      seq: 1,
      at: "2026-10-24T09:41:00Z",
      kind: "card",
      card,
    } as unknown as AssistantNarrowFrame,
  };
}

const STOPPED: TurnEvent = { type: "stopped", turnId: TURN, state: "stopped" };

function draw(...events: TurnEvent[]) {
  const view: ConversationView = events.reduce(reduce, EMPTY);
  render(
    <Router>
      <Thread
        view={view}
        now={Date.parse("2026-10-24T09:42:00Z")}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        onReconnect={() => {}}
      />
    </Router>,
  );
}

test("a stopped turn that changed nothing says so", () => {
  draw(...SENT, STOPPED);
  expect(screen.getByText("Stopped. Nothing changed.")).toBeDefined();
});

test("a stopped turn whose approved change applied does not say nothing changed", () => {
  draw(...SENT, proposal("applied"), STOPPED);
  expect(screen.queryByText("Stopped. Nothing changed.")).toBeNull();
  expect(screen.getByText("Stopped. What was already applied stays applied.")).toBeDefined();
});

test("a stopped turn whose change nobody heard back from says it may have landed", () => {
  draw(...SENT, proposal("unknown"), STOPPED);
  expect(screen.queryByText("Stopped. Nothing changed.")).toBeNull();
  expect(
    screen.getByText("Stopped. A change may have landed. Its card says where it stands."),
  ).toBeDefined();
});

test("a stopped turn whose page change applied does not say nothing changed", () => {
  draw(
    ...SENT,
    {
      type: "frame",
      frame: {
        v: 1,
        conversation_id: "c",
        turn_id: TURN,
        seq: 1,
        at: "2026-10-24T09:41:00Z",
        kind: "action",
        action: { action_id: "a1", name: "work.filters.apply", confirm: "none" },
      } as unknown as AssistantNarrowFrame,
    },
    {
      type: "action_local",
      actionId: "a1",
      local: { state: "applied", words: "Filtered to open tasks.", undoable: true },
    },
    STOPPED,
  );
  expect(screen.getByText("Stopped. What was already applied stays applied.")).toBeDefined();
});

// A SILENT ROUND DRAWS NOTHING OF ITS OWN: its message is stored empty so its
// cards have an anchor, and the thread draws the cards without an empty
// paragraph above them. Mutation: draw every reply and an empty prose block
// appears.
test("an empty reply draws no prose", () => {
  const { container } = render(
    <Router>
      <Thread
        view={[...SENT, STOPPED].reduce(reduce, EMPTY)}
        now={Date.parse("2026-10-24T09:42:00Z")}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        onReconnect={() => {}}
      />
    </Router>,
  );
  const before = container.querySelectorAll(".md").length;
  cleanup();
  const silent: TurnEvent = {
    type: "frame",
    frame: {
      v: 1,
      conversation_id: "c",
      turn_id: TURN,
      seq: 1,
      at: "2026-10-24T09:41:00Z",
      kind: "delta",
      delta: { message_id: "m0", text: "" },
    } as unknown as AssistantNarrowFrame,
  };
  const after = render(
    <Router>
      <Thread
        view={[...SENT, silent, STOPPED].reduce(reduce, EMPTY)}
        now={Date.parse("2026-10-24T09:42:00Z")}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        onReconnect={() => {}}
      />
    </Router>,
  );
  expect(after.container.querySelectorAll(".md").length).toBe(before);
});
