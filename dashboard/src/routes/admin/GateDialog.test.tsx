/**
 * The evict and readmit dialog: one gesture over every identity-claiming log,
 * answered per log, finished under the operation id it was minted with.
 *
 * EVERY ANSWER HERE IS THE ENGINE'S OWN RENDERING, read from the golden its Go
 * test regenerates (`internal/api/testdata/gate_answer.json`). The suite this
 * replaced typed its own fixtures — a top-level `outcome`, an op id of `op-7`
 * no node mints — and passed for as long as the route had stopped writing that
 * shape, while the dialog rendered every gesture, one applied on both logs
 * included, as "no acknowledgement, this is the case to retry".
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { engineFile } from "~/test/engineFiles.ts";
import type { RetentionGateResult } from "~/protocol/index.ts";
import { finishable, GateDialog, GateOutcome } from "./GateDialog.tsx";
import type { GateGesture } from "./GateDialog.tsx";

interface Golden {
  answers: Record<string, RetentionGateResult>;
  refusals: Record<string, { status: number; body: Record<string, unknown> }>;
}

const golden = engineFile<Golden>("internal/api/testdata/gate_answer.json");
const answer = (name: string): RetentionGateResult => {
  const a = golden.answers[name];
  if (!a) throw new Error(`the golden has no answer named ${name}`);
  return structuredClone(a);
};

/** The engine's grammar, as the route checks an op id a caller brings. */
const OP_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.[!-~]+$/;

/** One request the dialog sent. */
interface Sent {
  path: string;
  query: URLSearchParams;
}

/** A fetch that answers each request with the next of `replies`. */
function engine(...replies: { status: number; body: unknown }[]): Sent[] {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://engine.test");
      sent.push({ path: url.pathname, query: url.searchParams });
      const reply = replies[Math.min(sent.length - 1, replies.length - 1)]!;
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  localStorage.clear();
});

/** Types the node id and presses the gesture's own button. */
function confirmAndPress(node: string, verb: "Evict" | "Readmit") {
  fireEvent.change(screen.getByLabelText(`Type ${node} to confirm`), { target: { value: node } });
  fireEvent.click(screen.getByRole("button", { name: verb }));
}

// --- the outcome, per log -------------------------------------------------

// APPLIED ON EVERY LOG IS THE CONFIRMATION — and it is what the old dialog
// rendered as "no acknowledgement", sending the operator to press again.
test("a gesture applied on every log renders as the confirmation", () => {
  render(<GateOutcome result={answer("applied")} evict />);
  expect(screen.getByRole("status").className).toContain("success");
  expect(screen.getByText(/evicted on every log/)).toBeTruthy();
  expect(screen.queryByText(/No acknowledgement/)).toBeNull();
  expect(screen.getByText(/CREWLET_PAGES_LOG 4410/)).toBeTruthy();
});

// `pending` IS DURABLE AND UNRESOLVED: not success, not a failure, and never
// something to retry, because a fresh gesture appends a second record.
test("a gesture pending on a log is not rendered as success and says not to retry", () => {
  render(<GateOutcome result={answer("pending")} evict />);
  const banner = screen.getByRole("status");
  expect(banner.className).not.toContain("success");
  expect(banner.className).toContain("warning");
  expect(screen.getByText(/Do not retry/)).toBeTruthy();
  expect(screen.getByText(/not yet applied here/)).toBeTruthy();
});

// AN UNKNOWN LOG HAS NO POSITION — never "sequence 0", which reads as a record
// at the log's origin — and it is the one a finish under the same id is for.
test("an unknown log shows no position and offers finishing under the gesture's id", () => {
  const result = answer("unknown");
  render(<GateOutcome result={result} evict />);
  expect(screen.getByRole("alert").className).toContain("danger");
  expect(screen.getByText(/Finish it under the same operation id/)).toBeTruthy();
  expect(screen.getByText(result.op_id)).toBeTruthy();
  expect(screen.getByText(/may or may not be on the log/)).toBeTruthy();
  expect(screen.queryByText(/sequence 0/)).toBeNull();
  expect(screen.queryByText(/CREWLET_PAGES_LOG 0/)).toBeNull();
  expect(finishable({ opId: result.op_id, force: false, answer: result })).toBe(true);
});

