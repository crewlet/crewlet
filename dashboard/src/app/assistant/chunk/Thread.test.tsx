/**
 * The thinking indicator is the turn's live line: the Assistant's mark after
 * everything the turn has drawn, saying Thinking or the running step, and the
 * mark alone while the answer's words arrive. No caret is drawn in the words,
 * and the run chip is drawn from the first step, closed while the turn runs.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { AssistantCardEvent } from "~/contract/assistant.ts";
import { Router } from "~/app/router.tsx";
import type { CardHandlers } from "./blocks.tsx";
import { mentionsOf, readsOf } from "./mentions.ts";
import { Thread } from "./Thread.tsx";
import { EMPTY, type TurnView } from "./turn.ts";

// THE READS AN ANSWER WORKS OUT, counted: the real ones, through a spy.
vi.mock("./mentions.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("./mentions.ts")>();
  return { ...real, readsOf: vi.fn(real.readsOf), mentionsOf: vi.fn(real.mentionsOf) };
});

afterEach(cleanup);

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

const turn: TurnView = {
  key: "k",
  id: "t",
  phase: "thinking",
  seq: 1,
  question: "Which agents spent the most?",
  page: null,
  attachments: [],
  askedAt: "2026-10-24T09:41:00Z",
  answeredAt: null,
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

/** The indicator, its status line, and a caret, as the design system draws them. */
const INDICATOR = ".crewlet-thinking";
const STATUS = ".crewlet-thinking__status";
const CARET = ".crewlet-prose--streaming";

const running = {
  step_id: "s",
  index: 1,
  tool: "list_agents",
  label: "Listing the agents",
  state: "running" as const,
  started_at: "2026-10-24T09:41:00Z",
};

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

test("says Thinking once, with no run chip and no placeholder, before the first step", () => {
  const { container } = draw(turn);
  expect(container.querySelector(STATUS)!.textContent).toBe("Thinking");
  expect(container.querySelector('[class*="crewlet-skeleton"]')).toBeNull();
  expect(container.querySelector(".crewlet-run-chip")).toBeNull();
  // Announced once, through the indicator's own status region.
  expect(screen.getAllByRole("status").map((s) => s.textContent)).toEqual(["Thinking"]);
});

test("says the running step, under a run chip that stays closed while it runs", () => {
  const { container } = draw({ ...turn, phase: "working", steps: [running] });
  expect(container.querySelector(STATUS)!.textContent).toBe("Listing the agents");
  const chip = container.querySelector(".crewlet-run-chip")!;
  expect(chip.querySelector("[aria-expanded]")!.getAttribute("aria-expanded")).toBe("false");
});

