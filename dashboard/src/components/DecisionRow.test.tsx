/**
 * A held call in a decision list: a run parked on a call to a tool at Needs
 * approval is answered through the approvals route, never the run's own
 * answer, and says what the answer did.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { McpApprovalAnswer } from "~/contract/mcp.ts";
import type { ExecutorRow } from "~/protocol/index.ts";
import { RestError } from "~/protocol/rest.ts";
import { TestClient } from "~/test/client.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { DecisionRow, heldCallOf } from "./DecisionRow.tsx";
import { AnswerRunButton } from "./writes.tsx";

const answerHeldCall = vi.fn<(id: string, answer: string) => Promise<McpApprovalAnswer>>();
vi.mock("~/lib/heldCall.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/heldCall.ts")>()),
  answerHeldCall: (id: string, answer: string) => answerHeldCall(id, answer),
}));

const NOW = Date.parse("2026-11-01T12:00:00Z");

/** swe's run, parked on a call to the tracker's close_issue. */
function heldRun(over: Partial<ExecutorRow> = {}): ExecutorRow {
  return {
    turn_id: "turn-9",
    launch_id: "launch-1",
    agent_handle: "swe",
    role: "Software Engineer",
    status: "awaiting_clarification",
    harness: "claude-code",
    placement: "direct",
    question: "Call tr_close_issue on tracker?",
    question_context: "",
    decision: {
      question: "Call tr_close_issue on tracker?",
      role: "approver",
      options: [
        { id: "once", label: "Allow once" },
        { id: "always", label: "Always allow" },
        { id: "deny", label: "Deny" },
      ],
    },
    audience: "lead",
    audience_handles: ["cto"],
    branch: "",
    trace_id: "",
    owner: "",
    box_exists: true,
    paused_at: "2026-11-01T11:50:00Z",
    pause_ttl_seconds: 1800,
    started_at: "2026-11-01T11:30:00Z",
    updated_at: "2026-11-01T11:50:00Z",
    answerable_in_chat: false,
    work_item: { backend: "worklet", id: "w-1", key: "ENG-7", project: "ENG" },
    held_call: {
      id: "0190f3a8-0000-7000-8000-000000000001",
      server: "tracker",
      tool: "tr_close_issue",
      raw: "close_issue",
      digest: "sha256:d1",
      held_at: "2026-11-01T11:50:00Z",
    },
    held_arguments: { issue: 42 },
    ...over,
  } as ExecutorRow;
}

const sent = (answer: string, outcome: "pending" | "unknown" = "pending"): McpApprovalAnswer => ({
  id: "0190f3a8-0000-7000-8000-000000000001",
  server: "tracker",
  tool: "tr_close_issue",
  seat: "swe",
  turn_id: "turn-9",
  launch_id: "launch-1",
  answer,
  outcome,
  answer_id: outcome === "pending" ? "a-1" : "",
  revision: null,
});

/** The row's answered line, where the answer says what it did; null while it waits. */
const answeredLine = () => document.querySelector(".decision-answered");

function mount(run: ExecutorRow) {
  render(
    <TestClient>
      <ToastProvider>
        <LayerHost>
          <ul className="decision-list">
            <DecisionRow subject={{ kind: "run", at: run.paused_at, run }} now={NOW} />
          </ul>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

beforeEach(() => {
  answerHeldCall.mockReset();
});

afterEach(() => cleanup());

test("a run parked on a held call is drawn as the call, not as a question to answer", () => {
  mount(heldRun());
  expect(screen.getByText("tr_close_issue")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Allow once" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Always allow" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
  // The run's own Answer is refused by the engine for a held call, so it is not offered.
  expect(screen.queryByRole("button", { name: "Answer" })).toBeNull();
  expect(screen.getByText("ENG-7")).toBeTruthy();
});

test("each answer goes to the approvals route by the held call's id, and says what it did", async () => {
  for (const [label, answer, said] of [
    ["Allow once", "once", "Allowed once."],
    ["Always allow", "always", "Always allowed."],
    ["Deny", "deny", "Denied."],
  ] as const) {
    answerHeldCall.mockResolvedValueOnce(sent(answer));
    mount(heldRun());
    fireEvent.click(screen.getByRole("button", { name: label }));
    await waitFor(() => expect(answeredLine()?.textContent).toContain(said));
    expect(answerHeldCall).toHaveBeenLastCalledWith("0190f3a8-0000-7000-8000-000000000001", answer);
    // Answered, the row keeps its line and offers no second answer.
    expect(screen.queryByRole("button", { name: "Allow once" })).toBeNull();
    cleanup();
  }
});

test("an answer whose delivery is unknown says answering again is safe, and keeps the buttons", async () => {
  answerHeldCall.mockResolvedValueOnce(sent("once", "unknown"));
  mount(heldRun());
  fireEvent.click(screen.getByRole("button", { name: "Allow once" }));
  await waitFor(() => expect(answeredLine()?.textContent).toContain("Answering again is safe"));
  expect(screen.getByRole("button", { name: "Allow once" })).toBeTruthy();
});

test("a refused answer says the engine's words and answers nothing", async () => {
  answerHeldCall.mockRejectedValueOnce(
    new RestError(409, {
      error: "stale_version",
      detail: "the call to tr_close_issue is not waiting for an answer any more",
    }),
  );
  mount(heldRun());
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(screen.getByText("The answer was not taken")).toBeTruthy());
  expect(answeredLine()).toBeNull();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("what the call would send is shown only where the row carries it", () => {
  mount(heldRun({ held_arguments: undefined }));
  expect(screen.queryByText("Arguments")).toBeNull();
  cleanup();
  mount(heldRun());
  const row = screen.getByRole("listitem");
  expect(within(row).getByText("Arguments")).toBeTruthy();
});

test("a run parked on a plain question is no held call", () => {
  expect(heldCallOf(heldRun({ held_call: undefined }))).toBeNull();
});

// EVERY OTHER PLACE A PARKED RUN IS ANSWERED (the live run's banner, a seat's
// page) answers a held call the same way: the run's own Answer is refused for
// one. Mutation: drop the held call's branch in AnswerRunButton, and this is red.
test("a parked run's answer button answers a held call through the approvals route", async () => {
  answerHeldCall.mockResolvedValueOnce(sent("deny"));
  render(
    <TestClient>
      <ViewerProvider>
        <ToastProvider>
          <LayerHost>
            <AnswerRunButton run={heldRun()} seat="Software Engineer" />
          </LayerHost>
        </ToastProvider>
      </ViewerProvider>
    </TestClient>,
  );
  expect(screen.queryByRole("button", { name: "Answer" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(answerHeldCall).toHaveBeenCalledWith(heldRun().held_call!.id, "deny"));
});
