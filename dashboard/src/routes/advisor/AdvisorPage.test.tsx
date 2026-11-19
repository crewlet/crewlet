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
import {
  AdvisorPage,
  type AdvisorSection,
  goLiveSteps,
  pagedOn,
  runWords,
} from "./AdvisorPage.tsx";
import type { AdvisorKeptFinding, AdvisorRunSummary } from "~/protocol/advisorFindings.ts";
import { ranked } from "./words.ts";

vi.mock("./AdvisorModel.tsx", () => ({ AdvisorModel: () => null }));
vi.mock("./GoLiveReview.tsx", () => ({ GoLiveReview: () => <p>Review what each agent can do</p> }));

const NOW = Date.now();
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();

let serial = 0;
function finding(patch: Partial<AdvisorKeptFinding> = {}): AdvisorKeptFinding {
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
    subject: {
      kind: "seat",
      id: `s${serial}`,
      name: `Seat ${serial}`,
      seat: `s${serial}`,
      unit: "",
      schedule: "",
    },
    title: `Finding ${serial}`,
    why: "Because the records say so.",
    item: "",
    names: [],
    path: "",
    entries: null,
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

/**
 * The engine as the page meets it: the reads are its questions over the
 * socket, the presses requests over `fetch`, every one recorded.
 */
function engine(findings: AdvisorKeptFinding[], run: AdvisorRunSummary | null = null) {
  const sent: { method: string; path: string; body: unknown }[] = [];
  const asked: { name: string; params: Record<string, unknown> }[] = [];
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
      if (url.pathname === "/advisor/runs") return json({ run_id: "r2" }, 202);
      const decision = /^\/advisor\/findings\/([^/]+)\/decision$/.exec(url.pathname);
      if (decision) return json(findings.find((f) => f.id === decision[1]));
      return json({ error: "not_found" }, 404);
    }),
  );
  const answer = (name: string, params: Record<string, unknown>) => {
    asked.push({ name, params });
    if (name === "advisor_findings") {
      return {
        findings: params.state === "closed" ? [] : findings,
        next_cursor: null,
        counts: { blocks: 0, costs: 0, note: 0 },
        last_run: run,
      };
    }
    if (name === "advisor_run") return run;
    return {};
  };
  return { sent, asked, answer };
}

function mount(
  answer: (name: string, params: Record<string, unknown>) => unknown,
  section: AdvisorSection = "findings",
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (name: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (name, params) => Promise.resolve(answer(name, params ?? {}));
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <AdvisorPage section={section} />
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
  const { answer } = engine([
    finding({ title: "Fresh one", impact: { hours_per_week: 12, tokens_per_week: null } }),
    finding({ title: "Stale one", confirmed: "stale", last_confirmed_at: "2026-11-01T08:00:00Z" }),
    finding({
      title: "Applied one",
      state: "applied",
      effect_due_at: "2026-11-22T09:00:00Z",
      seen_before: 2,
    }),
  ]);
  mount(answer);
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
  const { sent, answer } = engine([add]);
  mount(answer);
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
  const { sent, asked, answer } = engine([f]);
  mount(answer);
  await screen.findByText("Noisy alarm");
  const reads = () => asked.filter((q) => q.name === "advisor_findings").length;
  const before = reads();
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
  // THE LIST IS READ AGAIN after the answer lands.
  await waitFor(() => expect(reads()).toBeGreaterThan(before));
});

test("what blocks the company brings the go-live check to the top", async () => {
  const { answer } = engine([
    finding({ severity: "blocks", title: "No model signs in" }),
    finding({ title: "Slow" }),
  ]);
  mount(answer);
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
    trigger: "schedule",
    by: "",
    unread: [],
    started_at: ago(5),
    ended_at: ago(4),
    status: "done",
    classes_run: 30,
    opened: 2,
    resolved: 1,
    model: null,
  };
  const { sent, answer } = engine([], run);
  mount(answer);
  await screen.findByText("Checked 4 minutes ago. It found 2 new findings and closed 1.");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
  });
  await waitFor(() =>
    expect(sent.some((r) => r.method === "POST" && r.path === "/advisor/runs")).toBe(true),
  );
  expect(runWords(null, NOW)).toBe("The checks have not run yet.");
});

