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
import type { ReactNode } from "react";
import { ClientContext } from "~/lib/store-hooks.ts";
import { Store } from "~/protocol/index.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { DecisionRow, HeldCallRow, heldCallOf } from "./DecisionRow.tsx";
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

/**
 * A connected client read as an admin the engine lets answer a run: who may
 * answer a held call is the seat's rule (`useAnswerHold`), so a reader nobody
 * has said anything about yet is shown its answers held.
 */
function Answering({ children }: { children: ReactNode }) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  const socket = {
    query: async (what: string) =>
      what === "viewer" ? { ...ADMIN_VIEWER, acts: ["answer_executor"] } : {},
    focus: () => {},
  };
  return (
    <ClientContext.Provider value={{ store, socket } as never}>
      <ViewerProvider>{children}</ViewerProvider>
    </ClientContext.Provider>
  );
}

async function mount(run: ExecutorRow) {
  render(
    <Answering>
      <ToastProvider>
        <LayerHost>
          <ul className="decision-list">
            <DecisionRow subject={{ kind: "run", at: run.paused_at, run }} now={NOW} />
          </ul>
        </LayerHost>
      </ToastProvider>
    </Answering>,
  );
  await afterMicrotasks();
}

beforeEach(() => {
  answerHeldCall.mockReset();
});

afterEach(() => cleanup());

test("a run parked on a held call is drawn as the call, not as a question to answer", async () => {
  await mount(heldRun());
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
    await mount(heldRun());
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
  await mount(heldRun());
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
  await mount(heldRun());
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(screen.getByText("The answer was not taken")).toBeTruthy());
  expect(answeredLine()).toBeNull();
  expect(screen.getByRole("button", { name: "Deny" })).toBeTruthy();
});

test("what the call would send is shown only where the row carries it", async () => {
  await mount(heldRun({ held_arguments: undefined }));
  expect(screen.queryByText("Arguments")).toBeNull();
  cleanup();
  await mount(heldRun());
  const row = screen.getByRole("listitem");
  expect(within(row).getByText("Arguments")).toBeTruthy();
});

