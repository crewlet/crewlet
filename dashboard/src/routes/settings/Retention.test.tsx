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

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  AlarmItem,
  AlarmList,
  Checks,
  DomainSize,
  donorsCounted,
  gateAction,
  LogRetention,
  LogRow,
  sharedTrimStatus,
  MaintenanceBanner,
  NodePositions,
  ServedLevelBanner,
  SnapshotsCard,
  Tombstone,
} from "./Retention.tsx";
import { Router } from "~/app/router.tsx";
import { engineAlarm, ENGINE_ALARMS_NOW } from "~/test/retentionAlarms.ts";
import type {
  RetentionAlarm,
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
  expect(screen.getByText("lag unknown")).toBeTruthy();
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
    <Checks
      domain={domain({
        terms: [
          term(),
          term({ name: "feed_ack_floor", state: "n/a", seq: undefined }),
          term({ name: "snapshot_floor", state: "unknown", seq: undefined }),
        ],
      })}
    />,
  );
  expect(screen.getByText("41")).toBeTruthy();
  expect(screen.getByText("n/a")).toBeTruthy();
  expect(screen.getByText("unknown")).toBeTruthy();
});

// A CHECK IS ITS WORDS AND ITS NAME. The name is what the CLI prints and the
// guide is indexed by, so a reader who meets it there finds it here.
test("each check is named in words beside the term's own name", () => {
  render(<Checks domain={domain({ terms: [term({ name: "feed_ack_floor" })] })} />);
  expect(screen.getByText("Wake feed")).toBeTruthy();
  expect(screen.getByText("feed_ack_floor")).toBeTruthy();
});

// ONLY THE CHECK THAT DECIDES THE TRIM CARRIES ITS REMEDY. Six remedies side
// by side told a healthy single node to evict itself and called `min_age`
// the problem on a log the backup was holding.
test("only the check that decides the trim carries its remedy", () => {
  const six = (blocked?: "backup_floor") =>
    domain({
      blocked_by: blocked,
      terms: [
        term({ name: "applied", seq: 269, remedy: "Bring the node back." }),
        term({ name: "min_hold", state: "unbounded", seq: undefined, remedy: "A hold pins it." }),
        blocked
          ? term({
              name: "backup_floor",
              state: "unknown",
              seq: undefined,
              remedy: "Take a backup.",
            })
          : term({ name: "backup_floor", seq: 300, remedy: "Take a backup." }),
        term({ name: "snapshot_floor", state: "unbounded", seq: undefined, remedy: "Snapshot." }),
        term({ name: "feed_ack_floor", seq: 269, remedy: "The feed lags." }),
        term({ name: "age_floor", seq: 1, remedy: "Nothing is wrong." }),
      ],
    });
  render(<Checks domain={six("backup_floor")} />);
  expect(screen.getByText("Take a backup.")).toBeTruthy();
  expect(screen.getByText("holding the trim")).toBeTruthy();
  for (const other of ["Bring the node back.", "The feed lags.", "Nothing is wrong."]) {
    expect(screen.queryByText(other)).toBeNull();
  }

  // ADVANCING, the lowest term that binds sets the floor, and only it.
  cleanup();
  render(<Checks domain={six()} />);
  expect(screen.getByText("sets the floor")).toBeTruthy();
  expect(screen.getByText("Nothing is wrong.")).toBeTruthy();
  expect(screen.queryByText("Take a backup.")).toBeNull();
  expect(screen.queryByText("holding the trim")).toBeNull();
});

const snapshot = (over: Partial<RetentionSnapshot>): RetentionSnapshot => ({
  node_id: "node-a",
  ...over,
});

// A SINGLE NODE TAKES NO SNAPSHOTS BY DESIGN, and says it in one sentence:
// a table of one `sole_node` row, a donor count of 0 of 2 and a warning on
// every log read as four faults on a fleet working as configured.
test("a single node's missing snapshots are one sentence, not a fault", () => {
  render(<SnapshotsCard snapshots={[snapshot({ skip: "sole_node" })]} now={Date.now()} />);
  expect(screen.getByText(/Snapshots start when a second node joins/)).toBeTruthy();
  expect(screen.queryByText("sole_node")).toBeNull();
  expect(screen.queryByText(/donors/)).toBeNull();
});

// THE SNAPSHOT BLOCK CARRIES ITS OWN REASON everywhere else. A stalled
// snapshot tier and a stalled trim are different problems with different
// remedies, and the first is silent until a node tries to join.
test("a fleet's missing snapshot names its reason, node by node", () => {
  render(
    <Router>
      <SnapshotsCard
        snapshots={[
          snapshot({ node_id: "node-a", at: "2031-04-01T12:00:00Z", domains: { tracker: 4 } }),
          snapshot({ node_id: "node-b", skip: "insufficient_space" }),
        ]}
        now={Date.now()}
      />
    </Router>,
  );
  expect(screen.getByText("insufficient_space")).toBeTruthy();
  expect(screen.getByText("1 of 2 donors")).toBeTruthy();
  expect(screen.queryByText(/Snapshots start when a second node joins/)).toBeNull();
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
  expect(screen.getByText("Waiting on its operator.")).toBeTruthy();
  expect(screen.queryByText(/^Waiting on (?!its operator\.$)/)).toBeNull();
});