// R5: AN ATLAS FIX IS OFFERED ON THE ENTRIES THE ENGINE NAMES, only to a reader
// it names them to (a holder of the atlas app); anybody else is sent to the
// atlas.
test("an atlas finding offers its fix where the engine named its entries, and the atlas where not", async () => {
  const act = {
    kind: "atlas_act",
    sentence: "Name the seat that answers for each.",
    options: [],
    act: { tool: "atlas_set_owner", asks: "owner" },
  };
  const named = finding({
    title: "3 services have no owner",
    fix: act,
    entries: [{ id: "e1", kind: "service", key: "checkout", name: "Checkout" }],
  });
  const unnamed = finding({ title: "2 sources are stale", fix: act, entries: null });
  const { answer } = engine([named, unnamed]);
  mount(answer);
  await screen.findByText("3 services have no owner");
  expect(screen.getAllByRole("button", { name: "Review the fix" })).toHaveLength(1);
  expect(screen.getByRole("link", { name: "Open the atlas" }).getAttribute("href")).toBe(
    "#/company/atlas",
  );
});

// THE REPORTS TAB lists each week's report, newest first, with what the week
// moved; each opens its week's page, and an older page is asked for by the
// cursor the engine answered.
test("the Reports tab lists each week and asks for older ones by the engine's cursor", async () => {
  const { asked, answer } = engine([]);
  const week = (n: number) => ({
    week: `2026-W${n}`,
    period: { from: "2026-11-16T00:00:00Z", to: "2026-11-23T00:00:00Z" },
    written_at: ago(60),
    opened: n === 47 ? 3 : 0,
    resolved: 1,
    applied: 0,
    dismissed: 2,
  });
  const reports = (name: string, params: Record<string, unknown>) => {
    const said = answer(name, params);
    if (name !== "advisor_reports") return said;
    return params.cursor === "c2"
      ? { reports: [week(45)], next_cursor: null }
      : { reports: [week(47), week(46)], next_cursor: "c2" };
  };
  mount(reports, "reports");
  const list = await screen.findByRole("list", { name: "Weekly reports" });
  const first = within(list).getByRole("link", { name: "Week 47 of 2026" });
  expect(first.getAttribute("href")).toBe("#/advisor/reports/2026-W47");
  expect(within(list).getAllByText(/3 opened · 1 resolved · 0 applied · 2 dismissed/)).toHaveLength(
    1,
  );
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Show older reports" }));
  });
  await within(list).findByRole("link", { name: "Week 45 of 2026" });
  // THE WEEKS ALREADY SHOWN STAY, the older page after them.
  expect(
    within(list)
      .getAllByRole("link")
      .map((a) => a.textContent),
  ).toEqual(["Week 47 of 2026", "Week 46 of 2026", "Week 45 of 2026"]);
  expect(asked.filter((q) => q.name === "advisor_reports").map((q) => q.params.cursor)).toEqual([
    undefined,
    "c2",
  ]);
  expect(screen.queryByRole("button", { name: "Show older reports" })).toBeNull();
});

test("the Reports tab says when no report was written yet", async () => {
  const { answer } = engine([]);
  mount((name, params) => {
    const said = answer(name, params);
    return name === "advisor_reports" ? { reports: [], next_cursor: null } : said;
  }, "reports");
  expect(await screen.findByText("No report yet")).toBeDefined();
});

// A PAGE STILL HELD while the next is asked for is not drawn twice.
test("a page's rows already held are drawn once", () => {
  const held = [{ id: "a" }, { id: "b" }];
  expect(pagedOn(held, held, (r) => r.id)).toEqual(held);
  expect(pagedOn(held, [{ id: "b" }, { id: "c" }], (r) => r.id).map((r) => r.id)).toEqual([
    "a",
    "b",
    "c",
  ]);
  expect(pagedOn([], undefined, (r: { id: string }) => r.id)).toEqual([]);
});
