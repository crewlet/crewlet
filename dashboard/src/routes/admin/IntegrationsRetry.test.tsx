/**
 * The Integrations screen's REST reads ask again when the engine says.
 *
 * Nothing pushes what `/setup` answers, so the screen reads it over REST: the
 * listing once (and again when the tab comes back), the pass history on a
 * poll, one pass while it runs. A `503` the engine wrote was none of their
 * business: the listing went quiet with no buttons and nothing asked again,
 * the history blanked into "No pass has run on this node" and waited out its
 * minute, and one pass disappeared from under the row that opened it. A `503`
 * says when (`Retry-After`), and with none it says waiting will not change
 * the answer; these hold that each read takes it at its word.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CATALOG, SetupPasses, useSetup, useSetupRun, useSetupRuns } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import type { SetupRun } from "~/protocol/types.ts";

/**
 * The fetches each path received, in order, and WHEN on the fake clock: a
 * wait is held as the gap between two asks, because `waitFor` moves the fake
 * clock while it polls and an absolute instant would measure that instead.
 */
let asked: Array<{ path: string; at: number }> = [];

/** What each read answers: the scripted answers per path, the last repeating. */
function answering(script: Record<string, Array<() => Response>>) {
  const next: Record<string, number> = {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      asked.push({ path, at: Date.now() });
      const answers = script[path] ?? [() => json({}, 200)];
      const at = Math.min(next[path] ?? 0, answers.length - 1);
      next[path] = at + 1;
      return answers[at]!();
    }),
  );
}