// ONE BUTTON LANGUAGE ON EVERY DECISION ROW: the first answer filled, every
// other outlined, all at the small size, whether the row is a held call or a
// question. Mutation: draw the parked run's Answer outlined and this is red.
test("every decision row draws a filled first answer and outlined others", async () => {
  const variants = () =>
    [...document.querySelectorAll<HTMLElement>(".decision-actions button")].map((b) => [
      b.className.match(/crewlet-btn--(primary|secondary|ghost|outline)/)?.[1],
      b.className.includes("crewlet-btn--small"),
    ]);
  await mount(heldRun());
  expect(variants()).toEqual([
    ["primary", true],
    ["secondary", true],
    ["secondary", true],
  ]);
  cleanup();
  await mount(heldRun({ held_call: undefined, question: "Which region?" }));
  expect(variants()).toEqual([["primary", true]]);
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
    <Answering>
      <ToastProvider>
        <LayerHost>
          <AnswerRunButton run={heldRun()} seat="Software Engineer" />
        </LayerHost>
      </ToastProvider>
    </Answering>,
  );
  await afterMicrotasks();
  expect(screen.queryByRole("button", { name: "Answer" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Deny" }));
  await waitFor(() => expect(answerHeldCall).toHaveBeenCalledWith(heldRun().held_call!.id, "deny"));
});

// A HELD CALL SAYS THE DECISION IT CARRIES OUT, so its approver reads the call
// as part of a plan a person already chose rather than a new question; a call
// no answer woke says nothing of the kind.
test("a held call names the decision it carries out", async () => {
  const call = {
    id: "h-1",
    seat: "swe",
    server: "tracker",
    tool: "tr_close_issue",
    at: "2026-11-01T11:50:00Z",
    item: null,
    args: undefined,
    acting: {
      key: "ENG-9",
      question: "Ship the migration?",
      choice: "Ship it",
      answered_by: "jane",
    },
  };
  render(
    <Answering>
      <ToastProvider>
        <LayerHost>
          <ul className="decision-list">
            <HeldCallRow call={call} now={NOW} />
          </ul>
        </LayerHost>
      </ToastProvider>
    </Answering>,
  );
  await afterMicrotasks();
  expect(
    screen.getByText(
      "It carries out a decision on ENG-9: jane answered \u201cShip the migration?\u201d with \u201cShip it\u201d.",
    ),
  ).toBeTruthy();
  cleanup();
  render(
    <Answering>
      <ToastProvider>
        <LayerHost>
          <ul className="decision-list">
            <HeldCallRow call={{ ...call, acting: null }} now={NOW} />
          </ul>
        </LayerHost>
      </ToastProvider>
    </Answering>,
  );
  await afterMicrotasks();
  expect(screen.queryByText(/It carries out a decision/)).toBeNull();
});

/** An agent's SSH command its delegate escalated (ADR-0125), with what it found. */
function escalatedRun(): ExecutorRow {
  return heldRun({
    agent_handle: "sre",
    question: "Run `ufw allow 22` on web-1?",
    held_call: {
      id: "0190f3a8-0000-7000-8000-000000000002",
      server: "crewlet",
      tool: "ssh_operate",
      raw: "ssh_operate",
      digest: "sha256:d2",
      held_at: "2026-11-01T11:50:00Z",
      fixed: true,
      approvers: "ssh",
      act: "ssh_operate",
      delegation: {
        rule: 1,
        delegates: ["security-engineer"],
        given: [
          {
            by: "security-engineer",
            verdict: "escalate",
            confidence: "high",
            reason: "Opens the firewall, which the item does not ask for.",
            findings: ["Changes firewall rules", "Not one of the item's servers"],
            at: "2026-11-01T11:52:00Z",
          },
        ],
        escalated: {
          overruled: ["firewall"],
          engine: ["The command changes firewall rules, which no delegate allows."],
          at: "2026-11-01T11:52:00Z",
        },
      },
    },
    held_arguments: { host: "web-1", command: "ufw allow 22" },
  });
}

// AN ESCALATION READS ITS DELEGATES BEFORE THE CALL: who put it to people and
// under which rule, each finding as given, and the engine's own reason, all
// ahead of what the call would send. Mutation: draw the note after the
// arguments, or drop the findings, and this is red.
test("an escalated call shows its delegate's answer and findings before the call", async () => {
  await mount(escalatedRun());
  const row = document.querySelector<HTMLElement>(".decision-list > li")!;
  const finding = within(row).getByText("Changes firewall rules");
  expect(within(row).getByText("Not one of the item's servers")).toBeTruthy();
  expect(
    within(row).getByText("The command changes firewall rules, which no delegate allows."),
  ).toBeTruthy();
  const args = within(row).getByText("Arguments");
  expect(finding.compareDocumentPosition(args) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  // ITS ANSWERS ARE UNCHANGED: a fixed call is reviewed whole before Allow once.
  expect(within(row).getByRole("button", { name: "Review and allow once" })).toBeTruthy();
});

// WHO ESCALATED, THE RULE AND THE CONFIDENCE ARE SAID ONCE, on one quiet
// callout: a sole escalation has no second line naming its delegate, its
// verdict or its confidence. Mutation: draw the answer's own line for it, or
// the tinted callout, and this is red.
test("a sole escalation is said once, on a quiet callout", async () => {
  await mount(escalatedRun());
  const row = document.querySelector<HTMLElement>(".decision-list > li")!;
  const heading = within(row).getByText("Escalated by security-engineer (rule 1), high confidence");
  expect(heading.closest(".crewlet-callout")?.className).toContain("crewlet-callout--quiet");
  expect(within(row).getAllByText(/security-engineer/)).toHaveLength(1);
  expect(within(row).queryByText("Escalated")).toBeNull();
  expect(within(row).queryByText("High confidence")).toBeNull();
});

// THE DETAIL SPANS THE ROW: the escalation and the arguments are a row of
// their own under the words and the buttons, never a third line of the words,
// which left the column under the buttons empty. Mutation: draw the detail
// inside `.decision-text` and this is red.
test("a held call's escalation and arguments span the row under its words and buttons", async () => {
  await mount(escalatedRun());
  const item = document.querySelector<HTMLElement>(".decision-list > li .decision-item")!;
  const [text, actions, detail] = [...item.children];
  expect(text?.className).toBe("decision-text");
  expect(actions?.className).toBe("decision-actions");
  expect(detail?.className).toBe("decision-detail");
  expect(within(detail as HTMLElement).getByText(/^Escalated by/)).toBeTruthy();
  expect(within(detail as HTMLElement).getByText("Arguments")).toBeTruthy();
  expect(within(text as HTMLElement).queryByText(/^Escalated by/)).toBeNull();
});

// THE TITLE IS IN PRODUCT WORDS from the call's own arguments, the item on the
// head as an asker's questions say theirs, and the call as it was made stays
// under Arguments. Mutation: title it by its tool and this is red.
test("a held command is titled by what it does, its raw call under Arguments", async () => {
  await mount(escalatedRun());
  const row = document.querySelector<HTMLElement>(".decision-list > li")!;
  expect(row.querySelector(".decision-title")?.textContent).toBe("Run a command on web-1?");
  expect(row.querySelector(".decision-group-head")?.textContent).toContain("on ENG-7");
  expect(row.querySelector(".decision-text")?.textContent).not.toContain("ENG-7");
  const raw = within(row).getByText("Call ssh_operate on crewlet");
  expect(raw.closest(".decision-detail")).toBeTruthy();
});

test("a call put to people at once carries no escalation", async () => {
  await mount(heldRun());
  expect(screen.queryByText(/Escalated by/)).toBeNull();
});

// A HELD MERGE'S ARGUMENTS READ AS THE MERGE, for whoever is sent them: the
// change, the head, the method and the reason, each as text and never Markdown.
// Mutation: draw them through the JSON block, and this is red.
test("a held merge's arguments are drawn as the merge, its reason as text", async () => {
  const reason = "**Ship it** [now](https://evil.example)";
  await mount(
    heldRun({
      agent_handle: "sre",
      question: "May sre merge web!42 at 4f1c2a9b7e3d?",
      held_call: {
        id: "0190f3a8-0000-7000-8000-000000000003",
        server: "crewlet",
        tool: "ask_to_merge",
        raw: "ask_to_merge",
        digest: "sha256:d3",
        held_at: "2026-11-01T11:50:00Z",
        fixed: true,
        approvers: "release",
        act: "pr_merge",
        delegation: null,
      },
      held_arguments: {
        change: "web!42",
        head: "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345",
        method: "merge",
        reason,
      },
    }),
  );
  const row = document.querySelector<HTMLElement>(".decision-list > li")!;
  fireEvent.click(within(row).getByText("Arguments"));
  expect(within(row).getByText("web!42")).toBeTruthy();
  expect(within(row).getByText("4f1c2a9").getAttribute("title")).toBe(
    "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345",
  );
  expect(within(row).getByText("A merge commit")).toBeTruthy();
  expect(within(row).getByText(reason)).toBeTruthy();
  expect(row.querySelector("dl a, dl strong, pre")).toBeNull();
  expect(within(row).getByRole("button", { name: "Review and allow once" })).toBeTruthy();
});
