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

import { act, cleanup, fireEvent, poll, render as rtlRender, screen } from "~/test/inCase.ts";
import type { ReactElement } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CATALOG, SetupPasses, useSetup, useSetupRun, useSetupRuns } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { isLogRefusal, LiveSocket, Store, type RestFailure } from "~/protocol/index.ts";
import type { SetupRun } from "~/protocol/types.ts";

/**
 * The client every read here is rendered under: each re-reads when the live
 * socket comes back, which is what its `closed` banner promises, so the case
 * that holds that moves this store's connection. Nothing dials.
 */
let store = new Store();

function render(ui: ReactElement) {
  return rtlRender(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>{ui}</Router>
    </ClientContext.Provider>,
  );
}

/** The hint behind a failure the engine wrote `503`, or "-" for any other. */
function hintOf(failure: RestFailure | null): number | string {
  const refusal = failure?.error === "unavailable" ? failure.refusal : null;
  return refusal !== null && isLogRefusal(refusal) ? refusal.retryAfter : "-";
}

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
  store = new Store();
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

/**
 * The listing as `code:hint:tools`, where tools is how many the listing holds.
 * A listing that never answered holds none, and its failure is what says that
 * none is not an answer.
 */
function Listing() {
  const setup = useSetup();
  return (
    <span data-testid="listing">
      {setup.loading
        ? "loading"
        : `${setup.failure?.error ?? "ok"}:${hintOf(setup.failure)}:${setup.byKey.size}`}
    </span>
  );
}

// NOTHING POLLS THE LISTING, so a node that said two seconds was asked again
// only when somebody changed tabs.
test("a listing the node could not read is read again when it says", async () => {
  answering({ [LISTING]: [unavailable(12), () => json({ tools: [] }, 200)] });
  render(<Listing />);
  await poll(() => expect(screen.getByTestId("listing").textContent).toBe("unavailable:12:0"));
  await wait(20_000);
  expect(gaps(LISTING)).toEqual([12]);
  expect(screen.getByTestId("listing").textContent).toBe("ok:-:0");
});

test("a listing the engine said waiting will not clear is not read again on a timer", async () => {
  answering({ [LISTING]: [unavailable()] });
  render(<Listing />);
  await poll(() => expect(screen.getByTestId("listing").textContent).toBe("unavailable:0:0"));
  await wait(600_000);
  expect(reads(LISTING)).toBe(1);
});

function History() {
  const { runs, failure } = useSetupRuns(["jira"]);
  return (
    <span data-testid="history">
      {runs.map((r) => r.state).join(",")}|{hintOf(failure)}
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
  await poll(() => expect(screen.getByTestId("history").textContent).toBe("running|-"));
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
  await poll(() => expect(reads(RUNS)).toBe(1));
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
  await poll(() => expect(reads(RUNS)).toBe(1));
  await wait(10_000);
  expect(gaps(RUNS)).toEqual([4, 4]);
});

function Pass() {
  const { run, failure } = useSetupRun("jira", "r1");
  return (
    <span data-testid="pass">
      {run?.state ?? "none"}|{hintOf(failure)}
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
  await poll(() => expect(screen.getByTestId("pass").textContent).toBe("running|-"));
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
  render(<SetupPasses entry={entry} kinds={["jira"]} />);
  await poll(() => expect(screen.getByText(/cannot answer yet/)).toBeTruthy());
  expect(screen.queryByText(/No pass has run/)).toBeNull();
});

