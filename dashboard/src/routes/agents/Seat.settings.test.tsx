/**
 * The half of a seat only the org chart's own read carries, and what the
 * profile says when it cannot read it.
 *
 * THE ORG PROJECTION WAS NARROWED TO A CHARTER AND A TREE:
 * `internal/api/orgprojection.go` spells the public shape out field by field.
 * A seat's model chain, token budget, contact identities and tool credentials
 * are on the other side of that line — the RUNTIME half of its chart row,
 * which `/chart` serves only to a reader who may read the company's
 * configuration (`config:read`), and says when it did not (`runtime: false`).
 * The company document holds no seats any more, so a profile that read the
 * seat from it found nothing for every seat in every company.
 *
 * THREE THINGS GO WRONG IF A SCREEN FORGETS THAT, and all three are here:
 *
 *  - it draws "not set" over settings it was never given — a statement about
 *    somebody's company, and the wrong one;
 *  - it keeps what a reader DID read after a later read is refused, which
 *    suits a poll and is wrong for a guarded read;
 *  - it reads the runtime half WITHHELD as a runtime half that is empty.
 *
 * And every outcome of the read claims only what it knows: a refusal names the
 * grant the engine named, and a node that could not answer claims nothing
 * about the reader.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type {
  ChartAnswer,
  ChartSeat,
  ChartSeatRead,
  ChartUnit,
  ChartUnitRead,
  OrgProjection,
} from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The projection, written out as `internal/api` emits it. */
const projection: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo" },
    // A HUMAN SEAT, because half of what the profile decides is decided by the
    // kind: the tab set and the Settings tab's cards both differ.
    { name: "Ada Founder", handle: "ada", kind: "human", availability: "CET business hours" },
  ],
  units: [
    {
      id: "engineering",
      name: "Engineering",
      type: "department",
      roles: [{ name: "Dev A", handle: "dev-a" }],
    },
  ],
  derived: {
    seats: [
      {
        handle: "ceo",
        name: "CEO",
        kind: "agent",
        placed_by_ref: false,
        manager: "",
        managers: null,
        reports: ["dev-a"],
        auto_reports: null,
        onboarding_chain: null,
      },
      {
        handle: "ada",
        name: "Ada Founder",
        kind: "human",
        placed_by_ref: false,
        manager: "",
        managers: null,
        reports: null,
        auto_reports: null,
        onboarding_chain: null,
      },
      {
        handle: "dev-a",
        name: "Dev A",
        kind: "agent",
        placed_by_ref: false,
        manager: "ceo",
        managers: ["ceo"],
        reports: null,
        auto_reports: null,
        onboarding_chain: null,
      },
    ],
    units: [
      {
        id: "engineering",
        name: "Engineering",
        type: "department",
        lead: "",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["dev-a"],
      },
    ],
  },
};