// A FULL LOG IS NOT TOLD TO RETRY — the same request is refused the same way
// until its ceiling moves — and it shows the engine's own sentence. But its
// gesture is still finished under its OWN id once there is room, so it is kept.
test("a full log is not offered a retry, shows its hint, and keeps the gesture", () => {
  const result = answer("log_full");
  render(<GateOutcome result={result} evict />);
  expect(screen.queryByText(/Finish it under the same operation id: a log/)).toBeNull();
  expect(screen.queryByText(/Finish this gesture sends it again/)).toBeNull();
  expect(screen.getByText(result.domains[1]!.hint!)).toBeTruthy();
  expect(screen.getByText(/sending it again now cannot finish it/)).toBeTruthy();
  expect(screen.getByText(/crewlet retention set-capacity/)).toBeTruthy();
  expect(finishable({ opId: result.op_id, force: false, answer: result })).toBe(true);
});

// A SUPERSEDED OPERATION CAN NEVER BE FINISHED, so nothing offers to.
test("a superseded operation offers no finish and says it needs another remedy", () => {
  const result = answer("superseded");
  render(<GateOutcome result={result} evict />);
  expect(screen.getByText(/this operation cannot be/)).toBeTruthy();
  expect(finishable({ opId: result.op_id, force: false, answer: result })).toBe(false);
});

// --- the request: the id, the finish, the force, the deadline -------------

// THE TYPED CONFIRMATION, AND AN ID IN THE ENGINE'S GRAMMAR, TRAVEL — minted
// in the browser before the first request, so a request that never answers
// still leaves a handle on the gesture. And no force nobody asked for.
test("the gesture carries the confirmation and an op id the route accepts", async () => {
  const sent = engine({ status: 200, body: answer("applied") });
  render(<GateDialog node="node-2" evict onHeld={() => {}} onClose={() => {}} />);
  confirmAndPress("node-2", "Evict");
  await waitFor(() => expect(sent.length).toBe(1));

  expect(sent[0]!.path).toBe("/work/retention/evict/node-2");
  expect(sent[0]!.query.get("confirm")).toBe("node-2");
  const opId = sent[0]!.query.get("op_id") ?? "";
  expect(opId).toMatch(OP_ID);
  expect(opId.endsWith(".evict-node-2")).toBe(true);
  // THE MINT INSTANT IS NOW, which the ledger reads.
  const minted = parseInt(opId.slice(0, 8) + opId.slice(9, 13), 16);
  expect(Math.abs(minted - Date.now())).toBeLessThan(60_000);
  expect(sent[0]!.query.has("force")).toBe(false);
});

// FINISH IS THE SAME REQUEST UNDER THE SAME ID. A second press that minted a
// fresh one was a second gesture over the logs the first one reached.
test("finishing a partial gesture re-sends the same op id", async () => {
  const held: (GateGesture | null)[] = [];
  const sent = engine(
    { status: 200, body: answer("unknown") },
    { status: 200, body: answer("applied") },
  );
  render(<GateDialog node="node-4" evict onHeld={(g) => held.push(g)} onClose={() => {}} />);
  confirmAndPress("node-4", "Evict");
  await waitFor(() => expect(screen.getByRole("button", { name: "Finish this gesture" })));
  // THE SCREEN HOLDS IT, so a close does not lose it.
  expect(held.at(-1)?.opId).toBe(sent[0]!.query.get("op_id"));

  fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
  await waitFor(() => expect(sent.length).toBe(2));
  expect(sent[1]!.query.get("op_id")).toBe(sent[0]!.query.get("op_id"));
  await waitFor(() => expect(screen.getByText(/evicted on every log/)).toBeTruthy());
  // AND ONCE EVERY LOG HOLDS IT THERE IS NOTHING LEFT TO FINISH — but it is
  // still HELD, complete, until the screen's report shows it: let go of now,
  // the row offered "Evict…" again and a reopen minted a second gesture.
  expect(held.at(-1)?.answer?.complete).toBe(true);
  expect(held.at(-1)?.opId).toBe(sent[0]!.query.get("op_id"));
  expect(finishable(held.at(-1)!)).toBe(false);
  expect(screen.queryByRole("button", { name: "Finish this gesture" })).toBeNull();
});

