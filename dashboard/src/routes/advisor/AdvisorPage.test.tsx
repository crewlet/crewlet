/**
 * The Advisor's page over its routes, stubbed at `fetch`: findings ranked by
 * severity then cost then recency, each saying how fresh it is, its fix's
 * press, and a person's answers; the last run and Run now.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { AssistantControlContext, type AssistantControl } from "~/app/assistant/control.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { AdvisorPage, type AdvisorSection, pagedOn } from "./AdvisorPage.tsx";
import type { AdvisorKeptFinding, AdvisorRunSummary } from "~/protocol/advisorFindings.ts";
import { ranked } from "./words.ts";
import { runView, runWords } from "./Run.tsx";

vi.mock("./GoLiveReview.tsx", () => ({ GoLiveReview: () => <p>What each agent can do</p> }));

// A FIXED MIDDAY: a row words its instants by the reader's calendar day, so a
// fixture taken from the real clock read "3 days ago" for two days near
// midnight. Every case's clock is held here (beforeEach).
const NOW = Date.parse("2026-06-10T12:00:00Z");
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
    headline: patch.title ?? `Finding ${serial}`,
    stats: [],
    chart: null,
    seats: [],
    actions: { primary: null, secondary: [] },
    premise_checked_at: null,
    ...patch,
  };
}

/** The company's numbers over the range, as `advisor_snapshot` answers them. */
const SNAPSHOT = {
  as_of: ago(0),
  run_id: null,
  range: { from: ago(60 * 24 * 7), to: ago(0), days: 7 },
  stats: [
    { key: "open_items", label: "Open items", value: 15, unit: "items", was: 12, better: "down" },
    { key: "turns", label: "Turns", value: 40, unit: "turns", was: null, better: null },
    {
      key: "wasted_turns_percent",
      label: "Wasted",
      value: 75,
      unit: "percent",
      was: null,
      better: "down",
    },
  ],
  charts: [
    {
      id: "wasted_wakes",
      title: "Turns that changed nothing, by what woke them",
      unit: "turns",
      kind: "bars",
      x: {
        kind: "reason",
        keys: ["work_item:lead_fallback"],
        labels: ["Work items (the unit's lead)"],
      },
      series: [
        { id: "nothing", label: "Changed nothing", values: [30] },
        { id: "moved", label: "Moved work", values: [10] },
      ],
      note: null,
    },
  ],
  unread: [],
};

/** A press of the engine's, every key sent. */
const press = (patch: Record<string, unknown>) => ({
  kind: "apply",
  label: "",
  option: null,
  items: null,
  to: null,
  page: null,
  target: null,
  setting: null,
  url: null,
  question: null,
  ...patch,
});

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
    if (name === "advisor_snapshot") return SNAPSHOT;
    return {};
  };
  return { sent, asked, answer };
}

/** An assistant the reader may ask, recording what each control was handed. */
function assistant() {
  const about: { question: string; send: boolean }[] = [];
  const asked: string[] = [];
  const control: AssistantControl = {
    open: false,
    unavailable: null,
    toggle: () => {},
    ask: (q) => asked.push(q),
    askAbout: (question, { send }) => about.push({ question, send }),
  };
  return { control, about, asked };
}

