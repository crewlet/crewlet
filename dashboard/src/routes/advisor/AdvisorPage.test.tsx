/**
 * The Advisor's page over its routes, stubbed at `fetch`: findings ranked by
 * severity then cost then recency, each saying how fresh it is, its fix's
 * press, and a person's answers; the last run and Run now.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { AdvisorPage, goLiveSteps, runWords } from "./AdvisorPage.tsx";
import type { AdvisorFinding, AdvisorRunSummary } from "./types.ts";
import { ranked } from "./words.ts";

vi.mock("./AdvisorModel.tsx", () => ({ AdvisorModel: () => null }));
vi.mock("./GoLiveReview.tsx", () => ({ GoLiveReview: () => <p>Review what each agent can do</p> }));

const NOW = Date.now();
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();

let serial = 0;
function finding(patch: Partial<AdvisorFinding> = {}): AdvisorFinding {
  serial += 1;
  const id = `00000000-0000-4000-8000-${String(serial).padStart(12, "0")}`;
  return {
    id,
    key: `class.${serial}:seat:s${serial}`,
    class: `class.${serial}`,
    source: "rule",
    actor: "owner",
    area: "capacity",
    severity: "costs",
    subject: { kind: "seat", id: `s${serial}`, name: `Seat ${serial}` },
    title: `Finding ${serial}`,
    why: "Because the records say so.",
    explanation: null,
    state: "open",
    close_reason: null,
    ineffective: null,
    snoozed_until: null,
    opened_at: ago(60 * 24),
    last_confirmed_at: ago(4),
    closed_at: null,
    effect_due_at: null,
    confirmed: "fresh",
    revision: 1,
    seen_before: 0,
    impact: null,
    evidence: { window: { from: ago(60 * 24 * 7), to: ago(0) }, facts: [], records: [] },
    fix: { kind: "none", sentence: "Nothing to change by hand.", options: [] },
    ...patch,
  };
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function engine(findings: AdvisorFinding[], run: AdvisorRunSummary | null = null) {
  const sent: { method: string; path: string; body: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const url = new URL(input);
      const method = init.method ?? "GET";
      sent.push({
        method,
        path: url.pathname,
        body: init.body ? JSON.parse(String(init.body)) : null,
      });
      if (url.pathname === "/advisor/findings") {
        return json({
          findings: url.searchParams.get("state") === "open" ? findings : [],
          next_cursor: null,
          counts: { blocks: 0, costs: 0, note: 0 },
          last_run: run,
        });
      }
      if (url.pathname === "/advisor/runs/latest") return json(run);
      if (url.pathname === "/advisor/runs") return json({ run_id: "r2" }, 202);
      const decision = /^\/advisor\/findings\/([^/]+)\/decision$/.exec(url.pathname);
      if (decision) return json(findings.find((f) => f.id === decision[1]));
      return json({ error: "not_found" }, 404);
    }),
  );
  return sent;
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <AdvisorPage section="findings" />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  serial = 0;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// THE OWNER'S ORDER: the most costly severity first, then the largest
// measured cost, then the newest.
test("findings are ranked by severity, then measured cost, then recency", () => {
  const note = finding({ severity: "note", title: "note" });
  const oldCost = finding({ title: "old cost", opened_at: ago(600) });
  const newCost = finding({ title: "new cost", opened_at: ago(60) });
  const bigCost = finding({
    title: "big cost",
    opened_at: ago(6000),
    impact: { hours_per_week: 30, tokens_per_week: null },
  });
  const block = finding({ severity: "blocks", title: "block" });
  expect(ranked([note, oldCost, newCost, bigCost, block]).map((f) => f.title)).toEqual([
    "block",
    "big cost",
    "new cost",
    "old cost",
    "note",
  ]);
});

test("each row says how fresh it is: checked when, or not re-checked since, and what became of a fix", async () => {
  engine([
    finding({ title: "Fresh one", impact: { hours_per_week: 12, tokens_per_week: null } }),
    finding({ title: "Stale one", confirmed: "stale", last_confirmed_at: "2026-11-01T08:00:00Z" }),
    finding({
      title: "Applied one",
      state: "applied",
      effect_due_at: "2026-11-22T09:00:00Z",
      seen_before: 2,
    }),
  ]);
  mount();
  await screen.findByText("Fresh one");
  expect(screen.getByText(/^12 h a week · found .+ · checked 4 minutes ago$/)).toBeDefined();
  expect(screen.getByText(/not re-checked since/)).toBeDefined();
  expect(screen.getByText(/applied, checking its effect until/)).toBeDefined();
  expect(screen.getByText(/seen 2 times before/)).toBeDefined();
});

test("a finding the engine builds a change for offers Apply, which re-checks it first", async () => {
  const add = finding({
    title: "Add a seat",
    fix: {
      kind: "config_change",
      sentence: "Add a second seat.",
      options: [
        { label: "Add one", change: { entity: "units", id: "eng", set: {}, summary: "add" } },
      ],
    },
  });
  const { id } = add;
  const sent = engine([add]);
  mount();
  await screen.findByText("Add a seat");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  });
  // THE DIALOG OPENS ON A RE-CHECK, never on the finding as it was stored.
  await waitFor(() =>
    expect(
      sent.some((r) => r.method === "POST" && r.path === `/advisor/findings/${id}/recheck`),
    ).toBe(true),
  );
  expect(screen.getByRole("dialog")).toBeDefined();
});

test("Snooze and Dismiss send the person's answer and read the list again", async () => {
  const f = finding({ title: "Noisy alarm" });
  const sent = engine([f]);
  mount();
  await screen.findByText("Noisy alarm");
  fireEvent.click(screen.getByRole("button", { name: "More for Noisy alarm" }));
  await act(async () => {
    fireEvent.click(await screen.findByRole("menuitem", { name: "Dismiss: it's wrong" }));
  });
  await waitFor(() =>
    expect(sent.find((r) => r.path === `/advisor/findings/${f.id}/decision`)?.body).toEqual({
      decision: "dismiss",
      reason: "wrong",
    }),
  );
  const reads = sent.filter((r) => r.path === "/advisor/findings").length;
  await waitFor(() =>
    expect(sent.filter((r) => r.path === "/advisor/findings").length).toBeGreaterThan(reads - 1),
  );
});

test("what blocks the company brings the go-live check to the top", async () => {
  engine([finding({ severity: "blocks", title: "No model signs in" }), finding({ title: "Slow" })]);
  mount();
  const check = await screen
    .findByRole("list", { name: /Go-live check/ })
    .catch(() => screen.findByLabelText(/Go-live check/));
  expect(within(check as HTMLElement).getByText("Fix what blocks the company (1)")).toBeDefined();
  expect(screen.getByText("Review what each agent can do")).toBeDefined();
  expect(goLiveSteps([finding({ severity: "note" })]).map((s) => s.state)).toEqual([
    "done",
    "done",
    "waiting",
  ]);
});

test("the head says when the checks last ran, and Run now starts them", async () => {
  const run: AdvisorRunSummary = {
    run_id: "r1",
    started_at: ago(5),
    ended_at: ago(4),
    status: "done",
    classes_run: 30,
    opened: 2,
    resolved: 1,
    model: null,
  };
  const sent = engine([], run);
  mount();
  await screen.findByText("Checked 4 minutes ago. It found 2 new findings and closed 1.");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
  });
  await waitFor(() =>
    expect(sent.some((r) => r.method === "POST" && r.path === "/advisor/runs")).toBe(true),
  );
  expect(runWords(null, NOW)).toBe("The checks have not run yet.");
});
