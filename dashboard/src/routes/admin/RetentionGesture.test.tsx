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
import type { RetentionGateResult, RetentionNode, RetentionReport } from "~/protocol/index.ts";
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

function report(nodes: RetentionNode[]): RetentionReport {
  return {
    v: 1,
    node_id: "node-1",
    at: "2026-09-24T00:00:00Z",
    domains: [],
    nodes,
    register_readable: true,
    snapshots: [],
    replica: { store_bytes: 0, projected_join_seconds: 0, rejoin_window_seconds: 0 },
    alarms: [],
  };
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
  expect(Object.keys(heldGestures(gestures, nodes))).toEqual(["node-5:evict"]);
  // A REPORT THAT DOES NOT SHOW IT YET keeps the complete one.
  expect(Object.keys(heldGestures(gestures, [node(), node({ node_id: "node-5" })]))).toEqual([
    "node-4:evict",
    "node-5:evict",
  ]);
  // A READMISSION IS SHOWN when the node is no longer evicted.
  const readmitted = { "node-4:readmit": complete };
  expect(heldGestures(readmitted, [node()])).toEqual({});
  expect(Object.keys(heldGestures(readmitted, [node({ evicted })]))).toEqual(["node-4:readmit"]);
});