// AN OPERATION NO REQUEST CAN FINISH IS LET GO OF, so the next gesture on that
// node is rightly a new one.
test("an operation no request can finish is not held", async () => {
  const held: (GateGesture | null)[] = [];
  engine({ status: 200, body: answer("superseded") });
  render(<GateDialog node="node-4" evict onHeld={(g) => held.push(g)} onClose={() => {}} />);
  confirmAndPress("node-4", "Evict");
  await waitFor(() => expect(held.length).toBe(1));
  expect(held[0]).toBeNull();
});

// A LIVE NODE'S REFUSAL OFFERS FORCE — as a control, since this screen has no
// -force — behind its own tick and a second typed confirmation; and a Finish of
// a forced gesture stays forced, or it is judged again against the lease the
// operator overrode.
test("a refused eviction offers force, and a forced gesture's finish carries it", async () => {
  const refusal = golden.refusals["eviction_refused"]!;
  const sent = engine(
    refusal,
    { status: 200, body: answer("unknown") },
    { status: 200, body: answer("applied") },
  );
  render(<GateDialog node="node-4" evict onHeld={() => {}} onClose={() => {}} />);
  confirmAndPress("node-4", "Evict");
  await waitFor(() => expect(screen.getByText(/may not be evicted/)).toBeTruthy());
  // THE ENGINE'S SENTENCE, WHICH NAMES NO FLAG.
  expect(screen.getByText(String(refusal.body.hint))).toBeTruthy();
  expect(screen.queryByText(/-force/)).toBeNull();

  fireEvent.click(screen.getByRole("checkbox", { name: "Force the eviction" }));
  const force = screen.getByRole("button", { name: "Force eviction" });
  expect((force as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(screen.getByLabelText("Type node-4 again to force it"), {
    target: { value: "node-4" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Force eviction" }));
  await waitFor(() => expect(sent.length).toBe(2));
  expect(sent[1]!.query.get("force")).toBe("true");
  // A REFUSAL WROTE NOTHING, so the gesture keeps its id.
  expect(sent[1]!.query.get("op_id")).toBe(sent[0]!.query.get("op_id"));

  await waitFor(() => screen.getByRole("button", { name: "Finish this gesture" }));
  fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
  await waitFor(() => expect(sent.length).toBe(3));
  expect(sent[2]!.query.get("force")).toBe("true");
  expect(sent[2]!.query.get("op_id")).toBe(sent[0]!.query.get("op_id"));
});

// AN UNJUDGED EVICTION — coordination unreachable, exactly when an absent node
// most needs evicting — offers force too.
test("an eviction nobody could judge offers force", async () => {
  engine(golden.refusals["eviction_unjudged"]!);
  render(<GateDialog node="node-4" evict onHeld={() => {}} onClose={() => {}} />);
  confirmAndPress("node-4", "Evict");
  await waitFor(() => expect(screen.getByText(/cannot be judged/)).toBeTruthy());
  expect(screen.getByRole("checkbox", { name: "Force the eviction" })).toBeTruthy();
});

// A REFUSED READMISSION SAYS WHAT TO DO, NOT ONLY WHY, offers no force — a
// readmission has nothing to force — and renders no outcome: nothing was
// written.
test("a refused readmission renders its reason and its remedy, and no outcome", async () => {
  const refusal = golden.refusals["readmission_refused"]!;
  engine(refusal);
  render(<GateDialog node="node-4" evict={false} onHeld={() => {}} onClose={() => {}} />);
  confirmAndPress("node-4", "Readmit");
  await waitFor(() => expect(screen.getByText(/may not be readmitted/)).toBeTruthy());
  expect(screen.getByText(String(refusal.body.hint))).toBeTruthy();
  expect(screen.getByText(/Wait for it to catch up/)).toBeTruthy();
  expect(screen.queryByRole("checkbox")).toBeNull();
  expect(screen.queryByText(/is readmitted/)).toBeNull();
});

// A REQUEST NOBODY ANSWERED KEEPS ITS ID. The engine allows a gesture a minute
// and finishes it whatever the connection does; the dialog gave up at thirty
// seconds holding nothing, and the only way on was a second gesture.
test("a gesture that times out keeps its op id and offers to finish it", async () => {
  vi.useFakeTimers();
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      sent.push({ path: url.pathname, query: url.searchParams });
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("aborted", "AbortError")),
        );
      });
    }),
  );
  const held: (GateGesture | null)[] = [];
  render(<GateDialog node="node-4" evict onHeld={(g) => held.push(g)} onClose={() => {}} />);
  confirmAndPress("node-4", "Evict");

  // NOT GIVEN UP AT THE DEFAULT THIRTY SECONDS.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(31_000);
  });
  expect(screen.queryByText(/No answer/)).toBeNull();

  await act(async () => {
    await vi.advanceTimersByTimeAsync(45_000);
  });
  expect(screen.getByText(/No answer/)).toBeTruthy();
  const opId = sent[0]!.query.get("op_id")!;
  expect(screen.getByText(opId)).toBeTruthy();
  expect(held.at(-1)?.opId).toBe(opId);

  fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
  expect(sent.length).toBe(2);
  expect(sent[1]!.query.get("op_id")).toBe(opId);
});