const answer: ChartAnswer = { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:10" };

/**
 * The chart's rows as `GET /chart/seats/{handle}?runtime=true` serves them:
 * MASKED, so a credential field — and a seat's address — holds a whole
 * `${VAR}` reference or the mask, never a value.
 */
const seats: Record<string, ChartSeat> = {
  ceo: {
    handle: "ceo",
    name: "CEO",
    // A SEALED ADDRESS: the chart keeps it in the secret store and serves the
    // reference that names it.
    email: "${CHART_SEAT_CEO_EMAIL_0A1B2C3D}",
    runtime: { token_budget: { week: 250_000 }, llm: ["fast", "backup"], llm_review: "big" },
  },
  ada: {
    handle: "ada",
    name: "Ada Founder",
    kind: "human",
    // The mask: an address is set, and this reader is not shown it.
    email: "__redacted__",
    runtime: { contact: { slack_user_id: "U0ADA" }, availability: "CET business hours" },
  },
  "dev-a": {
    handle: "dev-a",
    name: "Dev A",
    unit: "engineering",
    runtime: {
      mcp_env: { tracker: { API_TOKEN: "__redacted__" } },
    },
  },
};

const units: Record<string, ChartUnit> = {
  engineering: {
    key: "engineering",
    name: "Engineering",
    runtime: { mcp_env: { github: { GITHUB_HOST: "${ENGINEERING_GITHUB_HOST}" } } },
  },
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

/** How the engine answers one chart read. */
type ChartAnswerer = (path: string, query: URLSearchParams) => Response;

/** The chart serving every row whole, as it does a reader holding `config:read`. */
const servingChart: ChartAnswerer = (path, query) => {
  const runtime = query.get("runtime") === "true";
  const seat = /^\/chart\/seats\/([^/]+)$/.exec(path);
  if (seat) {
    const row = seats[decodeURIComponent(seat[1]!)];
    if (!row) return json({ error: "not_found", message: "Nothing by that name." }, 404);
    const body: ChartSeatRead = { seat: row, manages: null, answer, runtime };
    return json(body);
  }
  const unit = /^\/chart\/units\/([^/]+)$/.exec(path);
  if (unit) {
    const row = units[decodeURIComponent(unit[1]!)];
    if (!row) return json({ error: "not_found", message: "Nothing by that name." }, 404);
    const body: ChartUnitRead = { unit: row, children: [], seats: [], answer, runtime };
    return json(body);
  }
  return json({ error: "not_found", message: "Nothing by that name." }, 404);
};

/** The chart serving the rows WITHOUT their runtime half, and saying so. */
const strippingChart: ChartAnswerer = (path, query) => {
  const whole = servingChart(path, query);
  const strip = ({ runtime: _runtime, ...row }: { runtime?: unknown }) => row;
  const seat = /^\/chart\/seats\/([^/]+)$/.exec(path);
  const row = seat ? seats[decodeURIComponent(seat[1]!)] : undefined;
  if (!row) return whole;
  return json({ seat: strip(row), manages: null, answer, runtime: false });
};

/** A refusal on authority, as the router renders the table's own. */
const refusing: ChartAnswerer = () =>
  json(
    {
      error: "unauthorized",
      message: "You may not do that.",
      reason: "no_grant",
      grants: ["state:read"],
    },
    403,
  );

/** A reader holding the grant the runtime half is served under. */
const CONFIG_READER = {
  login: "ada.founder",
  grants: ["state:read", "config:read"],
  handle: "ada",
  name: "Ada Founder",
  owner: "ada",
  acts: [],
};

/**
 * What the engine answers the questions the profile asks beside the chart —
 * each in the shape the wire carries, so a case about the guarded half is not
 * a case about another card reading a malformed answer.
 */
const ANSWERS: Record<string, unknown> = {
  viewer: CONFIG_READER,
  work_items: { items: [], total_hint: 0 },
  work_inbox: { handle: "ada", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  turns: { turns: [], next: null },
};

let chartReads: { path: string; query: URLSearchParams }[] = [];

/** Installs the engine's REST half as `fetch`, answering the chart reads. */
function stubChart(answerer: () => ChartAnswerer) {
  chartReads = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://engine.test");
      if (url.pathname.startsWith("/chart/")) {
        chartReads.push({ path: url.pathname, query: url.searchParams });
        return answerer()(url.pathname, url.searchParams);
      }
      return json({});
    }),
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(hash: string, chart: () => ChartAnswerer = () => servingChart) {
  stubChart(chart);
  location.hash = hash;
  const store = new Store();
  store.applyOrg(projection);
  store.setConnected(true);
  // THE RESOLVED ROWS, which the handshake and every config apply push to
  // every reader. A seat's recurring work is read from these rather than from
  // the `schedules:` its runtime half authored, so a refusal of the chart read
  // must leave them standing.
  store.applySchedules({
    schedules: [
      {
        scope_type: "role",
        // THE ID IS NOT THE HANDLE: a role scope is keyed on the seat's agent
        // id, and the handle travels beside it as `scope_name`.
        scope_id: "b9f8fba1-4fe4-522f-8349-9f28db43654f",
        scope_name: "ceo",
        name: "weekly-review",
        cron: "0 9 * * 1",
        timezone: "UTC",
        task: "Review the week",
        target: "",
        enabled: true,
        timeout_seconds: 0,
        catchup: false,
        runners: ["ceo"],
        next_run: new Date(Date.now() + 86_400_000).toISOString(),
      },
    ],
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(ANSWERS[what] ?? {});
  const handle = hash.split("/")[3]!.split("?")[0]!;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <InboxCountsProvider>
            <SeatScreen handle={handle} />
          </InboxCountsProvider>
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
  return { store };
}

/** Let every settled promise land, and the renders they cause. */
async function settle() {
  for (let i = 0; i < 6; i++) await act(async () => Promise.resolve());
}

/** The Overview's Setup card. */
function setupCard(): HTMLElement {
  return screen.getByText("Setup").closest(".crewlet-card") as HTMLElement;
}

// READ BY THE HANDLE, from the chart, with the runtime half asked for: the
// company document holds no seats any more, and a name is prose two seats may
// share.
test("the settings the projection does not carry are read from the chart, by handle", async () => {
  mount("#/agents/seats/ceo?tab=settings");
  await settle();
  // The chain, in the order the fallback walks it.
  expect(screen.getByText("fast")).toBeTruthy();
  expect(screen.getByText("backup")).toBeTruthy();
  const read = chartReads.find((r) => r.path === "/chart/seats/ceo");
  expect(read?.query.get("runtime")).toBe("true");
  // A root seat has no home unit to read.
  expect(chartReads.some((r) => r.path.startsWith("/chart/units/"))).toBe(false);
});

// A SEAT'S ADDRESS IS SEALED, so the chart serves the reference that names it
// or the mask, and neither is shown as though it were an address.
test("a sealed address is shown as the reference it is, and a masked one as hidden", async () => {
  mount("#/agents/seats/ceo?tab=settings");
  await settle();
  expect(screen.getByText("${CHART_SEAT_CEO_EMAIL_0A1B2C3D}")).toBeTruthy();
  cleanup();

  mount("#/agents/seats/ada?tab=settings");
  await settle();
  expect(screen.getByText("A literal value is set (hidden)")).toBeTruthy();
  expect(document.body.textContent).not.toContain("__redacted__");
});

// WHAT A SEAT'S UNIT GIVES IT IS MERGED IN, the seat's own winning, and the
// unit is read by its KEY — which is what the seat's row names it by.
test("a seat's tool credentials merge its unit's, read by the unit's key", async () => {
  mount("#/agents/seats/dev-a?tab=settings");
  await settle();
  expect(screen.getByText("API_TOKEN")).toBeTruthy();
  expect(screen.getByText("GITHUB_HOST")).toBeTruthy();
  expect(chartReads.some((r) => r.path === "/chart/units/engineering")).toBe(true);
  expect(document.body.textContent).not.toContain("__redacted__");
});

// A REFUSAL CLEARS WHAT AN EARLIER READ SHOWED.
test("a refused re-read takes the guarded settings off the page", async () => {
  let refuse = false;
  const { store } = mount("#/agents/seats/ceo?tab=settings", () =>
    refuse ? refusing : servingChart,
  );
  await settle();
  expect(screen.getByText("fast")).toBeTruthy();

  refuse = true;
  // An org push is what follows a chart write, and it re-reads the chart.
  act(() => store.applyOrg({ ...projection }));
  await settle();
  expect(screen.queryByText("fast")).toBeNull();

  // AND THE RECURRING WORK STAYS, which is the half that must NOT be cleared:
  // it comes from the pushed rows, which every reader gets.
  fireEvent.click(screen.getByRole("tab", { name: "Schedules" }));
  expect(screen.getByText("weekly-review")).toBeTruthy();
});

// A RUNTIME HALF WITHHELD IS NOT A RUNTIME HALF THAT IS EMPTY. The chart
// serves the rows without it to a reader it will not show it to, and says so;
// a profile that drew "uncapped" or "the company's default" over that would
// be describing a company it was never shown.
test("a runtime half the chart withheld is said to be withheld, never drawn as empty", async () => {
  mount("#/agents/seats/ceo?tab=settings", () => strippingChart);
  await settle();
  expect(
    screen.getAllByText("This seat's runtime half was not shown to you").length,
  ).toBeGreaterThan(0);
  expect(screen.queryByText("uncapped")).toBeNull();
  expect(screen.queryByText(/the company’s default/)).toBeNull();

  fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
  await settle();
  expect(setupCard().textContent).toContain("was not shown to you");
  expect(setupCard().textContent).not.toContain("not offered");
});

// A REFUSED READ NAMES THE GRANT IT NAMED, AND A NODE CATCHING UP IS NOT A
// REFUSAL. Every failed read of a seat's settings printed "needs an operator
// token": to a person signed in without the grant, who lacks a grant rather
// than a token, and to every reader of every seat while a node was still
// catching up after a restart — a claim about the READER made by an answer
// about the NODE.
test("the setup card names a refusal's grant, and an unavailable read claims nothing about the reader", async () => {
  mount("#/agents/seats/ceo", () => refusing);
  await settle();
  expect(setupCard().textContent).toContain("needs state:read");
  expect(setupCard().textContent).not.toContain("operator token");
  cleanup();

  mount(
    "#/agents/seats/ceo",
    () => () => json({ error: "unavailable", message: "Try again shortly." }, 503),
  );
  await settle();
  expect(setupCard().textContent).toContain("could not be read just now");
  expect(setupCard().textContent).not.toContain("needs");
});

// A FAILED CHART READ SAYS WHICH FAILURE IT MET, in the banner every read
// draws. A bespoke "The engine did not answer. This panel fills in when it
// does." was drawn over every one of them: over a `500` the engine DID answer,
// and over a request nothing answered that nothing was going to ask again
// until the next org push.
test("the settings tab says which failure the chart read met", async () => {
  mount(
    "#/agents/seats/ceo?tab=settings",
    () => () => json({ error: "internal_error", message: "It broke." }, 500),
  );
  await settle();
  expect(screen.getAllByText(/tried to answer and failed/).length).toBeGreaterThan(0);
  expect(screen.queryByText(/fills in when it does/)).toBeNull();
  cleanup();

  mount("#/agents/seats/ceo?tab=settings", () => () => {
    throw new TypeError("Failed to fetch");
  });
  await settle();
  expect(screen.getAllByText(/No answer from the engine reached this page/).length).toBeGreaterThan(
    0,
  );
  expect(screen.queryByText(/fills in when it does/)).toBeNull();
});