test("stands after a card a read built, rather than going", () => {
  const { container } = draw({ ...turn, cards: [card] });
  expect(screen.getByText("Settle-up algorithm")).toBeTruthy();
  const indicator = container.querySelector(INDICATOR)!;
  const drawn = screen.getByText("Settle-up algorithm");
  expect(drawn.compareDocumentPosition(indicator) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

// THE CARET IS GONE: it stood on a line of its own after the last block, and
// alone above the cards of a silent round. The mark stands after the words.
test("draws the mark alone after a streaming answer, and no caret in it", () => {
  const { container } = draw({
    ...turn,
    phase: "working",
    replies: [
      { id: "m0", text: "" },
      { id: "m", text: "Both of your agents are idle.\n\n- pm\n- engineer" },
    ],
  });
  expect(container.querySelector(CARET)).toBeNull();
  expect(container.querySelector(INDICATOR)).not.toBeNull();
  expect(container.querySelector(STATUS)).toBeNull();
  const words = screen.getByText(/Both of your agents are idle/);
  expect(
    words.compareDocumentPosition(container.querySelector(INDICATOR)!) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
});

test("goes once the turn waits on the person or has ended", () => {
  for (const phase of ["awaiting_approval", "completed", "stopped"] as const) {
    const { container, unmount } = draw({ ...turn, phase });
    expect(container.querySelector(INDICATOR), phase).toBeNull();
    unmount();
  }
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
  draw(timeout, "Ask Crewlet's model is signed out");
  expect(screen.getByRole("button", { name: /Try again/ }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});

test("a turn stopped before it wrote a word still says how it ended", () => {
  draw({ ...turn, phase: "stopped", replies: [] });
  expect(screen.getByText("Stopped. Nothing changed.")).toBeTruthy();
});

test("a turn stopped after an empty round says how it ended, the empty prose hidden", () => {
  const { container } = draw({ ...turn, phase: "stopped", replies: [{ id: "m", text: "" }] });
  expect(screen.getByText("Stopped. Nothing changed.")).toBeTruthy();
  expect(container.querySelectorAll(".crewlet-prose").length).toBe(1);
});

test("a turn that failed before it wrote a word shows its sentence", () => {
  draw({
    ...turn,
    phase: "failed",
    error: {
      code: "model_timeout",
      message: "The assistant's model took too long to answer.",
      retryable: true,
    },
  });
  expect(screen.getByText("The assistant's model took too long to answer.")).toBeTruthy();
});

test("a sent question shows the images it carried as chips under its bubble", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(new Uint8Array([137, 80, 78, 71]))),
  );
  vi.stubGlobal(
    "URL",
    Object.assign(URL, { createObjectURL: () => "blob:sent/1", revokeObjectURL: () => {} }),
  );
  const { container } = draw({
    ...turn,
    phase: "completed",
    attachments: [
      {
        file_id: "f1",
        name: "screenshot.png",
        media_type: "image/png",
        size: 4,
        download: "/assistant/conversations/c1/files/f1",
      },
    ],
  });
  const bubble = container.querySelector(".crewlet-message__attachment")!;
  expect(bubble.textContent).toContain("screenshot.png");
  expect(bubble.querySelector("button")).toBeNull();
  await waitFor(() => expect(bubble.querySelector("img")?.getAttribute("src")).toBe("blob:sent/1"));
  vi.unstubAllGlobals();
});

test("an answer works out its reads and mentions once while its words and cards are the same", () => {
  const keyed: AssistantCardEvent = {
    card_id: "k",
    message_id: "m",
    card: {
      type: "entity",
      entity: {
        kind: "task",
        id: "FAIR-1",
        key: "FAIR-1",
        title: "Settle-up algorithm",
        open: { label: "FAIR-1", screen: "item", href: "#/worklet/FAIR-1" },
      },
    },
  } as AssistantCardEvent;
  const answered: TurnView = {
    ...turn,
    phase: "working",
    replies: [{ id: "m", text: "FAIR-1 is next." }],
    cards: [keyed],
  };
  const view = { ...EMPTY, id: "c1", turns: [answered] };
  // THE DRAWER BUILDS ITS HANDLERS INLINE: a fresh object every render.
  const at = (now: string, handlers: CardHandlers) => (
    <Router>
      <Thread
        view={view}
        now={Date.parse(now)}
        on={{ ...handlers }}
        opened={new Set()}
        onRetry={() => {}}
        retryDisabledReason={undefined}
        onReconnect={() => {}}
      />
    </Router>
  );
  const first = { ...on, openHere: vi.fn() };
  const newest = { ...on, openHere: vi.fn() };
  const { rerender } = render(at("2026-10-24T09:41:02Z", first));
  const reads = vi.mocked(readsOf).mock.calls.length;
  const mentions = vi.mocked(mentionsOf).mock.calls.length;
  // THE CLOCK TICKS AND THE HANDLERS ARE REBUILT, the turn does not change:
  // nothing is worked out again.
  rerender(at("2026-10-24T09:41:03Z", first));
  rerender(at("2026-10-24T09:41:04Z", newest));
  expect(vi.mocked(readsOf).mock.calls.length).toBe(reads);
  expect(vi.mocked(mentionsOf).mock.calls.length).toBe(mentions);
  // AND A CHIP RUNS THE NEWEST HANDLER, not the one it was drawn with.
  fireEvent.click(screen.getByRole("button", { name: /FAIR-1/ }));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("link", { name: "Open FAIR-1" }), {
    button: 0,
  });
  expect(newest.openHere).toHaveBeenCalledOnce();
  expect(first.openHere).not.toHaveBeenCalled();
});
