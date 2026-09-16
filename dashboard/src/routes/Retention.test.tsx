/**
 * The renderings on this panel that are load-bearing, and why each one is.
 *
 * Every case here protects a rendering whose WRONG form reads as a different
 * fact rather than as a missing one. That is the failure mode a screen has and
 * a log does not: an operator acts on what the tile says, and a zero where the
 * answer is "nobody has said" is a lie they will believe.
 *
 * The write-outcome cases are step 16's: `pending` is durable-but-unapplied,
 * and rendering it as success is the browser half of the lie the
 * durable-versus-applied split exists to prevent.
 *
 * WHAT AN ABSENCE LOOKS LIKE IS THE DESIGN SYSTEM'S, so these cases assert its
 * READING rather than its punctuation: `EmptyValue` draws a mark that is
 * hidden from a screen reader and says what the absence means beside it, which
 * is the only half a reader who cannot see the cell ever receives.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Callout } from "@crewlethq/ui";
import { isDrawnAs } from "~/testing.tsx";
import { GateDialog, GateOutcome } from "./GateDialog.tsx";
import { MaintenanceBanner, NodePositions, ServedLevelBanner, Terms } from "./Retention.tsx";
import type { RetentionGateResult, RetentionNode, RetentionTerm } from "~/protocol/index.ts";

afterEach(cleanup);

const node = (over: Partial<RetentionNode> = {}): RetentionNode => ({
  node_id: "node-a",
  counted: true,
  live: true,
  ...over,
});

// A NODE THAT PUBLISHED NOTHING IS NOT A NODE AT ZERO. The two block the trim
// for very different lengths of time, and a 0 in the position column is a
// claim that this node has applied nothing, which is the opposite of "we do
// not know yet".
test("a node with no published position renders a marked absence, never a zero", () => {
  render(<NodePositions node={node()} />);
  // THE READING, not the mark: the mark is hidden from a screen reader on
  // purpose, so this sentence is the whole of what such a reader is told.
  expect(screen.getByText("No position published yet")).toBeTruthy();
  expect(screen.queryByText("0")).toBeNull();
});

// APPLIED_THROUGH BESIDE SEQ. A node applying nothing while its position
// advances looks identical to a caught-up one from either number alone, so a
// deferral rendered as lag is the state nobody diagnoses.
test("a deferral renders as its own state rather than as lag", () => {
  render(
    <NodePositions
      node={node({
        domains: { tracker: { generation: 1, seq: 90, applied_through: 41, deferred: 2 } },
      })}
    />,
  );
  expect(screen.getByText(/applied 41/)).toBeTruthy();
  expect(screen.getByText("2 deferred")).toBeTruthy();

  // THE CONTROL: a caught-up node shows neither, or the assertions above
  // would pass on a panel that always draws them.
  cleanup();
  render(
    <NodePositions
      node={node({ domains: { tracker: { generation: 1, seq: 90, applied_through: 90 } } })}
    />,
  );
  expect(screen.queryByText(/applied/)).toBeNull();
  expect(screen.queryByText(/deferred/)).toBeNull();
});

// AN UNKNOWN LAG IS NOT ZERO LAG. The server sends it absent when the stream
// could not be read, and a screen that renders that as "0 behind" reports a
// node as caught up on evidence nobody has.
test("an unreadable lag renders as unknown rather than as caught up", () => {
  render(
    <NodePositions
      node={node({ domains: { tracker: { generation: 1, seq: 90, applied_through: 90 } } })}
    />,
  );
  // "lag not measured" is the sentence the two halves make together, and the
  // second half is the one that cannot be mistaken for a number.
  expect(screen.getByText("lag")).toBeTruthy();
  expect(screen.getByText("not measured")).toBeTruthy();
  expect(screen.queryByText(/behind/)).toBeNull();
});

const term = (over: Partial<RetentionTerm> = {}): RetentionTerm => ({
  name: "applied",
  state: "ok",
  seq: 41,
  remedy: "nothing to do",
  ...over,
});

// `n/a` RATHER THAN `0`, and `unknown` rather than a number. A term this
// domain does not have, a term nobody could evaluate, and a term permitting
// zero are three different things to do about.
test("a term's third value is rendered rather than collapsed to a number", () => {
  render(
    <Terms
      terms={[
        term(),
        term({ name: "backup", state: "n/a", seq: undefined }),
        term({ name: "snapshot", state: "unknown", seq: undefined }),
      ]}
    />,
  );
  expect(screen.getByText("41")).toBeTruthy();
  expect(screen.getByText("n/a")).toBeTruthy();
  expect(screen.getByText("unknown")).toBeTruthy();
});

// THE SNAPSHOT BLOCK CARRIES ITS OWN REASON. A stalled snapshot tier and a
// stalled trim are different problems with different remedies, and the first
// is silent until a node tries to join.
test("a blocked snapshot loop says so separately from the trim", () => {
  render(<Terms terms={[term()]} snapshotBlocked="insufficient_space" />);
  expect(screen.getByText(/insufficient_space/)).toBeTruthy();
});

// --- the write outcome, which is step 16's clause -------------------------

const gate = (over: Partial<RetentionGateResult> = {}): RetentionGateResult => ({
  node: "node-a",
  evicted: true,
  outcome: "applied",
  ...over,
});

/**
 * WHICH STATE A READER IS LOOKING AT, asked of the design system.
 *
 * Every banner here is a claim about a state, and the claim is the one this
 * panel's whole split exists for: the confirmation is drawn as a success, the
 * durable-but-unapplied outcome as a caution, and an unresolved write or an
 * open capacity operation as a failure. The class that paints each belongs to
 * uilet, changes on a bump and is invisible to a reader, so the assertion
 * compares against what uilet itself draws for that variant rather than
 * spelling a name. See testing.tsx.
 */