function mount(
  answer: (name: string, params: Record<string, unknown>) => unknown,
  section: AdvisorSection = "findings",
  control: AssistantControl = assistant().control,
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
        <AssistantControlContext.Provider value={control}>
          <ToastProvider>
            <LayerHost>
              <AdvisorPage section={section} />
            </LayerHost>
          </ToastProvider>
        </AssistantControlContext.Provider>
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  serial = 0;
  vi.useFakeTimers({ now: NOW, toFake: ["Date"] });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
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

// A ROW IS ITS NUMBERS AND WHAT BECAME OF IT; when it was found and checked
// is behind Details.
test("each row says its numbers and what became of it, and Details when it was checked", async () => {
  const { answer } = engine([
    finding({
      title: "Fresh one",
      stats: [
        { key: "h", label: "Hours a week", value: 12, unit: "hours", was: null, better: null },
      ],
    }),
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
  expect(screen.getByText(/Hours a week: 12 h/)).toBeDefined();
  expect(screen.getByText(/not re-checked since/)).toBeDefined();
  expect(screen.getByText(/applied, checking its effect until/)).toBeDefined();
  expect(screen.getByText(/seen 2 times before/)).toBeDefined();
  expect(screen.queryByText(/checked 4 minutes ago/)).toBeNull();
  const fresh = screen
    .getAllByRole("listitem")
    .find((li) => li.textContent?.includes("Fresh one"))!;
  fireEvent.click(within(fresh).getByRole("button", { name: /Details/ }));
  expect(await within(fresh).findByText(/checked 4 minutes ago/)).toBeDefined();
});

test("a fix's press says what it does and opens its Apply in place, on a re-check", async () => {
  const add = finding({
    title: "Add a seat",
    fix: {
      kind: "config_change",
      sentence: "Add a second seat.",
      options: [
        { label: "Add one", change: { entity: "units", id: "eng", set: {}, summary: "add" } },
      ],
    },
    actions: { primary: press({ kind: "apply", label: "Add one", option: 0 }), secondary: [] },
  });
  const { id } = add;
  const { sent, answer } = engine([add]);
  mount(answer);
  await screen.findByText("Add a seat");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Add one" }));
  });
  // APPLY OPENS IN PLACE, under its row, on a re-check, never on the finding
  // as it was stored, and never in a dialog over the list.
  await waitFor(() =>
    expect(
      sent.some((r) => r.method === "POST" && r.path === `/advisor/findings/${id}/recheck`),
    ).toBe(true),
  );
  const panel = screen.getByRole("region", { name: "Apply: Add one" });
  expect(screen.getAllByRole("listitem").find((li) => li.contains(panel))).toBeDefined();
  expect(screen.queryByRole("dialog")).toBeNull();
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

test("while anything blocks, the review of each agent's access is offered after the findings", async () => {
  const { answer } = engine([
    finding({ severity: "blocks", title: "No model signs in" }),
    finding({ title: "Slow" }),
  ]);
  mount(answer);
  await screen.findByText("No model signs in");
  expect(screen.getByText("What each agent can do")).toBeDefined();
  expect(screen.queryByText(/Go-live check/)).toBeNull();
});

test("the head says when the checks last ran, and Run starts them", async () => {
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
    kind: "rules",
    progress: [],
    budget: null,
    stopped_by: null,
    classes: [],
  };
  const { sent, answer } = engine([], run);
  mount(answer);
  await screen.findByText("Checked 4 minutes ago");
  const head = document.querySelector<HTMLElement>(".advisor-head__run")!;
  expect(head.textContent).toContain("2 new");
  expect(head.textContent).toContain("1 closed");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
  });
  await waitFor(() =>
    expect(sent.some((r) => r.method === "POST" && r.path === "/advisor/runs")).toBe(true),
  );
  expect(sent.find((r) => r.path === "/advisor/runs")?.body).toEqual({});
  expect(runWords(runView(null), NOW)).toBe("The checks have not run yet.");
});