// A DOCUMENT THAT CANNOT CLAIM AN AGE SAYS SO, and one that can says nothing.
//
// The retention answer is the one a person opens DURING the outage it
// describes, so it never takes a barrier. But `stale` is a claim about age,
// and a node that could not read its own lag cannot make one — the broker was
// unreachable, or coordination was. Without the banner the two render
// identically: the same figures, read as fresh, in exactly the incident that
// made them unmeasurable.
test("a replication answer that cannot claim an age says so", () => {
  render(<ServedLevelBanner level="consistent_prefix" />);
  expect(
    screen.getByText("How current these figures are can’t be measured right now."),
  ).toBeTruthy();
  expect(screen.queryByText("consistent_prefix")).toBeNull();
});

// THE CONTROL, and it is the half that fails when somebody turns this into a
// badge: a level rendered on every answer is a word nobody reads, and the one
// case that matters then arrives as a changed word rather than as a banner.
test("an answer served at stale renders no banner at all", () => {
  const { container } = render(<ServedLevelBanner level="stale" />);
  expect(container.textContent).toBe("");

  cleanup();
  // AND AN ABSENT LEVEL — the answer has not arrived — IS NOT AN ALARM: it
  // must not paint this screen red.
  const missing = render(<ServedLevelBanner />);
  expect(missing.container.textContent).toBe("");
});

/**
 * A `trim_blocked` or `log_headroom` alarm a case shapes for itself. The backup
 * alarm is never typed here: every case drawing one reads the engine's own
 * rendering (`~/test/retentionAlarms.ts`).
 */
const alarm = (over: Partial<RetentionAlarm> & Pick<RetentionAlarm, "kind">): RetentionAlarm => ({
  detail: "",
  remedy: "The term holding each log names what to fix.",
  ...over,
});

const NOW = ENGINE_ALARMS_NOW;
const HELD_SINCE = "2031-03-31T05:14:00Z"; // 46h before NOW

/** The engine's `trim_blocked` sentence for a log keeping `kept` records past its window. */
const keeping = (kept: number) =>
  `the trim has not advanced for 46h, and the log is keeping up to ${kept} record(s) older than its 24h replay window`;

// ONE CAUSE IS ONE ALARM. A company with no backup was drawn as a banner per
// log, a `trim_blocked` per log and a backup alarm: five problems for one.
// The logs one term holds are one line under the alarm whose remedy covers
// them, dated by the one held longest — and each still says its own backup's
// measurement and what it keeps past its window, which is why it is listed at
// all and which no date says.
test("an alarm is drawn once, with the logs it holds as one line and each log's own count", () => {
  const backup = engineAlarm("held");
  const domains = backup.domains ?? [];
  const { container } = render(<AlarmItem alarm={backup} now={NOW} />);
  const text = container.textContent ?? "";
  expect(screen.getByText("Backup age")).toBeTruthy();
  // THE HEADLINE IS THE MOST PRESSING LOG'S OWN MEASUREMENT, as a sentence.
  expect(text).toContain(
    "No verified backup of this log has been recorded, and the policy asks for one every 24h; " +
      "the trim has not advanced for 46h",
  );
  expect(text.match(/Trim held by/g)).toHaveLength(1);
  const names = domains.flatMap((d) => d.domains);
  expect(text).toContain(
    `Trim held by the backup on ${names.slice(0, -1).join(", ")} and ${names.at(-1)}, for 46h.`,
  );
  // EACH LOG SAYS ITS OWN COUNT, and no two say the same one.
  expect(new Set(domains.map((d) => d.detail)).size).toBe(domains.length);
  for (const d of domains) {
    expect(text).toContain(`${d.domains.join(", ")}: ${d.detail}`);
  }
  // EACH LOG IS A WAY TO ITS OWN PAGE, wherever it is named.
  const links = screen.getAllByRole("link", { name: "pages" });
  expect(links.length).toBeGreaterThan(0);
  for (const link of links) {
    expect(link.getAttribute("href")).toContain("settings/backups/pages");
  }
  // THE REMEDY IS THE ENGINE'S, with its command drawn as code.
  expect(screen.getByText("crewlet backup").tagName).toBe("CODE");
});