// A PASS'S READING BELONGS TO THAT PASS. The detail under the list is drawn by
// one component for whichever row is open, and a failed read keeps what was
// read rather than blanking it — so a component that outlived the row it was
// opened for went on showing the first pass's findings under the second
// pass's row once the second one's read failed: a claim about the wrong pass,
// not a reading kept through a blip.
test("opening another pass never shows the last one's reading under it", async () => {
  const first: SetupRun = {
    ...run("running"),
    findings: [{ kind: "identity_missing", detail: "what the first pass found" }],
  };
  const second: SetupRun = { ...run("done"), run_id: "r2", started_at: "2026-09-30T11:00:00Z" };
  answering({
    [RUNS]: [() => json({ runs: [first, second], scope: "node-a" }, 200)],
    [ONE]: [() => json(first, 200)],
    [`${RUNS}/r2`]: [() => json({ error: "internal_error" }, 500)],
  });
  const entry = CATALOG.find((e) => e.surfaces.some((s) => s.key === "jira"))!;
  render(<SetupPasses entry={entry} kinds={["jira"]} />);
  // NEWEST FIRST, the grid's own order: the first pass, then the second.
  const rows = () => [...document.querySelectorAll<HTMLElement>(".grid-row")];
  await poll(() => expect(rows()).toHaveLength(2));

  fireEvent.click(rows()[0]!);
  await poll(() => expect(screen.getByText("what the first pass found")).toBeTruthy());

  fireEvent.click(rows()[1]!);
  await poll(() => expect(reads(`${RUNS}/r2`)).toBe(1));
  await wait(0);
  expect(screen.queryByText("what the first pass found")).toBeNull();
  // AND WHAT HAPPENED TO ITS OWN READ IS SAID, where it drew nothing at all.
  expect(screen.getByText(/tried to answer and failed/)).toBeTruthy();
});

// EVERY FAILURE IS SAID, not only the engine's `503`. These reads had learned
// to draw that one; anything else drew nothing (the listing, one pass) or a
// claim nobody made — a FIRST read of the history that failed was drawn as
// "No pass has run on this node", the very sentence a failed poll was stopped
// from blanking into.
test("a first read of the history that failed is said, never drawn as no passes", async () => {
  answering({ [RUNS]: [() => json({ error: "internal_error" }, 500)] });
  const entry = CATALOG.find((e) => e.surfaces.some((s) => s.key === "jira"))!;
  render(<SetupPasses entry={entry} kinds={["jira"]} />);
  await poll(() => expect(screen.getByText(/tried to answer and failed/)).toBeTruthy());
  expect(screen.queryByText(/No pass has run/)).toBeNull();
});

/** A request no answer came back to: the connection dropped on the way. */
const dropped = (): Response => {
  throw new TypeError("Failed to fetch");
};

test.each([
  ["a fault", () => json({ error: "internal_error" }, 500), "query_failed"],
  // NOT `closed`, which says the SOCKET went away: it routinely did not.
  ["a request that never arrived", dropped, "unanswered"],
  // NOR A FAULT ON THE NODE: a gateway wrote this, and the engine never did.
  [
    "a gateway's page",
    () => new Response("<html>Bad Gateway</html>", { status: 502 }),
    "unanswered",
  ],
])("a listing that failed with %s says so", async (_, answer, code) => {
  answering({ [LISTING]: [answer] });
  render(<Listing />);
  await poll(() => expect(screen.getByTestId("listing").textContent).toBe(`${code}:-:0`));
});

// A READ NO ANSWER CAME BACK TO IS ASKED AGAIN ON ITS OWN, with the live socket
// up the whole time. It was drawn `closed`, whose banner promises a read "once
// the socket is back", and only the socket coming back read it again — so with
// the socket up, a listing nothing polls stood failed until a reload. It backs
// off: a second, then two, then four.
test("a read that never arrived is asked again on its own while the socket stays up", async () => {
  act(() => store.setConnected(true));
  answering({ [LISTING]: [dropped, dropped, dropped, () => json({ tools: [] }, 200)] });
  render(<Listing />);
  await poll(() => expect(screen.getByTestId("listing").textContent).toBe("unanswered:-:0"));
  await wait(600_000);
  expect(gaps(LISTING)).toEqual([1, 2, 4]);
  expect(screen.getByTestId("listing").textContent).toBe("ok:-:0");
});

// AND THE SOCKET COMING BACK STILL ASKS AT ONCE, ahead of the backoff: it is the
// engine being reachable again, for each of these reads — the history, idle,
// would otherwise wait out a backoff, and one pass the same.
test.each([
  ["the listing", LISTING, () => render(<Listing />), () => json({ tools: [] }, 200)],
  ["the history", RUNS, () => render(<History />), listed("done")],
  ["one pass", ONE, () => render(<Pass />), () => json(run("done"), 200)],
])("%s that never arrived is read again the moment the socket comes back", async (...args) => {
  const [, path, mount, answer] = args;
  answering({ [path]: [dropped, answer] });
  mount();
  await poll(() => expect(reads(path)).toBe(1));
  await wait(500);
  expect(reads(path)).toBe(1);

  act(() => store.setConnected(true));
  await wait(0);
  expect(reads(path)).toBe(2);
});