// ONE PRESS, NO KIND TO CHOOSE: the head offers Run alone, and the press asks
// for one run, the checks and then the investigation, naming no kind.
test("the head offers one Run, which asks for one run naming no kind", async () => {
  const { sent, answer } = engine([], null);
  mount(answer);
  const press = await screen.findByRole("button", { name: "Run" });
  expect(screen.queryByRole("button", { name: "Run deep" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Run now" })).toBeNull();
  await act(async () => {
    fireEvent.click(press);
  });
  await waitFor(() =>
    expect(sent.find((r) => r.method === "POST" && r.path === "/advisor/runs")?.body).toEqual({}),
  );
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
  const actions = { primary: press({ kind: "fix", label: "Review the fix" }), secondary: [] };
  const named = finding({
    title: "3 services have no owner",
    fix: act,
    entries: [{ id: "e1", kind: "service", key: "checkout", name: "Checkout" }],
    actions,
  });
  const unnamed = finding({ title: "2 sources are stale", fix: act, entries: null, actions });
  const { answer } = engine([named, unnamed]);
  mount(answer);
  await screen.findByText("3 services have no owner");
  expect(screen.getAllByRole("button", { name: "Review the fix" })).toHaveLength(1);
  expect(screen.queryByRole("link", { name: "Open the atlas" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "More for 2 sources are stale" }));
  expect(await screen.findByRole("menuitem", { name: "Open the atlas" })).toBeDefined();
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

// "CLOSED 0" SAID NOTHING: a run says what it moved, and only that.
test("the run line says only what the run moved", () => {
  const run = (opened: number, resolved: number): AdvisorRunSummary => ({
    run_id: "r",
    trigger: "schedule",
    by: "",
    unread: [],
    started_at: ago(7),
    ended_at: ago(6),
    status: "done",
    classes_run: 30,
    opened,
    resolved,
    model: null,
    kind: "rules",
    progress: [],
    budget: null,
    stopped_by: null,
    classes: [],
  });
  expect(runWords(runView(run(1, 0)), NOW)).toBe(
    "All checks ran 6 minutes ago. It found 1 new finding.",
  );
  expect(runWords(runView(run(0, 2)), NOW)).toBe(
    "All checks ran 6 minutes ago. It closed 2 findings.",
  );
  expect(runWords(runView(run(0, 0)), NOW)).toBe("All checks ran 6 minutes ago.");
});

// THE HEAD'S PLACEMENT IS ON A BOX OF OUR OWN: on the kit's Text, our rule
// tied with the kit's own (min-width: 0), and whichever chunk's style loaded
// last won, which squeezed the line to a word a line at 390 px.
test("the run line's placement is on the page's own box, never on the kit's Text", async () => {
  const { answer } = engine([]);
  mount(answer);
  await screen.findByText("Not run yet");
  const box = document.querySelector(".advisor-head__run")!;
  expect(box.classList.contains("crewlet-text")).toBe(false);
  expect(box.textContent).toContain("Not run yet");
  expect(document.querySelector(".crewlet-text.advisor-head__run")).toBeNull();
});

// WHAT TO CHANGE COMES BEFORE THE REVIEW: the review of what each agent can
// do stood between the go-live check and the findings, and pushed the first
// finding below the fold.
test("the review of each agent's access comes after what to change", async () => {
  const { answer } = engine([finding({ severity: "blocks", title: "No model signs in" })]);
  mount(answer);
  const list = await screen.findByText("What to change");
  const review = screen.getByText("What each agent can do");
  expect(list.compareDocumentPosition(review) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

// A SNOOZE IS SEEN AND CAN BE ENDED: the finding leaves What to change for a
// list of its own that says until when, and its menu ends the snooze.
test("a snoozed finding is listed apart, says until when, and can be woken", async () => {
  const later = new Date(NOW + 6 * 86_400_000).toISOString();
  const quiet = finding({ title: "Quiet one", snoozed_until: later });
  const { sent, answer } = engine([finding({ title: "Loud one" }), quiet]);
  mount(answer);
  const snoozedList = await screen
    .findByRole("region", { name: /Snoozed/ })
    .catch(() => screen.findByLabelText(/Snoozed/));
  expect(within(snoozedList as HTMLElement).getByText("Quiet one")).toBeDefined();
  expect(within(snoozedList as HTMLElement).getByText(/snoozed until/)).toBeDefined();
  const change = screen.getByText("What to change").closest("section, [role=region], div")!;
  expect(within(change as HTMLElement).queryByText("Quiet one")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "More for Quiet one" }));
  await act(async () => {
    fireEvent.click(await screen.findByRole("menuitem", { name: "End the snooze" }));
  });
  await waitFor(() =>
    expect(sent.find((r) => r.path === `/advisor/findings/${quiet.id}/decision`)?.body).toEqual({
      decision: "reopen",
    }),
  );
});

// A ROW'S TIMES ARE READ, NOT LOGGED: no seconds, and no year of this one.
test("a row's instants carry no seconds and no year", async () => {
  const { answer } = engine([
    finding({ title: "Stale", confirmed: "stale", last_confirmed_at: ago(60 * 26) }),
    finding({
      title: "Applied",
      state: "applied",
      effect_due_at: new Date(NOW + 20 * 3_600_000).toISOString(),
      ineffective: { fix_digest: "x", applied_at: ago(60 * 50) },
    }),
  ]);
  mount(answer);
  await screen.findByText("Stale");
  const metas = [
    screen.getByText(/not re-checked since/).textContent!,
    screen.getByText(/checking its effect until/).textContent!,
  ];
  for (const meta of metas) {
    expect(meta).not.toMatch(/\d{1,2}:\d{2}:\d{2}/);
    expect(meta).not.toMatch(new RegExp(String(new Date(NOW).getFullYear())));
  }
  expect(screen.getByText(/a change applied 2 days ago did not clear it/)).toBeDefined();
});

// ONE CONDITION ON MANY SEATS IS ONE ROW, each seat inside it with its own
// fix: seven seats were seven rows of the same sentence.
test("findings of one class are one row, each subject inside it with its own fix", async () => {
  const fix: AdvisorKeptFinding["fix"] = {
    kind: "config_change",
    sentence: "Run learning on a smaller model.",
    options: [
      { label: "Learn on Haiku", change: { entity: "roles", id: "x", set: {}, summary: "s" } },
    ],
  };
  const same = (n: number) =>
    finding({
      class: "learning.auxiliary_model",
      key: `learning.auxiliary_model:seat:s${n}`,
      title: "Agents that learn on their main model.",
      severity: "note",
      fix,
      actions: {
        primary: press({ kind: "apply", label: "Learn on Haiku", option: 0 }),
        secondary: [],
      },
    });
  const { answer } = engine([same(1), same(2), same(3), finding({ title: "Alone" })]);
  mount(answer);
  await screen.findByText("Alone");
  const lines = [...document.querySelectorAll(".crewlet-attention-list__line")].map(
    (l) => l.textContent,
  );
  expect(lines.filter((l) => l?.includes("Agents that learn on their main model."))).toEqual([
    "Worth knowing: 3 seats: Agents that learn on their main model.",
  ]);
  fireEvent.click(screen.getAllByRole("button", { name: /Details/ })[1]!);
  const inside = await screen.findByRole("list", { name: "Each of the 3 seats" });
  expect(
    within(inside)
      .getAllByRole("link")
      .map((a) => a.textContent),
  ).toEqual(["Seat 1", "Seat 2", "Seat 3"]);
  expect(within(inside).getByRole("button", { name: "Learn on Haiku for Seat 2" })).toBeDefined();
  expect(within(inside).getByRole("button", { name: /More for Seat 3/ })).toBeDefined();
});

// ASK CREWLET ABOUT THIS opens the finding's own page with the question in the
// box of a new conversation, for the person to send: never sent for them,
// never asked in the conversation open, and never naming the finding by its
// id, which the page's context names.
test("Ask Crewlet about this drafts a question on the finding's page, never sends it", async () => {
  const f = finding({ title: "Slow reviews" });
  const { answer } = engine([f]);
  const { control, about, asked } = assistant();
  mount(answer, "findings", control);
  await screen.findByText("Slow reviews");
  fireEvent.click(screen.getByRole("button", { name: "More for Slow reviews" }));
  await act(async () => {
    fireEvent.click(await screen.findByRole("menuitem", { name: "Ask Crewlet about this" }));
  });
  expect(asked).toEqual([]);
  expect(about).toHaveLength(1);
  expect(about[0]!.send).toBe(false);
  expect(about[0]!.question).not.toContain(f.id);
  expect(window.location.hash).toBe(`#/advisor/findings/${f.id}`);
});

// AN ASK ACTION'S OWN QUESTION is the one put in the box, not the plain one
// every finding is offered.
test("a finding's ask action drafts its own question", async () => {
  const question = "Is the round cap of 300 too low for this seat's reviews?";
  const f = finding({
    title: "Capped runs",
    actions: {
      primary: {
        kind: "ask",
        label: "Ask about the cap",
        option: null,
        items: null,
        to: null,
        page: null,
        target: null,
        setting: null,
        url: null,
        question,
      },
      secondary: [],
    },
  });
  const { answer } = engine([f]);
  const { control, about } = assistant();
  mount(answer, "findings", control);
  await screen.findByText("Capped runs");
  // A PRESS THAT CHANGES NOTHING IS NO BUTTON ON A ROW: it is in its menu.
  expect(screen.queryByRole("button", { name: /Ask about the cap/ })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "More for Capped runs" }));
  await act(async () => {
    fireEvent.click(await screen.findByRole("menuitem", { name: "Ask about the cap" }));
  });
  expect(about).toEqual([{ question, send: false }]);
});

// A DISMISSAL CAN BE TAKEN BACK at once, from its toast.
test("a dismissal's toast offers Undo, which reopens it", async () => {
  const f = finding({ title: "Wrong one" });
  const { sent, answer } = engine([f]);
  mount(answer);
  await screen.findByText("Wrong one");
  fireEvent.click(screen.getByRole("button", { name: "More for Wrong one" }));
  await act(async () => {
    fireEvent.click(await screen.findByRole("menuitem", { name: "Dismiss: it's wrong" }));
  });
  await act(async () => {
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));
  });
  await waitFor(() =>
    expect(
      sent.filter((r) => r.path === `/advisor/findings/${f.id}/decision`).map((r) => r.body),
    ).toEqual([{ decision: "dismiss", reason: "wrong" }, { decision: "reopen" }]),
  );
});

// THE HEAD NAMES NO MODEL: the Advisor's model is chosen in Settings › Models & Fleets,
// and the page neither says it nor reads it (no model list, no company
// document), so it offers no model line, no link to it and no picker.
test("the head shows no model line, no model link and no picker, and reads no model", async () => {
  const { asked, answer } = engine([finding({ title: "Slow" })]);
  mount(answer);
  await screen.findByText("Slow");
  const head = document.querySelector<HTMLElement>(".advisor-head")!;
  expect(head.textContent).not.toMatch(/Explains with|No model explains|Model settings/);
  expect(within(head).queryByRole("link")).toBeNull();
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(document.querySelector('a[href="#/settings/models"]')).toBeNull();
  expect(asked.map((q) => q.name)).not.toContain("llm_entries");
  expect(asked.map((q) => q.name)).not.toContain("config");
});

// THE PAGE OPENS ON THE NUMBERS: each figure against the previous range, and
// linked to the open finding that explains it.
test("the numbers open the page, each linked to the finding that explains it", async () => {
  const waste = finding({ class: "wake.noop_share", title: "Turns that changed nothing" });
  const { answer } = engine([waste]);
  mount(answer);
  const strip = await screen.findByLabelText("The company's numbers");
  const wakes = strip.querySelector<HTMLElement>('[data-stat="wasted_turns_percent"]')!;
  expect(within(wakes).getByText("75%")).toBeDefined();
  expect(
    within(wakes)
      .getByRole("link", { name: "Why: Turns that changed nothing" })
      .getAttribute("href"),
  ).toBe(`#/advisor/findings/${waste.id}`);
  const open = strip.querySelector<HTMLElement>('[data-stat="open_items"]')!;
  expect(within(open).getByText("15")).toBeDefined();
  expect(within(open).queryByRole("link")).toBeNull();
  expect(screen.getByText("Turns that changed nothing, by what woke them")).toBeDefined();
});

test("the range picker asks the numbers for its range", async () => {
  const { asked, answer } = engine([]);
  mount(answer);
  await screen.findByLabelText("The company's numbers");
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: "30 days" }));
  });
  await waitFor(() =>
    expect(asked.filter((q) => q.name === "advisor_snapshot").map((q) => q.params.range)).toContain(
      "30d",
    ),
  );
});