// A BLOCKED TRIM ON TWO LOGS LEADS WITH THE MORE PRESSING ONE, and the other
// still says its own count: read off the headline alone, the second log was
// keeping what the first one keeps.
test("a trim blocked on two logs says each log's count, not only the lead's", () => {
  const { container } = render(
    <AlarmItem
      alarm={alarm({
        kind: "trim_blocked",
        detail: keeping(1200),
        domains: [
          { domains: ["tracker"], detail: keeping(1200), term: "applied", since: HELD_SINCE },
          { domains: ["pages"], detail: keeping(7), term: "applied", since: HELD_SINCE },
        ],
      })}
      now={NOW}
    />,
  );
  const text = container.textContent ?? "";
  expect(text).toContain("Trim held by");
  expect(text).toContain(`pages: ${keeping(7)}`);
  expect(text).toContain(`tracker: ${keeping(1200)}`);
});

// ONE HELD LOG WHOSE SENTENCE IS THE HEADLINE is not said twice: the line
// keeps its term and its date, and the count is the headline's.
test("a trim blocked on one log says its count once", () => {
  const { container } = render(
    <AlarmItem
      alarm={alarm({
        kind: "trim_blocked",
        detail: keeping(1200),
        domains: [
          { domains: ["tracker"], detail: keeping(1200), term: "applied", since: HELD_SINCE },
        ],
      })}
      now={NOW}
    />,
  );
  const text = container.textContent ?? "";
  expect(text.match(/1200 record\(s\) older/g)).toHaveLength(1);
  expect(text).toContain("Trim held by");
  expect(text).toContain("for 46h.");
});

// ONE LOG'S STALE COPY IS THE BACKUP ALARM'S WHOLE MEASUREMENT, folded into
// the headline as any single log's is: the log is named beside it, and no
// held trim is drawn for a hold that has not yet lasted past the window.
test("a backup alarm on one log says that log's measurement once, beside it", () => {
  const { container } = render(<AlarmItem alarm={engineAlarm("stale")} now={NOW} />);
  const text = container.textContent ?? "";
  expect(text.match(/30h old/g)).toHaveLength(1);
  expect(text).toContain("tracker: The newest verified backup of this log is 30h old");
  expect(screen.getByRole("link", { name: "tracker" })).toBeTruthy();
  expect(text).not.toContain("Trim held by");
});

// A YOUNG FLEET'S MISSING BACKUP IS SAID ONCE, NAMING EVERY LOG. The engine
// measures it in the same words on each log and carries it once, naming them,
// so the panel draws one sentence with the logs beside it — it drew the
// headline and then that sentence once per log, one fact said six times.
test("a measurement taken alike on every log is said once, naming them all", () => {
  const backup = engineAlarm("young");
  const names = (backup.domains ?? []).flatMap((d) => d.domains);
  expect(names.length).toBeGreaterThan(1);
  const { container } = render(<AlarmItem alarm={backup} now={NOW} />);
  const text = container.textContent ?? "";
  expect(text.match(/o verified backup of this log has been recorded/g)).toHaveLength(1);
  expect(text).toContain(
    `${names.slice(0, -1).join(", ")} and ${names.at(-1)}: No verified backup of this log`,
  );
  for (const name of names) {
    expect(screen.getByRole("link", { name })).toBeTruthy();
  }
});

// A SINGLE LOG WHOSE MEASUREMENT IS THE ALARM'S IS FOLDED INTO IT, as the CLI
// folds it: drawn as both the headline and a line, one sentence read twice.
test("a single log's measurement is said once, with the log beside it", () => {
  const detail = "3% of the log's byte ceiling for ordinary writes is left";
  const { container } = render(
    <AlarmItem
      alarm={alarm({ kind: "log_headroom", detail, domains: [{ domains: ["vectors"], detail }] })}
      now={NOW}
    />,
  );
  expect(container.textContent?.match(/byte ceiling/g)).toHaveLength(1);
  expect(screen.getByRole("link", { name: "vectors" })).toBeTruthy();
});

