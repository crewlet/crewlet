/**
 * A CLOSED TOOL ROW COSTS NOTHING.
 *
 * `lazy` defers a disclosure's CHILDREN, not the body of the component that
 * renders it — so formatting placed in the row itself ran for every call on a
 * phase's first render however many were closed, and a forty-round phase paid
 * the whole parse before the reader opened anything. The work lives in a child
 * mounted inside the disclosure now, and this is what says so: the formatter
 * is not called until a row is opened.
 *
 * Asserted through a spy rather than through the DOM, because the DOM cannot
 * tell the difference — `lazy` keeps the markup out either way, and the cost
 * this protects against is the parse nobody could see.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

const indentJSON = vi.fn((text: string) => text);
vi.mock("~/lib/jsontext.ts", () => ({ indentJSON: (t: string) => indentJSON(t) }));

// IMPORTED AFTER THE MOCK, because `vi.mock` is hoisted above static imports
// but the spy it closes over is not — a static import of the card would bind
// the real module before the factory ever ran.
const { PhaseCard } = await import("./PhaseCard.tsx");

afterEach(() => {
  cleanup();
  indentJSON.mockClear();
});

function record() {
  return {
    key: "turn-1|execute|1",
    turnId: "turn-1",
    workKey: "wk-1",
    phase: "execute",
    iteration: 1,
    role: "Support Engineer",
    model: "scripted",
    providerKey: "",
    live: false,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    response: "",
    tools: [
      {
        name: "read_file",
        round: 1,
        args: '{"path":"a.go"}',
        result: '{"ok":true}',
        failed: false,
        durationMs: 0,
        origin: "builtin",
        server: "",
      },
    ],
    narration: [{ round: 1, reasoning: "", content: "Working." }],
    partial: null,
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    roundsUsed: 1,
    exhaustedRounds: false,
    emptyAnswerRounds: 0,
    rescueFired: false,
    decision: "",
    notes: "",
    conversationKey: "",
    toolsAvailable: [],
    toolCatalogue: [],
    worker: "",
    taskId: "",
    hostPhase: "",
    hostIteration: 0,
    backend: "",
    codingAgent: "",
    sandboxId: "",
    costUSD: 0,
    deliveredRefs: [],
    trigger: null,
    at: "2026-09-02T10:00:00Z",
    startedAt: "2026-09-02T10:00:00Z",
    durationMs: 0,
    eventId: "ev-1",
  };
}

describe("formatting a tool call's records", () => {
  test("does not happen until the row is opened", () => {
    render(<PhaseCard record={record() as never} defaultOpen />);
    expect(indentJSON).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /^read_file/ }));
    // Both records, once the reader asks for them.
    expect(indentJSON.mock.calls.map(([t]) => t)).toEqual(['{"path":"a.go"}', '{"ok":true}']);
  });
});
