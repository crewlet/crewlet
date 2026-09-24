/**
 * The Fleet screen's hold on a gesture it just made, across the dialog closing
 * and the report catching up.
 *
 * Its own file because it mocks the screen's query hook, which the renderings
 * in Retention.test.tsx must not see.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { engineFile } from "~/test/engineFiles.ts";
import type {
  RetentionDomain,
  RetentionGateResult,
  RetentionNode,
  RetentionReport,
} from "~/protocol/index.ts";
import { Router } from "~/app/router.tsx";
import { heldGestures, RetentionPanels } from "./Retention.tsx";

const query = vi.hoisted(() => ({
  data: null as unknown,
  refetch: (() => {}) as () => void,
}));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({ data: query.data, loading: false, error: null, refetch: query.refetch }),
}));

const golden = engineFile<{ answers: Record<string, RetentionGateResult> }>(
  "internal/api/testdata/gate_answer.json",
);

const node = (over: Partial<RetentionNode> = {}): RetentionNode => ({
  node_id: "node-4",
  counted: true,
  live: false,
  ...over,
});

const evicted = { by: "ops", at: "", effective_at: "", effective: true };

/** One identity-claiming log's row, its evictions read unless it says not. */
const logRow = (domain: string, over: Partial<RetentionDomain> = {}): RetentionDomain => ({
  domain,
  stream: `CREWLET_${domain.toUpperCase()}_LOG`,
  generation: 1,
  replay: "strict",
  first_seq: 1,
  last_seq: 1,
  bytes: 0,
  trim_floor: 0,
  trim_to: 0,
  trim_floor_state: "none_at_generation",
  terms: [],
  ...over,
});

/** The two logs every gate gesture writes, both read. */
const bothLogs = (): RetentionDomain[] => [logRow("tracker"), logRow("pages")];

function report(nodes: RetentionNode[], domains: RetentionDomain[] = bothLogs()): RetentionReport {
  return {
    v: 1,
    node_id: "node-1",
    at: "2026-09-24T00:00:00Z",
    domains,
    nodes,
    register_readable: true,
    snapshots: [],
    replica: { store_bytes: 0, projected_join_seconds: 0, rejoin_window_seconds: 0 },
    alarms: [],
  };
}

