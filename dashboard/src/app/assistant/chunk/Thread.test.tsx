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
  prepareAgain: () => {},
  openHere: () => {},
  undo: () => {},
};

const turn: TurnView = {
  key: "k",
  id: "t",
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

function draw(t: TurnView, retryDisabledReason?: string) {
  return render(
    <Router>
      <Thread
        view={{ ...EMPTY, id: "c1", turns: [t] }}
        now={Date.parse("2026-10-24T09:41:02Z")}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        retryDisabledReason={retryDisabledReason}
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

test("a run that took no step has no count and nothing to open", () => {
  const { container } = draw({
    ...turn,
    phase: "completed",
    done: { state: "completed", duration_ms: 1000, steps: 0 },
    replies: [{ id: "m", text: "Nothing to look up." }],
  });
  expect(container.textContent).not.toContain("0 steps");
  expect(container.querySelector("[aria-expanded]")).toBeNull();
});

const failed = (error: NonNullable<TurnView["error"]>): TurnView => ({
  ...turn,
  phase: "failed",
  done: { state: "failed", duration_ms: 1000, steps: 0 },
  error,
});

// A MODEL FAILURE IS TOLD IN THE ENGINE'S WORDS, with its hint, and Try again
// is offered only where asking again can help: not for a signed-out model.
test("a signed-out model says so with its hint and offers no Try again", () => {
  draw(
    failed({
      code: "model_signed_out",
      message: "The assistant's model is signed out, so it cannot answer.",
      hint: "Ask an administrator to sign the assistant's model in again.",
      retryable: false,
    }),
  );
  expect(
    screen.getByText("The assistant's model is signed out, so it cannot answer."),
  ).toBeTruthy();
  expect(document.body.textContent).toContain(
    "Ask an administrator to sign the assistant's model in again.",
  );
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
});

test("a model that took too long offers Try again, held off while Send is", () => {
  const timeout = failed({
    code: "model_timeout",
    message: "The assistant's model took too long to answer.",
    hint: "Ask again, or ask a narrower question.",
    retryable: true,
  });
  const { unmount } = draw(timeout);
  expect(screen.getByRole("button", { name: "Try again" }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
  unmount();
  draw(timeout, "The assistant's model is signed out");
  expect(screen.getByRole("button", { name: /Try again/ }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});
