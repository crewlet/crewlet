/**
 * The renderings on this panel that are load-bearing, and why each one is.
 *
 * Every case here protects a rendering whose WRONG form reads as a different
 * fact rather than as a missing one. That is the failure mode a screen has and
 * a log does not: an operator acts on what the tile says, and a zero where the
 * answer is "nobody has said" is a lie they will believe.
 *
 * The gate dialog's own cases — the write outcome, the operation id it mints
 * and finishes under, force — are in GateDialog.test.tsx.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  BlockedBanner,
  DomainBlock,
  DomainSize,
  donorsCounted,
  gateAction,
  MaintenanceBanner,
  NodePositions,
  ServedLevelBanner,
  snapshotKey,
  Terms,
  Tombstone,
} from "./Retention.tsx";
import type {
  RetentionDomain,
  RetentionNode,
  RetentionSnapshot,
  RetentionTerm,
} from "~/protocol/index.ts";

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
test("a node with no published position renders a marked absence, never a zero", () => {
  render(<NodePositions node={node()} />);
  // The mark AND the sentence. `EmptyValue` puts the reason in the accessible
  // name rather than in a hover `title`, so it is assertable here at all.
  expect(screen.getByText(EMPTY_VALUE)).toBeTruthy();
  expect(screen.getByText("This node has published no position for any domain")).toBeTruthy();
  expect(screen.queryByText("0")).toBeNull();
});

// APPLIED_THROUGH BESIDE SEQ. A node applying nothing while its position
// advances looks identical to a caught-up one from either number alone, so a
// deferral rendered as lag is the state nobody diagnoses.
test("a deferral renders as its own state rather than as lag", () => {
  render(
    <NodePositions
      node={node({
        domains: {
          tracker: {
            generation: 1,
            seq: 90,
            applied_through: 41,
            deferred: 2,
            generation_state: "current",
          },
        },
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
      node={node({
        domains: {
          tracker: { generation: 1, seq: 90, applied_through: 90, generation_state: "current" },
        },
      })}
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
      node={node({
        domains: {
          tracker: { generation: 1, seq: 90, applied_through: 90, generation_state: "current" },
        },
      })}
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

const domain = (over: Partial<RetentionDomain> = {}): RetentionDomain => ({
  domain: "tracker",
  stream: "CREWLET_TRACKER_LOG",
  generation: 0,
  replay: "strict",
  first_seq: 1,
  last_seq: 90,
  bytes: 1024 * 1024 * 1024,
  max_bytes: 1024 * 1024 * 1024 + 64 * 1024 * 1024,
  trim_floor: 1,
  trim_to: 1,
  trim_floor_state: "published",
  terms: [],
  ...over,
});

// 0% FREE ON A LOG THAT KEEPS A GATE RESERVE IS NOT A LOG NOTHING CAN TAKE.
// Its ordinary writes are refused, and an eviction — the gesture that unpins
// it — still lands in the reserve, so the reserve is drawn beside the
// headroom. A log that keeps none draws no reserve rather than a zero one.
test("a log's gate reserve is drawn beside its headroom, and only where it keeps one", () => {
  render(<DomainSize domain={domain({ headroom_fraction: 0, reserve_bytes: 64 * 1024 * 1024 })} />);
  expect(screen.getByText(/0% free/)).toBeTruthy();
  expect(screen.getByText(/kept for evictions/)).toBeTruthy();

  cleanup();
  render(<DomainSize domain={domain({ domain: "vectors", headroom_fraction: 0.5 })} />);
  expect(screen.getByText(/50% free/)).toBeTruthy();
  expect(screen.queryByText(/kept for evictions/)).toBeNull();
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
  expect(screen.getByRole("alert").className).toContain("danger");
  expect(screen.getByText(/Waiting on node-b, node-c/)).toBeTruthy();
});

// NOBODY OUTSTANDING IS NOT PROGRESS — it is the operation waiting on whoever
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

// A DOCUMENT THAT CANNOT CLAIM AN AGE SAYS SO, and one that can says nothing.
//
// The retention answer is the one a person opens DURING the outage it
// describes, so it never takes a barrier. But `stale` is a claim about age,
// and a node that could not read its own lag cannot make one — the broker was
// unreachable, or coordination was. Without the banner the two render
// identically: the same figures, read as fresh, in exactly the incident that
// made them unmeasurable.
test("a replication answer that cannot claim an age renders the reason", () => {
  render(<ServedLevelBanner level="consistent_prefix" />);
  expect(screen.getByText(/no statement about age/)).toBeTruthy();
  expect(screen.getByText("consistent_prefix")).toBeTruthy();
});

// THE CONTROL, and it is the half that fails when somebody turns this into a
// badge: a level rendered on every answer is a word nobody reads, and the one
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

// THE SERVER'S SENTENCE IS THE WHOLE SENTENCE. `statelog`'s report composes
// "Nothing is being trimmed on tracker: <why>." — the headline included,
// because the CLI prints the same line — and the banner once put its own
// headline in front of it, so the operator read the headline twice. It is
// printed once; only a report with no sentence gets the banner's own.
test("a blocked trim's banner states its headline exactly once", () => {
  const blocked = (over: Partial<RetentionDomain>) =>
    ({ domain: "tracker", blocked_by: "backup_max_age", ...over }) as RetentionDomain;
  const { container } = render(
    <BlockedBanner
      domain={blocked({
        prose: "Nothing is being trimmed on tracker: no backup has been recorded.",
      })}
      now={Date.now()}
    />,
  );
  const text = container.textContent ?? "";
  expect(text.match(/Nothing is being trimmed/g)).toHaveLength(1);
  expect(text).toContain("no backup has been recorded.");
  expect(text).not.toContain("..");

  // No sentence from the server: the banner composes one, naming the term.
  cleanup();
  const bare = render(<BlockedBanner domain={blocked({})} now={Date.now()} />).container;
  expect(bare.textContent).toMatch(/Nothing is being trimmed on tracker: the backup_max_age term/);
});

// A GESTURE STILL TO BE FINISHED OWNS THE ROW'S BUTTON.
//
// The report marks a node evicted only once EVERY log holds its tombstone, so
// after a partial eviction the row offered "Evict…" again — and that press was
// a fresh gesture, writing every log the first one reached a second time and
// re-dating its eviction. A held gesture is what says which sign is in flight.
test("a node with an unfinished gesture offers to finish it rather than start afresh", () => {
  const held = {
    opId: "01a0c450-6c00-7011-a233-445566778899.evict-node-a",
    force: false,
    unanswered: "the engine did not answer within 60 seconds",
  };
  expect(gateAction(node(), { "node-a:evict": held })).toEqual({
    evict: true,
    label: "Finish eviction…",
  });
  // A PARTIAL READMISSION LEAVES THE NODE NOT EVICTED EVERYWHERE, and the
  // row would otherwise offer the eviction.
  expect(gateAction(node(), { "node-a:readmit": held })).toEqual({
    evict: false,
    label: "Finish readmission…",
  });
  // THE CONTROL: nothing held is the ordinary gesture for the node's state.
  expect(gateAction(node(), {})).toEqual({ evict: true, label: "Evict…" });
  expect(
    gateAction(
      node({
        evicted: { by: "o", at: "", effective_at: "", effective: true },
      }),
      {},
    ),
  ).toEqual({ evict: false, label: "Readmit…" });
});

// A DOMAIN THE TRIM HAS CONCLUDED NOTHING ABOUT SAYS SO — AND DRAWS AT ALL.
//
// After every reanchor, until the trim's first tick on the adopted stream, the
// row had no floor, no blocking term and `terms: null`: `terms.map` threw
// during render, and the whole screen went blank on the retention section and
// the domain page — the two things an operator opens to watch a reanchor
// recover. Guarded, both drew "floor 0 · advancing".
test("a reanchored domain with no conclusion renders its state, never advancing", () => {
  render(
    <DomainBlock
      domain={domain({
        generation: 1,
        trim_floor: 0,
        trim_to: 0,
        trim_floor_state: "none_at_generation",
        terms: [],
      })}
    />,
  );
  expect(screen.getByText(/concluded nothing about generation 1 yet/)).toBeTruthy();
  expect(screen.getByText("no conclusion yet")).toBeTruthy();
  expect(screen.getByText("no floor yet")).toBeTruthy();
  expect(screen.queryByText("advancing")).toBeNull();
  expect(screen.queryByText(/floor 0/)).toBeNull();

  // AN UNREADABLE REGISTER IS NOT A TRIM THAT HAS CONCLUDED NOTHING.
  cleanup();
  render(<DomainBlock domain={domain({ trim_floor_state: "unreadable", terms: [] })} />);
  expect(screen.getAllByText(/floor unreadable/).length).toBeGreaterThan(0);
  expect(screen.queryByText("advancing")).toBeNull();

  // THE CONTROL: a published, unblocked floor is the one that advances.
  cleanup();
  render(<DomainBlock domain={domain()} />);
  expect(screen.getByText("advancing")).toBeTruthy();
});

// A BLOCKED DOMAIN'S CONCLUSION IS "BLOCKED", not "concluded 0": the wire value
// is zero while blocked, and printed as a number it reads as a position.
test("a blocked domain reads blocked rather than a conclusion of zero", () => {
  render(
    <DomainBlock domain={domain({ trim_floor: 700, trim_to: 0, blocked_by: "backup_floor" })} />,
  );
  expect(screen.getByText(/blocked/)).toBeTruthy();
  expect(screen.queryByText(/concluded/)).toBeNull();
  expect(screen.getByText("backup_floor")).toBeTruthy();
});

// A DOMAIN THIS NODE REFUSES SAYS SO ABOVE EVERYTHING ELSE ABOUT IT, NAMING THE
// FINDING — the guides send an operator here after a restore to find it.
test("a refused domain names its refusal, the finding and the sentence", () => {
  render(
    <DomainBlock
      domain={domain({
        not_ready: {
          code: "wrong_stream",
          causes: ["recreated"],
          detail: "tracker's rows are keyed to the stream created at 2031-04-01",
        },
        writes_refused: { code: "log_truncated", detail: "peer node-4 stands past the end" },
      })}
    />,
  );
  expect(screen.getByText("wrong_stream")).toBeTruthy();
  expect(screen.getByText(/\(recreated\)/)).toBeTruthy();
  expect(screen.getByText(/keyed to the stream created at/)).toBeTruthy();
  expect(screen.getByText("log_truncated")).toBeTruthy();
});

// AN EVICTION NAMES WHO RAN IT, AND IS NOT IMMEDIATE. The node stays counted
// until `effective_at`, and an operator who cannot see that reads the
// unchanged watermark as a failure and runs the gesture twice.
test("an evicted node names its operator, and counts down until it takes effect", () => {
  const at = "2031-04-01T12:00:00Z";
  const effectiveAt = "2031-04-01T12:01:00Z";
  const { rerender } = render(
    <Tombstone
      evicted={{ by: "ops", at, effective_at: effectiveAt, effective: false }}
      now={Date.parse(at)}
    />,
  );
  expect(screen.getByText(/^evicted in /)).toBeTruthy();
  expect(screen.getByTitle(/^evicted by ops; takes effect /)).toBeTruthy();

  rerender(
    <Tombstone
      evicted={{ by: "ops", at, effective_at: effectiveAt, effective: true }}
      now={Date.parse(effectiveAt)}
    />,
  );
  expect(screen.getByText("evicted")).toBeTruthy();
  expect(screen.getByTitle("evicted by ops")).toBeTruthy();
});

// A POSITION FROM ANOTHER GENERATION IS LABELLED, NOT SUBTRACTED, and a
// diverged node is marked — nothing else on its line shows it.
test("a node on a generation the log left is labelled rather than caught up", () => {
  render(
    <NodePositions
      node={node({
        domains: {
          tracker: {
            generation: 0,
            seq: 918000000,
            applied_through: 918000000,
            generation_state: "left",
            log_diverged: true,
          },
        },
      })}
    />,
  );
  expect(screen.getByText("left gen 0")).toBeTruthy();
  expect(screen.getByText("gen 0")).toBeTruthy();
  expect(screen.getByText("log diverged")).toBeTruthy();
  expect(screen.queryByText("lag —")).toBeNull();
  expect(screen.queryByText(/behind/)).toBeNull();
});

// A DIVIDED LAYOUT'S DONORS ARE COUNTED PER PARTITION. A snapshot is a copy of
// one partition's file and the trim's sixth term is per log, so two donors of
// one partition and none of another is a fleet that cannot rejoin the second
// — which a count of every row read as two donors, satisfied. Each row is also
// its own row: one node's two partitions are two artefacts, not one twice.
test("a divided layout's donors are those of the partition fewest nodes hold", () => {
  const at = "2026-09-30T12:00:00Z";
  const held = (node_id: string, partition?: string): RetentionSnapshot => ({
    node_id,
    ...(partition ? { partition } : {}),
    at,
    domains: { [partition ? `tracker@${partition}` : "tracker"]: 4 },
  });
  const divided = [
    held("node-a", "tracker.000"),
    held("node-b", "tracker.000"),
    held("node-a", "tracker.001"),
    { node_id: "node-b", partition: "tracker.001", skip: "lagging" },
    // A NODE REPORTING NO PARTITION donates none, and is no partition.
    { node_id: "node-c" },
  ];
  expect(donorsCounted(divided)).toBe(1);
  expect(new Set(divided.map(snapshotKey)).size).toBe(divided.length);

  // THE CONTROL: layout 0's rows name no partition, and count as one.
  expect(donorsCounted([held("node-a"), held("node-b"), { node_id: "node-c" }])).toBe(2);
});
