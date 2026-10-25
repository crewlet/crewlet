/**
 * The thinking shimmer stands in for an answer not yet drawn: it shows while
 * the turn thinks with nothing on screen, and goes once a card a read built
 * is drawn, rather than sitting between the steps and that card.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import type { CardHandlers } from "./blocks.tsx";
import { Thread } from "./Thread.tsx";
import { EMPTY, type TurnView } from "./turn.ts";

afterEach(cleanup);

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  openHere: () => {},
  undo: () => {},
};

const turn: TurnView = {
  key: "k",
  id: "t",
  opKey: "o",
  phase: "thinking",
  seq: 1,
  question: "Which agents spent the most?",
  page: null,
  askedAt: "2026-10-24T09:41:00Z",
  stepsDone: 0,
  steps: [],
  replies: [],
  cards: [],
  proposals: {},
  actions: [],
  actionStates: {},
  done: null,
  error: null,
  refusal: null,
  stopping: false,
};

const card: AssistantCardEvent = {
  card_id: "c",
  message_id: "m",
  card: {
    type: "entity",
    entity: { kind: "task", id: "FAIR-1", title: "Settle-up algorithm" },
  },
} as AssistantCardEvent;

/** The shimmer, as the design system draws it: a skeleton hidden from assistive technology. */
const SHIMMER = '[class*="crewlet-skeleton"]';

function draw(t: TurnView) {
  return render(
    <Router>
      <Thread
        view={{ ...EMPTY, id: "c1", turns: [t] }}
        now={Date.parse("2026-10-24T09:41:02Z")}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        onReconnect={() => {}}
      />
    </Router>,
  );
}

test("the shimmer shows while the turn thinks with nothing drawn", () => {
  const { container } = draw(turn);
  expect(container.querySelector(SHIMMER)).not.toBeNull();
});

test("the shimmer goes once a card is drawn", () => {
  const { container } = draw({ ...turn, cards: [card] });
  expect(screen.getByText("Settle-up algorithm")).toBeTruthy();
  expect(container.querySelector(SHIMMER)).toBeNull();
});
