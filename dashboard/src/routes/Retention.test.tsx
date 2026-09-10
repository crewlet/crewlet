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
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { GateOutcome } from "./GateDialog.tsx";
import { NodePositions, Terms } from "./Retention.tsx";
import type { RetentionNode, RetentionTerm } from "~/protocol/index.ts";

afterEach(cleanup);

const node = (over: Partial<RetentionNode> = {}): RetentionNode => ({
  node_id: "node-a",
  counted: true,
  live: true,
  ...over,
});

// A NODE THAT PUBLISHED NOTHING IS NOT A NODE AT ZERO. The two block the trim
// for very different lengths of time, and a 0 in the position column is a
// claim that this node has applied nothing — which is the opposite of "we do
// not know yet".
test("a node with no published position renders an em-dash, never a zero", () => {
  render(<NodePositions node={node()} />);
  expect(screen.getByText("—")).toBeTruthy();
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
  expect(screen.getByText("lag —")).toBeTruthy();
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

// `applied` IS THE ONLY ONE THAT MEANS IT LANDED HERE.
test("an applied gate renders as the confirmation", () => {
  render(
    <GateOutcome
      result={{ node: "node-a", evicted: true, outcome: "applied", position: { seq: 41 } }}
      evict
    />,
  );
  expect(screen.getByRole("status").className).toContain("positive");
});

// `pending` IS DURABLE AND UNRESOLVED, and it must be visually distinct from
// the confirmed state: a chip that reads as a tick is precisely the lie.
test("a pending gate is not rendered as success and not as a failure", () => {
  render(
    <GateOutcome
      result={{ node: "node-a", evicted: true, outcome: "pending", position: { seq: 41 } }}
      evict
    />,
  );
  const banner = screen.getByRole("status");
  expect(banner.className).not.toContain("positive");
  expect(banner.className).toContain("caution");
  // AND IT SAYS NOT TO RETRY, because the record is already on the log and a
  // second gesture appends a second one.
  expect(screen.getByText(/Retrying would append a second record/)).toBeTruthy();
  expect(screen.getByText(/at sequence 41/)).toBeTruthy();
});

// `unknown` IS THE ONE WHERE RETRYING IS CORRECT, so it is the one that
// renders as a failure — and it carries the op id, because retrying with the
// same one is what makes the retry idempotent.
test("an unknown gate renders as the failure and names the op id", () => {
  render(
    <GateOutcome
      result={{ node: "node-a", evicted: true, outcome: "unknown", op_id: "op-7" }}
      evict
    />,
  );
  expect(screen.getByRole("alert").className).toContain("critical");
  expect(screen.getByText("op-7")).toBeTruthy();
});