function drawnAs(banner: Element, variant: "success" | "warning" | "danger"): boolean {
  return isDrawnAs(banner, Callout, { variant }, {});
}

// `applied` IS THE ONLY ONE THAT MEANS IT LANDED HERE.
test("an applied gate renders as the confirmation", () => {
  render(<GateOutcome result={gate({ position: { seq: 41 } })} evict />);
  const banner = screen.getByRole("status");
  expect(drawnAs(banner, "success")).toBe(true);
  expect(banner.textContent).toContain("node-a");
  // IT LANDED IN THIS NODE'S ROWS, which is the half `pending` cannot say.
  expect(screen.getByText(/own rows/)).toBeTruthy();
  // AND NOTHING IS TO BE DONE ABOUT IT, so it interrupts nobody.
  expect(screen.queryByRole("alert")).toBeNull();
});

// `pending` IS DURABLE AND UNRESOLVED, and it must be visually distinct from
// the confirmed state: a chip that reads as a tick is precisely the lie.
test("a pending gate is not rendered as success and not as a failure", () => {
  render(<GateOutcome result={gate({ outcome: "pending", position: { seq: 41 } })} evict />);
  const banner = screen.getByRole("status");
  expect(drawnAs(banner, "success")).toBe(false);
  expect(drawnAs(banner, "warning")).toBe(true);
  // AND NOT A FAILURE EITHER: the record is on the log, so nothing here
  // interrupts a reader to be retried.
  expect(screen.queryByRole("alert")).toBeNull();
  // AND IT SAYS NOT TO RETRY, because the record is already on the log and a
  // second gesture appends a second one.
  expect(screen.getByText(/Retrying would append a second record/)).toBeTruthy();
  expect(screen.getByText(/at sequence 41/)).toBeTruthy();
});

// `unknown` IS THE ONE WHERE RETRYING IS CORRECT, so it is the one that
// renders as a failure, and it carries the op id, because retrying with the
// same one is what makes the retry idempotent.
test("an unknown gate renders as the failure and names the op id", () => {
  render(<GateOutcome result={gate({ outcome: "unknown", op_id: "op-7" })} evict />);
  // AN ALERT rather than a status: this is the one outcome that interrupts,
  // because it is the one with something for a person to do.
  const banner = screen.getByRole("alert");
  expect(drawnAs(banner, "danger")).toBe(true);
  expect(screen.getByText("op-7")).toBeTruthy();
});

// --- maintenance, which was visible on no screen at all -------------------