/** What the screen holds against a report served by `servedBy`. */
function held(
  gestures: Parameters<typeof heldGestures>[0],
  nodes: RetentionNode[],
  servedBy?: string,
  domains: RetentionDomain[] = bothLogs(),
) {
  return heldGestures(gestures, { nodes, domains, node_id: servedBy ?? "" });
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

// A GESTURE THAT FINISHED IS HELD UNTIL THE REPORT SHOWS IT.
//
// The report is polled. Until its next answer, the row behind a dialog that
// had just said "evicted on every log — do not retry" still read "Evict…", and
// reopening it minted a fresh operation id: a second record on every log,
// re-dating the eviction. The screen now asks the report again at once, and
// until the report agrees the row reopens the gesture's own answer.
test("a complete gesture reopens as itself until the report shows it", async () => {
  const refetch = vi.fn();
  query.refetch = refetch;
  query.data = report([node()]);
  const sent: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      sent.push(String(input));
      return new Response(JSON.stringify(golden.answers["pending"]), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  const { rerender } = render(
    <Router>
      <RetentionPanels />
    </Router>,
  );

  fireEvent.click(screen.getByRole("button", { name: "Evict…" }));
  fireEvent.change(screen.getByLabelText("Type node-4 to confirm"), {
    target: { value: "node-4" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Evict" }));
  await waitFor(() => expect(screen.getByText(/Durable on every log/)).toBeTruthy());
  // THE REPORT IS ASKED AGAIN AT ONCE, rather than at the next poll.
  expect(refetch).toHaveBeenCalled();

  fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]!);
  // NOT "Evict…" — which would start a second gesture over the logs this one
  // already holds.
  fireEvent.click(screen.getByRole("button", { name: "Eviction sent…" }));
  expect(screen.getByText(/Durable on every log/)).toBeTruthy();
  expect(screen.queryByLabelText("Type node-4 to confirm")).toBeNull();
  expect(sent.length).toBe(1);
  fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]!);

  // ONCE THE REPORT SHOWS IT, it is let go of: the node's own state decides.
  query.data = report([node({ evicted })]);
  rerender(
    <Router>
      <RetentionPanels />
    </Router>,
  );
  await waitFor(() => expect(screen.getByRole("button", { name: "Readmit…" })).toBeTruthy());
});

// WHAT IS HELD AGAINST A REPORT: every gesture a request can still finish, and
// every complete one the report does not show yet — and nothing the report
// already shows, or a later gesture on the node would reopen this one.
test("a held gesture is let go of exactly when the report shows it", () => {
  const complete = { opId: "op", force: false, answer: golden.answers["applied"] };
  const partial = { opId: "op", force: false, answer: golden.answers["unknown"] };
  const gestures = { "node-4:evict": complete, "node-5:evict": partial };
  const nodes = [node({ evicted }), node({ node_id: "node-5", evicted })];
  // node-4's eviction is shown and let go of; node-5's is unfinished and kept.
  expect(Object.keys(held(gestures, nodes))).toEqual(["node-5:evict"]);
  // A REPORT THAT DOES NOT SHOW IT YET keeps the complete one.
  expect(Object.keys(held(gestures, [node(), node({ node_id: "node-5" })]))).toEqual([
    "node-4:evict",
    "node-5:evict",
  ]);
  // A READMISSION IS SHOWN when the node is no longer evicted.
  const readmitted = { "node-4:readmit": complete };
  expect(held(readmitted, [node()])).toEqual({});
  expect(Object.keys(held(readmitted, [node({ evicted })]))).toEqual(["node-4:readmit"]);
});

/** The serving node's own row, applied through `seq` on both logs. */
const servingNode = (seq: { tracker: number; pages: number }, generation = 1): RetentionNode =>
  node({
    node_id: "node-1",
    live: true,
    domains: Object.fromEntries(
      (["tracker", "pages"] as const).map((domain) => [
        domain,
        {
          generation,
          seq: seq[domain],
          applied_through: seq[domain],
          generation_state: "current",
        },
      ]),
    ) as RetentionNode["domains"],
  });

// A COMPLETE GESTURE A LATER CHANGE OVERTOOK IS LET GO OF. The operator evicts
// node-4 here and, before the refetch lands, somebody readmits it from the
// command line: the report shows node-4 counted and the row read "Eviction
// sent…" for the rest of the page's life, reopening an answer with nothing to
// press. Once the node serving the report has applied every record the
// gesture wrote, its disagreement is a later change, not a lag.
test("a complete eviction somebody else undid is let go of once the report includes it", () => {
  const complete = { opId: "op", force: false, answer: golden.answers["applied"] };
  const gestures = { "node-4:evict": complete };
  // THE GOLDEN'S RECORDS: the tracker's at 918280002, the pages log's at 4410.
  const past = servingNode({ tracker: 918280010, pages: 4420 });
  expect(held(gestures, [node(), past], "node-1")).toEqual({});
  // NOT WHILE THE SERVING NODE IS BEHIND EITHER RECORD — a `pending` record
  // it has not applied is exactly why a report disagrees — nor on another
  // generation's number space, nor when it cannot say where it is.
  for (const [why, nodes, servedBy] of [
    [
      "behind the pages record",
      [node(), servingNode({ tracker: 918280010, pages: 4409 })],
      "node-1",
    ],
    ["another generation", [node(), servingNode({ tracker: 918280010, pages: 4420 }, 2)], "node-1"],
    ["no row of its own", [node()], "node-1"],
    ["no serving node named", [node(), past], undefined],
  ] as const) {
    expect(Object.keys(held(gestures, [...nodes], servedBy)), why).toEqual(["node-4:evict"]);
  }
  // AND AN UNFINISHED GESTURE IS NEVER LET GO OF THIS WAY: a request can
  // still finish it.
  const partial = {
    "node-4:evict": { opId: "op", force: false, answer: golden.answers["unknown"] },
  };
  expect(Object.keys(held(partial, [node(), past], "node-1"))).toEqual(["node-4:evict"]);
});

// A NODE THAT COULD NOT READ ITS EVICTIONS PROVES NOTHING BY SHOWING NONE. An
// unread log contributes no tombstone, so node-4 reads as not evicted there
// whatever happened — during an adoption's rename, say — and that absence let
// go of an eviction the operator had just made, offering "Evict…" again: a
// second record on every log, re-dating the first.
test("a complete eviction is held while the serving node could not read the evictions", () => {
  const gestures = {
    "node-4:evict": { opId: "op", force: false, answer: golden.answers["applied"] },
  };
  const past = servingNode({ tracker: 918280010, pages: 4420 });
  expect(held(gestures, [node(), past], "node-1")).toEqual({});
  for (const [why, domains] of [
    ["the pages log unread", [logRow("tracker"), logRow("pages", { evictions_unreadable: true })]],
    [
      "the tracker log unread",
      [logRow("tracker", { evictions_unreadable: true }), logRow("pages")],
    ],
    ["no row for a log the gesture wrote", [logRow("tracker")]],
  ] as const) {
    expect(Object.keys(held(gestures, [node(), past], "node-1", [...domains])), why).toEqual([
      "node-4:evict",
    ]);
  }
});

test("a log whose evictions could not be read says so above the node block", () => {
  query.data = report(
    [node()],
    [logRow("tracker"), logRow("pages", { evictions_unreadable: true })],
  );
  render(
    <Router>
      <RetentionPanels />
    </Router>,
  );
  expect(screen.getByText(/an eviction may be hidden rather than absent/)).toBeTruthy();
  expect(screen.getAllByText(/could not read/).length).toBe(1);
});

test("the row offers a new eviction once a readmission elsewhere overtook this one", async () => {
  query.refetch = vi.fn();
  query.data = report([node(), servingNode({ tracker: 1, pages: 1 })]);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify(golden.answers["applied"]), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    ),
  );
  const { rerender } = render(
    <Router>
      <RetentionPanels />
    </Router>,
  );
  fireEvent.click(rowButton("node-4", "Evict…"));
  fireEvent.change(screen.getByLabelText("Type node-4 to confirm"), {
    target: { value: "node-4" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Evict" }));
  await waitFor(() => expect(screen.getByText(/evicted on every log/)).toBeTruthy());
  fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]!);
  expect(screen.getByRole("button", { name: "Eviction sent…" })).toBeTruthy();

  // READMITTED FROM THE COMMAND LINE, and the serving node has applied past
  // both of this gesture's records: node-4 reads counted, and the row is
  // the node's own state again.
  query.data = report([node(), servingNode({ tracker: 918280010, pages: 4420 })]);
  rerender(
    <Router>
      <RetentionPanels />
    </Router>,
  );
  await waitFor(() => expect(screen.queryByRole("button", { name: "Eviction sent…" })).toBeNull());
  expect(rowButton("node-4", "Evict…")).toBeTruthy();
});

/**
 * The gate button on one node's row: the one whose nearest ancestor naming a
 * node names this one — the row, whose first cell is the node id.
 */
function rowButton(nodeId: string, name: string): HTMLElement {
  const rowOf = (b: HTMLElement): string => {
    for (let at = b.parentElement; at; at = at.parentElement) {
      const text = at.textContent ?? "";
      if (/node-\d/.test(text)) return text;
    }
    return "";
  };
  const button = screen.getAllByRole("button", { name }).find((b) => rowOf(b).includes(nodeId));
  if (!button) throw new Error(`no ${name} button on ${nodeId}'s row`);
  return button;
}