function json(body: unknown, status: number, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

/** The engine's own `503`, with the hint it names — none for "no wait clears it". */
const unavailable = (retryAfter?: number) => () =>
  json(
    { error: "identity_unavailable", detail: "this node could not read the identity estate" },
    503,
    retryAfter === undefined ? {} : { "Retry-After": String(retryAfter) },
  );

const run = (state: SetupRun["state"]): SetupRun => ({
  run_id: "r1",
  key: "jira",
  state,
  started_at: "2026-09-30T12:00:00Z",
});

const listed = (state: SetupRun["state"]) => () =>
  json({ runs: [run(state)], scope: "node-a" }, 200);

/** The whole-second gaps between the asks of one path — see [asked]. */
function gaps(path: string): number[] {
  const at = asked.filter((a) => a.path === path).map((a) => a.at);
  return at.slice(1).map((t, i) => Math.floor((t - at[i]!) / 1_000));
}

const reads = (path: string) => asked.filter((a) => a.path === path).length;

async function wait(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

beforeEach(() => {
  asked = [];
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const LISTING = "/setup/integrations";
const RUNS = "/setup/integrations/jira/runs";
const ONE = "/setup/integrations/jira/runs/r1";

function Listing() {
  const setup = useSetup();
  return (
    <span data-testid="listing">
      {setup.loading ? "loading" : `${setup.unavailable?.retryAfter ?? "-"}:${setup.byKey.size}`}
    </span>
  );
}

// NOTHING POLLS THE LISTING, so a node that said two seconds was asked again
// only when somebody changed tabs.
test("a listing the node could not read is read again when it says", async () => {
  answering({ [LISTING]: [unavailable(12), () => json({ tools: [] }, 200)] });
  render(<Listing />);
  await vi.waitFor(() => expect(screen.getByTestId("listing").textContent).toBe("12:0"));
  await wait(20_000);
  expect(gaps(LISTING)).toEqual([12]);
  expect(screen.getByTestId("listing").textContent).toBe("-:0");
});

test("a listing the engine said waiting will not clear is not read again on a timer", async () => {
  answering({ [LISTING]: [unavailable()] });
  render(<Listing />);
  await vi.waitFor(() => expect(screen.getByTestId("listing").textContent).toBe("0:0"));
  await wait(600_000);
  expect(reads(LISTING)).toBe(1);
});

function History() {
  const { runs, unavailable } = useSetupRuns(["jira"]);
  return (
    <span data-testid="history">
      {runs.map((r) => r.state).join(",")}|{unavailable?.retryAfter ?? "-"}
    </span>
  );
}

// THE HINT REPLACES THE NEXT TICK IN BOTH DIRECTIONS. A pass in flight is
// followed every four seconds, and a node that then said twelve is not asked
// at four; an idle history is read once a minute, and a node that said two is
// not left for the minute.
test("the pass history waits the engine's hint in place of its own tick", async () => {
  answering({ [RUNS]: [listed("running"), unavailable(12), unavailable(2), listed("done")] });
  render(<History />);
  await vi.waitFor(() => expect(screen.getByTestId("history").textContent).toBe("running|-"));
  await wait(20_000);
  // 4 s after the running pass, 12 s after the first refusal, 2 s after the
  // second — and the history it had stays through both.
  expect(gaps(RUNS)).toEqual([4, 12, 2]);
  expect(screen.getByTestId("history").textContent).toBe("done|-");
});

// AND A ZERO STOPS THE POLL: the node said asking again will not change it.
// The tab coming back is a person looking again, which asks.
test("the pass history stops polling a refusal no wait clears, until the tab comes back", async () => {
  answering({ [RUNS]: [listed("done"), unavailable(), listed("done")] });
  render(<History />);
  await vi.waitFor(() => expect(reads(RUNS)).toBe(1));
  await wait(60_000);
  expect(reads(RUNS)).toBe(2);
  // KEPT, not blanked into "no pass has run": one failed read is no claim
  // about the integration.
  expect(screen.getByTestId("history").textContent).toBe("done|0");
  await wait(600_000);
  expect(reads(RUNS)).toBe(2);

  Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  await wait(0);
  expect(reads(RUNS)).toBe(3);
});

// A FAILURE THE ENGINE GAVE NO HINT FOR keeps the cadence it was on, as the
// interval this replaced did: a request that never arrived says nothing about
// whether the pass it was following has ended.
test("a failure with no hint keeps the poll's cadence", async () => {
  answering({
    [RUNS]: [listed("running"), () => json({ error: "internal_error" }, 500), listed("done")],
  });
  render(<History />);
  await vi.waitFor(() => expect(reads(RUNS)).toBe(1));
  await wait(10_000);
  expect(gaps(RUNS)).toEqual([4, 4]);
});

function Pass() {
  const { run, unavailable } = useSetupRun("jira", "r1");
  return (
    <span data-testid="pass">
      {run?.state ?? "none"}|{unavailable?.retryAfter ?? "-"}
    </span>
  );
}

// ONE PASS IS FOLLOWED WHILE IT RUNS, and a node that could not answer is asked
// again when it says rather than dropping the pass from under the row.
test("one pass is asked again when the engine says, and kept meanwhile", async () => {
  answering({
    [ONE]: [() => json(run("running"), 200), unavailable(9), () => json(run("done"), 200)],
  });
  render(<Pass />);
  await vi.waitFor(() => expect(screen.getByTestId("pass").textContent).toBe("running|-"));
  await wait(4_000);
  expect(screen.getByTestId("pass").textContent).toBe("running|9");
  await wait(20_000);
  expect(gaps(ONE)).toEqual([4, 9]);
  expect(screen.getByTestId("pass").textContent).toBe("done|-");
});

// THE PANEL SAYS SO, rather than "No pass has run on this node": a history the
// node could not read is not an empty one.
test("the pass panel draws a node that could not answer, not an empty history", async () => {
  answering({ [RUNS]: [unavailable(12)] });
  const entry = CATALOG.find((e) => e.surfaces.some((s) => s.key === "jira"))!;
  render(
    <Router>
      <SetupPasses entry={entry} kinds={["jira"]} />
    </Router>,
  );
  await vi.waitFor(() => expect(screen.getByText(/cannot answer yet/)).toBeTruthy());
  expect(screen.queryByText(/No pass has run/)).toBeNull();
});