// MAINTENANCE STOPS EVERY PUBLISHER ON EVERY NODE, and an operator watching a
// company go completely quiet had nothing to look at that said why. The alarm
// that names it only fires after an hour.
test("an open capacity operation is rendered as an outage in progress", () => {
  render(
    <MaintenanceBanner
      op={{
        stream: "CREWLET_TRACKER",
        operation_id: "op-1",
        phase: "observe",
        attempt: 2,
        target_max_bytes: 2_000_000_000,
        original_max_bytes: 1_000_000_000,
        since: new Date(Date.now() - 3_600_000).toISOString(),
        by: "sre@example.com",
        participants_missing: ["node-b", "node-c"],
      }}
      now={Date.now()}
    />,
  );
  const banner = screen.getByRole("alert");
  // THE CLAIM IS THE VARIANT, compared against what the design system draws
  // for danger rather than against a class name of its own. See testing.tsx.
  expect(drawnAs(banner, "danger")).toBe(true);
  // HOW LONG IT HAS BEEN OPEN, as an elapsed duration rather than as "1h
  // ago": the second reads as something that is over, and this one is not.
  expect(banner.textContent).toContain("attempt 2, open for 1h");
  expect(banner.textContent).not.toContain("ago");
  expect(screen.getByText(/Waiting on node-b, node-c/)).toBeTruthy();
});

// NOBODY OUTSTANDING IS NOT PROGRESS, it is the operation waiting on whoever
// ran the verb. An empty list rendered as a list reads as "nearly done", which
// is the one reading that stops somebody finishing it.
test("an operation with nobody outstanding says it is waiting on its operator", () => {
  render(
    <MaintenanceBanner
      op={{
        stream: "CREWLET_TRACKER",
        operation_id: "op-1",
        phase: "sealed",
        attempt: 1,
        target_max_bytes: 2_000_000_000,
        original_max_bytes: 1_000_000_000,
        since: new Date(Date.now() - 60_000).toISOString(),
      }}
      now={Date.now()}
    />,
  );
  expect(screen.getByText(/waiting on its operator/)).toBeTruthy();
  expect(screen.queryByText(/Waiting on/)).toBeNull();
});

// THE TYPED CONFIRMATION HAS TO REACH THE SERVER.
//
// The server refuses an eviction unless `?confirm=` repeats the node id, which
// is the same shape the destructive CLI gestures use. Checking it only in the
// browser made the gesture unreachable from this dashboard for every node: the
// request it sent carried no query at all, so every press was a 400 and the
// dialog rendered the error banner.
test("the evict gesture repeats the node id in the query the server checks", async () => {
  const sent: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://engine.test");
      sent.push(url.pathname + url.search);
      return new Response(JSON.stringify({ node_id: "node-2", outcome: "applied" }), {
        status: 200,
      });
    }),
  );
  localStorage.setItem("crewlet_api_token", "t");

  render(<GateDialog node="node-2" evict={true} onClose={() => {}} />);
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "node-2" } });
  fireEvent.click(screen.getByRole("button", { name: "Evict" }));
  await waitFor(() => expect(sent.length).toBe(1));

  expect(sent[0]).toBe("/work/retention/evict/node-2?confirm=node-2");

  vi.unstubAllGlobals();
  localStorage.clear();
});

// A DOCUMENT THAT CANNOT CLAIM AN AGE SAYS SO, and one that can says nothing.
//
// The retention answer is the one a person opens DURING the outage it
// describes, so it never takes a barrier. But `stale` is a claim about age,
// and a node that could not read its own lag cannot make one: the broker was
// unreachable, or coordination was. Without the banner the two render
// identically, the same figures, read as fresh, in exactly the incident that
// made them unmeasurable.
test("a replication answer that cannot claim an age renders the reason", () => {
  render(<ServedLevelBanner level="consistent_prefix" />);
  expect(screen.getByText(/no statement about age/)).toBeTruthy();
  expect(screen.getByText("consistent_prefix")).toBeTruthy();
});

// THE CONTROL, and it is the half that fails when somebody turns this into a
// tag: a level rendered on every answer is a word nobody reads, and the one
// case that matters then arrives as a changed word rather than as a banner.
test("an answer served at stale renders no banner at all", () => {
  const { container } = render(<ServedLevelBanner level="stale" />);
  expect(container.textContent).toBe("");

  cleanup();
  // AND AN ABSENT LEVEL IS NOT AN ALARM. An older node that does not send the
  // field must not paint this screen red.
  const missing = render(<ServedLevelBanner />);
  expect(missing.container.textContent).toBe("");
});