// AN ALARM IS A ROW OF ITS CARD, NOT A CALLOUT IN IT: a bordered warning
// callout inside the bordered Alarms card was a frame in a frame. And it
// offers no Take a backup of its own, since the page bar's is on the same
// screen. Mutation: draw an alarm as a Callout and the first query finds one.
test("an alarm is a row of the alarms card, with no button the page bar already offers", () => {
  const { container } = render(
    <AlarmList
      alarms={[engineAlarm("held"), engineAlarm("held_beside_a_blocked_trim", "trim_blocked")]}
      now={NOW}
    />,
  );
  expect(container.querySelector(".crewlet-callout")).toBeNull();
  const rows = container.querySelectorAll(".alarm-rows > .alarm-row");
  expect(rows).toHaveLength(2);
  expect(screen.getByText("Backup age").closest(".alarm-row")).toBe(rows[0]);
  expect(screen.queryByRole("button", { name: "Take a backup" })).toBeNull();
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
    <LogRow
      domain={domain({
        generation: 1,
        trim_floor: 0,
        trim_to: 0,
        trim_floor_state: "none_at_generation",
        terms: [],
      })}
    />,
  );
  openRow("tracker");
  expect(screen.getByText("No trim result for generation 1 yet.")).toBeTruthy();
  expect(screen.getByText("no conclusion yet")).toBeTruthy();
  expect(screen.getByText("no floor yet")).toBeTruthy();
  expect(screen.queryByText("advancing")).toBeNull();
  expect(screen.queryByText(/floor 0/)).toBeNull();

  // AN UNREADABLE REGISTER IS NOT A TRIM THAT HAS CONCLUDED NOTHING.
  cleanup();
  render(<LogRow domain={domain({ trim_floor_state: "unreadable", terms: [] })} />);
  expect(screen.getAllByText(/floor unreadable/).length).toBeGreaterThan(0);
  expect(screen.queryByText("advancing")).toBeNull();

  // THE CONTROL: a published, unblocked floor is the one that advances.
  cleanup();
  render(<LogRow domain={domain()} />);
  expect(screen.getByText("advancing")).toBeTruthy();
});

/** Opens a log row's checks by pressing its disclosure, as a reader does. */
function openRow(name: string) {
  fireEvent.click(screen.getByRole("button", { name: new RegExp(name) }));
}

// A BLOCKED DOMAIN'S CONCLUSION IS "BLOCKED", not "concluded 0": the wire value
// is zero while blocked, and printed as a number it reads as a position.
test("a blocked domain reads blocked rather than a conclusion of zero", () => {
  render(
    <LogRow
      domain={domain({
        trim_floor: 700,
        trim_to: 0,
        blocked_by: "backup_floor",
        terms: [term({ name: "backup_floor", state: "unknown", seq: undefined })],
      })}
    />,
  );
  expect(screen.getByText(/blocked/)).toBeTruthy();
  expect(screen.queryByText(/concluded/)).toBeNull();
  // WHAT HOLDS IT, in words on the row and by its own name in the checks.
  expect(screen.getByText("held by the backup")).toBeTruthy();
  openRow("tracker");
  const checks = screen.getByText("backup_floor").closest("dl") as HTMLElement;
  expect(within(checks).getByText("holding the trim")).toBeTruthy();
});

// A DOMAIN THIS NODE REFUSES SAYS SO ABOVE EVERYTHING ELSE ABOUT IT, NAMING THE
// FINDING — the guides send an operator here after a restore to find it.
test("a refused domain names its refusal, the finding and the sentence", () => {
  render(
    <LogRow
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
  expect(screen.queryByText("lag unknown")).toBeNull();
  expect(screen.queryByText(/behind/)).toBeNull();
});

// A DONOR IS A NODE HOLDING AN ARTEFACT, ONE ROW EACH. Every data node keeps the
// one estate, so its row is its one artefact: a node that took one counts, and
// one that holds none — whatever its reason — does not.
test("the donors are the nodes holding an artefact, one row each", () => {
  const at = "2026-09-30T12:00:00Z";
  const held = (node_id: string): RetentionSnapshot => ({ node_id, at, domains: { tracker: 4 } });
  expect(
    donorsCounted([held("node-a"), held("node-b"), { node_id: "node-c", skip: "lagging" }]),
  ).toBe(2);
  expect(donorsCounted([{ node_id: "node-c" }])).toBe(0);
});

// ONE CAUSE ON EVERY LOG IS SAID ONCE: "held by the backup" on all seven rows
// was one fact seven times, and at a phone's width the tag kept its width while
// the log's name was cut. The head says the shared state, and only a log that
// differs carries a tag. Mutation: tag every row and the first count is three.
test("log retention says the trim state most logs share once, and tags only the logs that differ", () => {
  const held = { blocked_by: "backup_floor" as const };
  const domains = [
    domain({ domain: "tracker", ...held }),
    domain({ domain: "pages", ...held }),
    domain({ domain: "usage" }),
  ];
  expect(sharedTrimStatus(domains)).toEqual({
    key: "held:backup_floor",
    label: "held by the backup",
    count: 2,
  });
  const { container } = render(<LogRetention domains={domains} />);
  expect(screen.getByText("Held by the backup on 2 of 3 logs")).toBeTruthy();
  expect(screen.queryAllByText("held by the backup")).toHaveLength(0);
  expect(screen.getByText("advancing").closest(".crewlet-tag")).not.toBeNull();
  expect(container.querySelectorAll(".log-row")).toHaveLength(3);

  // NO MAJORITY, NO SHARED LINE: every row says its own.
  expect(sharedTrimStatus([domain({ ...held }), domain()])).toBeNull();
  expect(sharedTrimStatus([domain()])).toBeNull();
});