// AN ANSWER THE ENGINE DID NOT WRITE IS NOT A REFUSAL. A reverse proxy's read
// timeout is a minute by default — the node's own budget for a gesture past its
// judgement — so a slow eviction reached the browser as a gateway's 504 with an
// HTML page, and a 200 can be cut off part way through. Read as a refusal, the
// dialog never held the gesture, offered no Finish, and closing it lost the id
// of a gesture the node went on to finish.
test("a gateway's answer or a cut-off one keeps the op id and offers to finish it", async () => {
  for (const reply of [
    () =>
      new Response("<html><body><h1>504 Gateway Time-out</h1></body></html>", {
        status: 504,
        headers: { "Content-Type": "text/html" },
      }),
    () => new Response(JSON.stringify({ message: "upstream closed" }), { status: 502 }),
    () =>
      new Response('{"node":"node-4","op_id":"01a0c4', {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
  ]) {
    const sent: Sent[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(String(input), "http://engine.test");
        sent.push({ path: url.pathname, query: url.searchParams });
        return sent.length === 1
          ? reply()
          : new Response(JSON.stringify(answer("applied")), {
              status: 200,
              headers: { "Content-Type": "application/json" },
            });
      }),
    );
    const held: (GateGesture | null)[] = [];
    render(<GateDialog node="node-4" evict onHeld={(g) => held.push(g)} onClose={() => {}} />);
    confirmAndPress("node-4", "Evict");
    await waitFor(() => expect(screen.getByText(/No answer/)).toBeTruthy());
    const opId = sent[0]!.query.get("op_id")!;
    expect(held.at(-1)?.opId).toBe(opId);
    expect(held.at(-1)?.unanswered).toBeTruthy();
    // NEVER THE REFUSAL'S FORM: nothing to type again, and no fresh id.
    expect(screen.queryByLabelText("Type node-4 to confirm")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
    await waitFor(() => expect(sent.length).toBe(2));
    expect(sent[1]!.query.get("op_id")).toBe(opId);
    await waitFor(() => expect(screen.getByText(/evicted on every log/)).toBeTruthy());
    cleanup();
    vi.unstubAllGlobals();
  }
});

// A REFUSED FINISH KEEPS WHAT THE GESTURE ALREADY HEARD. Finish re-runs the
// judgement, so it can come back `503 eviction_unjudged` — and a refusal wrote
// nothing THIS TIME, which says nothing about the logs the first request
// reached. The dialog dropped the per-log answer and the operation id and fell
// back to "Type node-4 to confirm", as though no gesture had been made.
test("a refused finish renders beside the answer it already had", async () => {
  const result = answer("unknown");
  const sent = engine(golden.refusals["eviction_unjudged"]!, {
    status: 200,
    body: answer("applied"),
  });
  const held: (GateGesture | null)[] = [];
  render(
    <GateDialog
      node="node-4"
      evict
      held={{ opId: result.op_id, force: false, answer: result }}
      onHeld={(g) => held.push(g)}
      onClose={() => {}}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
  await waitFor(() => expect(screen.getByText(/cannot be judged/)).toBeTruthy());
  expect(screen.getByText(/Finishing it was refused, and wrote nothing/)).toBeTruthy();
  // THE ANSWER IS STILL THERE: its operation, and Finish under it.
  expect(screen.getByText(result.op_id)).toBeTruthy();
  expect(screen.queryByLabelText("Type node-4 to confirm")).toBeNull();
  expect(screen.getByRole("button", { name: "Finish this gesture" })).toBeTruthy();
  // AND THE SCREEN STILL HOLDS IT: a refusal is not a new state of the gesture.
  expect(held).toEqual([]);

  // THE REFUSAL'S OWN REMEDY IS OFFERED BESIDE IT, force carried under the
  // gesture's own id.
  fireEvent.click(screen.getByRole("checkbox", { name: "Force the eviction" }));
  fireEvent.change(screen.getByLabelText("Type node-4 again to force it"), {
    target: { value: "node-4" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Force eviction" }));
  await waitFor(() => expect(sent.length).toBe(2));
  expect(sent[1]!.query.get("op_id")).toBe(result.op_id);
  expect(sent[1]!.query.get("force")).toBe("true");
});

// A DIALOG REOPENED ON A HELD GESTURE OFFERS TO FINISH IT, with nothing to type
// and nothing freshly minted: the gesture was confirmed when it started.
test("a dialog reopened on an unfinished gesture offers finish under its id", async () => {
  const result = answer("unknown");
  const sent = engine({ status: 200, body: answer("applied") });
  render(
    <GateDialog
      node="node-4"
      evict
      held={{ opId: result.op_id, force: false, answer: result }}
      onHeld={() => {}}
      onClose={() => {}}
    />,
  );
  expect(screen.queryByRole("textbox")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Finish this gesture" }));
  await waitFor(() => expect(sent.length).toBe(1));
  expect(sent[0]!.query.get("op_id")).toBe(result.op_id);
});

// A GESTURE STARTED ELSEWHERE IS FINISHED HERE UNDER ITS OWN ID: a node that is
// itself evicted cannot write, and its gesture is finished through another.
test("a pasted operation id is the one the gesture is sent under", async () => {
  const earlier = answer("unknown").op_id;
  const sent = engine({ status: 200, body: answer("applied") });
  render(<GateDialog node="node-4" evict onHeld={() => {}} onClose={() => {}} />);
  fireEvent.change(screen.getByLabelText("Operation id of the earlier gesture"), {
    target: { value: earlier },
  });
  fireEvent.change(screen.getByLabelText("Type node-4 to confirm"), {
    target: { value: "node-4" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Evict" }));
  await waitFor(() => expect(sent.length).toBe(1));
  expect(sent[0]!.query.get("op_id")).toBe(earlier);
});