// THE PAGE DOES NOT MOVE AS IT LOADS: the lists under the numbers wait for the
// numbers' first answer, the charts holding their room meanwhile, so nothing
// already drawn is pushed down when the charts land. Mutation: draw the lists
// before the numbers answer, and they appear above charts still to come.
test("the lists wait for the numbers, which hold their room while they are read", async () => {
  let numbers: (value: unknown) => void = () => {};
  const { answer } = engine([finding({ title: "Work waits on one seat" })]);
  mount((name, params) =>
    name === "advisor_snapshot"
      ? new Promise((resolve) => {
          numbers = resolve;
        })
      : answer(name, params),
  );
  await waitFor(() =>
    expect(document.querySelector(".advisor-charts[aria-busy='true']")).not.toBeNull(),
  );
  expect(screen.queryByText("Work waits on one seat")).toBeNull();
  await act(async () => {
    numbers(SNAPSHOT);
  });
  expect(await screen.findByText("Work waits on one seat")).toBeDefined();
  expect(document.querySelector(".advisor-charts[aria-busy='true']")).toBeNull();
});

// HISTORY READS BY DAY, ONE ROW A FINDING: the time, the title on a line of
// its own, what it was about and why it closed under it, and how it ended as
// a pill, the newest day first.
test("history groups closed findings by the day they closed, each with its outcome", async () => {
  const closedToday = finding({
    title: "A run ended on its round cap after 251 rounds",
    state: "resolved",
    close_reason: "condition_cleared",
    closed_at: new Date(NOW - 3_600_000).toISOString(),
  });
  const closedEarlier = finding({
    title: "A seat holds most of its job's open work",
    state: "dismissed",
    close_reason: "dismissed_wrong",
    closed_at: new Date(NOW - 3 * 86_400_000).toISOString(),
  });
  const { answer } = engine([]);
  mount(
    (name, params) =>
      name === "advisor_findings" && params.state === "closed"
        ? { findings: [closedEarlier, closedToday], next_cursor: null, counts: {}, last_run: null }
        : answer(name, params),
    "history",
  );
  const today = await screen.findByRole("region", { name: "Today" });
  expect(today.textContent).toContain("A run ended on its round cap after 251 rounds");
  expect(today.textContent).toContain("The condition cleared");
  expect(today.textContent).toContain("Resolved");
  const days = screen.getAllByRole("region").filter((r) => r.tagName === "SECTION");
  expect(days.map((d) => d.getAttribute("aria-label"))[0]).toBe("Today");
  expect(days[1]!.textContent).toContain("Dismissed as wrong");
  expect(
    screen
      .getByRole("link", { name: /A seat holds most of its job's open work/ })
      .getAttribute("href"),
  ).toContain(`advisor/findings/${closedEarlier.id}`);
});
