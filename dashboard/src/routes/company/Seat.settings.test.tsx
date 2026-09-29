/**
 * The half of a seat only the org chart's own read carries, and what the
 * screen says when it cannot read it.
 *
 * `/org` IS ANONYMOUSLY READABLE, so the projection was narrowed to a charter
 * and a tree: `internal/api/orgprojection.go` spells the public shape out field
 * by field, and `orgprojection_test.go` fails the build over a field of
 * `config.Role` or `config.Unit` nobody has classified. A seat's model chain,
 * token budget, contact identities and tool credentials are on the other side
 * of that line: the RUNTIME half of its chart row, which `/chart` serves only
 * to a reader who may read the company's configuration, and says when it did
 * not (`runtime: false`).
 *
 * THREE THINGS GO WRONG IF A SCREEN FORGETS THAT, and all three are here:
 *
 *  - it reads the fields off the projection, where they are simply absent, and
 *    draws "not set" over settings it was never given — a statement about
 *    somebody's company, and the wrong one;
 *  - it keeps what a reader DID read after a later read is refused. That suits
 *    a poll and is wrong for a guarded read: the budget stayed on screen
 *    beside a banner saying the answer needs a grant;
 *  - it reads the runtime half WITHHELD as a runtime half that is empty.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatPeek, SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { fmtCount } from "~/lib/format.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
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

/** The anonymous projection, written out as `internal/api` emits it. */
const projection: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo" },
    // A HUMAN SEAT, because half of what this screen decides is decided by the
    // kind: the tab set, the overview's tiles, the Configured card's rows and
    // the tool-credential card all differ, and a fixture with only agents in it
    // can assert none of it.
    { name: "Ada Founder", handle: "ada", kind: "human", availability: "CET business hours" },
  ],
  units: [
    {
      name: "Engineering",
      type: "department",
      roles: [{ name: "Dev A" }],
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
    runtime: { token_budget: 250000, llm: ["fast", "backup"], llm_review: "big" },
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
      contact: { slack_user_id: "U0FOUNDER" },
      // AUTH_HEADER is what an engine whose redaction took any value
      // CONTAINING `${` for a reference sent: its literal half intact.
      mcp_env: {
        tracker: { API_TOKEN: "__redacted__", AUTH_HEADER: "Bearer sk-live-${SUFFIX}" },
      },
    },
  },
};

const units: Record<string, ChartUnit> = {
  engineering: {
    key: "engineering",
    name: "Engineering",
    // A WHOLE REFERENCE, which is the only unmasked form a credential field
    // ever carries: the engine masks a literal in `mcp_env` server-side.
    runtime: { mcp_env: { github: { GITHUB_HOST: "${ENGINEERING_GITHUB_HOST}" } } },
  },
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

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

function mount(
  hash: string,
  answer: (what: string) => Promise<unknown>,
  chart: () => ChartAnswerer = () => servingChart,
) {
  stubChart(chart);
  location.hash = hash;
  const store = new Store();
  store.applyOrg(projection);
  // THE RESOLVED ROWS, which the handshake and every config apply push to
  // every reader, token or not. A seat's recurring work is read from these
  // rather than from the `schedules:` it authored in the document — see the
  // derivation in Seat.tsx — so the fixture belongs on the store beside the
  // projection and not in the guarded answer.
  store.applySchedules({
    schedules: [
      {
        scope_type: "role",
        // THE ID IS NOT THE HANDLE. A role scope is keyed on the seat's
        // agent id so a rename keeps the schedule's history, and the handle
        // travels beside it as `scope_name` — so a screen that filtered or
        // rendered by the id would show this seat none of its own schedules.
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
      // A UNIT SCHEDULE THIS SEAT IS A RUNNER OF, which the authored read
      // could not see at all: it is not this seat's schedule and it still
      // lands in this seat's day.
      {
        scope_type: "unit",
        // A RENAMED UNIT: the fire ledger keys on the key it was created
        // under, and the screen reads the one it answers to now.
        scope_id: "eng",
        scope_name: "Engineering",
        name: "standup",
        cron: "0 9 * * 1-5",
        timezone: "UTC",
        task: "Post the standup",
        target: "",
        enabled: true,
        timeout_seconds: 0,
        catchup: false,
        runners: ["ceo"],
        next_run: new Date(Date.now() + 3_600_000).toISOString(),
      },
    ],
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = answer;
  const handle = hash.split("/")[3]!.split("?")[0]!;
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle={handle} />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, view };
}

const answering = (what: string) =>
  // THE SHAPE THE ENGINE ACTUALLY SENDS for a rollup. `totals` is not optional
  // on that answer, and the Cost tab reads it without a guard — so a fixture
  // answering `{}` for it crashed the tab rather than testing it.
  what === "tokens"
    ? Promise.resolve({ totals: { total_tokens: 0, calls: 0 }, by_model: [], by_turn: [] })
    : Promise.resolve({ llm_history: [], next: "" });

/** Let every settled promise land, and the renders they cause. */
async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

// READ BY THE HANDLE, from the chart, with the runtime half asked for: the
// company document holds no seats any more, and a name is prose two seats may
// share.
test("the settings the projection does not carry are read from the chart, by handle", async () => {
  mount("#/company/people/ceo", answering);

  // The chain, in the order the fallback walks it.
  expect(await screen.findByText("fast")).toBeDefined();
  expect(screen.getByText("backup")).toBeDefined();
  const read = chartReads.find((r) => r.path === "/chart/seats/ceo");
  expect(read?.query.get("runtime")).toBe("true");
  // A root seat has no home unit to read.
  expect(chartReads.some((r) => r.path.startsWith("/chart/units/"))).toBe(false);
});

// A SEAT'S ADDRESS IS SEALED, so the chart serves the reference that names it
// or the mask, and neither is shown as though it were an address.
test("a sealed address is shown as the reference it is, and a masked one as hidden", async () => {
  mount("#/company/people/ceo", answering);
  expect(await screen.findByText("${CHART_SEAT_CEO_EMAIL_0A1B2C3D}")).toBeDefined();
  cleanup();

  mount("#/company/people/ada", answering);
  expect(await screen.findByText("A literal value is set (hidden)")).toBeDefined();
  expect(document.body.textContent).not.toContain("__redacted__");
});

// A REFUSAL CLEARS WHAT AN EARLIER READ SHOWED.
test("a refused re-read takes the guarded settings off the page", async () => {
  let refuse = false;
  const { store } = mount("#/company/people/ceo", answering, () =>
    refuse ? refusing : servingChart,
  );
  expect(await screen.findByText("fast")).toBeDefined();

  refuse = true;
  // An org push is what follows a chart write, and it re-reads the chart.
  act(() => store.applyOrg({ ...projection }));

  await screen.findByRole("tab", { name: "Cost" });
  await settle();
  expect(screen.queryByText("fast")).toBeNull();

  // AND THE RECURRING WORK STAYS, which is the half that must NOT be cleared.
  // It comes from the pushed rows, which every reader gets, so a refused read
  // takes the settings that read bought and nothing else.
  fireEvent.click(screen.getByRole("tab", { name: "Schedules" }));
  expect(screen.getByText("weekly-review")).toBeDefined();

  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.queryByText(fmtCount(250000))).toBeNull();
  // AND SAYS SO. "Unknown" is not "unlimited": a cap nobody was allowed to
  // read and a company with no cap are different facts.
  expect(screen.getByText("Unknown")).toBeDefined();
});

// A RUNTIME HALF WITHHELD IS NOT A RUNTIME HALF THAT IS EMPTY. The chart
// serves the rows without it to a reader who may not read the configuration,
// and says so; a screen that drew "unlimited" or "default provider" over that
// would be describing a company it was never shown.
test("a runtime half the chart withheld is said to be withheld, never drawn as empty", async () => {
  mount("#/company/people/ceo", answering, () => strippingChart);
  await settle();
  expect(headerFacts()).toContain("not shown to you");
  expect(headerFacts()).not.toContain("default provider");
  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.queryByText("unlimited")).toBeNull();
  expect(screen.getByText("Unknown")).toBeDefined();
  cleanup();

  // The control: the same seat served whole names its chain and its cap.
  mount("#/company/people/ceo", answering);
  await settle();
  expect(headerFacts()).toContain("fast → backup");
});

// A CREDENTIAL FIELD SHOWS A WHOLE REFERENCE OR NOTHING, whatever the engine
// sent — see `configValueKind`.
test("a credential is never printed, and a reference is shown as the name it is", async () => {
  mount("#/company/people/dev-a?tab=access", answering);

  expect(await screen.findByText("U0FOUNDER")).toBeDefined();
  // The mask says only that something is set, and a credential field that is
  // not one whole reference is hidden the same way.
  expect(screen.getAllByText("A literal value is set (hidden)").length).toBe(2);
  expect(document.body.textContent).not.toContain("__redacted__");
  expect(document.body.textContent).not.toContain("sk-live");
  // What the home unit gives the seat is merged in, the seat's own winning —
  // and a reference NAMES a secret rather than being one, so it is shown. The
  // unit is read by its KEY, which is what the seat's row names it by.
  expect(await screen.findByText("${ENGINEERING_GITHUB_HOST}")).toBeDefined();
  expect(chartReads.some((r) => r.path === "/chart/units/engineering")).toBe(true);
});

// A UNIT SCHEDULE IS THIS SEAT'S RECURRING WORK TOO.
//
// The old derivation read the `schedules:` a seat AUTHORED out of the company
// document, so it could only ever find the rows this seat declared. A unit
// schedule reaches every seat in the unit and `runners` is the engine's own
// resolved answer to whose day it lands in — so the rows that actually wake a
// seat were exactly the ones the page could not show. Nothing was marked
// missing; the card simply listed fewer schedules than the engine would fire.
test("a unit schedule this seat runs is on its schedules tab", async () => {
  mount("#/company/people/ceo?tab=schedules", answering);

  expect(await screen.findByText("standup")).toBeDefined();
  // Named as the unit's rather than as this seat's own, because whose
  // schedule it is decides who can change it.
  expect(screen.getByText("Engineering")).toBeDefined();
  // And the seat's own is still there beside it.
  expect(screen.getByText("weekly-review")).toBeDefined();
});

// A SCHEDULE THAT CANNOT FIRE SAYS SO, which is the other half the authored
// read had no way to carry: a `ScheduleSpec` is name, cron and task, and the
// engine's own row carries the effective timezone, the `next_run` it worked
// out, and `problem` when a cron or a zone cannot be read. A schedule that
// will never fire looked exactly like one firing tomorrow.
test("a schedule the engine cannot fire is marked, not left blank", async () => {
  const { store } = mount("#/company/people/ceo?tab=schedules", answering);
  await screen.findByText("weekly-review");

  act(() => {
    store.applySchedules({
      schedules: [
        {
          scope_type: "role",
          scope_id: "b9f8fba1-4fe4-522f-8349-9f28db43654f",
          scope_name: "ceo",
          name: "weekly-review",
          cron: "0 9 * * 1",
          timezone: "Mars/Olympus",
          task: "Review the week",
          target: "",
          enabled: true,
          timeout_seconds: 0,
          catchup: false,
          runners: ["ceo"],
          next_run: "",
          problem: "unknown timezone Mars/Olympus",
        },
      ],
    });
  });

  expect(await screen.findByText("cannot fire")).toBeDefined();
});

// THE WORK TAB IS TWO READS, AND ONLY ONE OF THEM IS ANYBODY'S.
//
// `work_items {assignee}` is ungated: every reader of this page gets the list
// of what is open on this seat, and that is the floor. `work_my_work` is
// scoped by the engine to the seat the caller's own credential is bound to —
// the same rule the person record follows — so the blocks built on it are for
// a person reading their own page and for an operator.
//
// The failure this guards against is the quiet one: asking anyway and drawing
// the refusal, so a colleague's page reads as a seat with an empty queue
// rather than as somebody else's queue that is not theirs to read.
test("a reader without the credential gets the open list and not the private queue", async () => {
  let askedMyWork = false;
  mount("#/company/people/ceo?tab=work", (what) => {
    if (what === "work_my_work") {
      askedMyWork = true;
      return Promise.resolve({ priorities: [], asked_of_me: [] });
    }
    if (what === "work_items") {
      return Promise.resolve({
        items: [
          { id: "t1", key: "ENG-1", title: "Ship the thing", status: "in_progress", type: "task" },
        ],
      });
    }
    if (what === "viewer") return Promise.resolve({ login: "", grants: [], handle: "" });
    return answering(what);
  });

  // The floor, which needs no credential.
  expect(await screen.findByText("Ship the thing")).toBeDefined();
  // And the honest sentence where the scoped blocks would be.
  expect(screen.getByText(/theirs to read/i)).toBeDefined();
  // NOT ASKED AT ALL. Asking and rendering the refusal is the shape this
  // avoids: the answer is not "no queue", it is "not yours".
  expect(askedMyWork).toBe(false);
});

/**
 * The header's fact line, which renders ABOVE the tab strip on every tab.
 *
 * Read off the DOM rather than through `getByText`, because the Overview panel's
 * own `ModelChain` renders the same keys and normalises to the same string — so
 * a text query would match two elements on one tab and one on the others, and
 * pass for the wrong reason.
 */
function headerFacts(): string {
  return (document.querySelector(".object-head .fact-line")?.textContent ?? "")
    .replace(/\s+/g, " ")
    .trim();
}

// THE HEADER IS A PROPERTY OF THE SEAT, NOT OF THE OPEN TAB.
//
// The guarded read was gated on `tab === "overview" | "cost" | "access"` and
// the header that reads the model chain out of it renders on all eight tabs. So a reader WITH a token was shown the chain on three and told
// "needs an operator token" on Work, Turns, Conversations, Memory and Schedules
// — the same seat, the same header, one click apart.
test("the model fact is the same on every tab", async () => {
  mount("#/company/people/ceo", answering);
  await settle();
  expect(headerFacts()).toContain("fast → backup");

  for (const name of [
    "Work",
    "Turns",
    "Conversations",
    "Memory",
    "Cost",
    "Access",
    "Schedules",
    "Overview",
  ]) {
    fireEvent.click(screen.getByRole("tab", { name }));
    await settle();
    expect(headerFacts(), name).toContain("fast → backup");
    expect(headerFacts(), name).not.toContain("needs an operator token");
  }
});

// A REFUSED READ NAMES THE GRANT IT NAMED, AND A NODE CATCHING UP IS NOT A
// REFUSAL.
//
// Every failed read of the seat's settings printed "needs an operator token":
// to a person signed in without the grant, who lacks a grant rather than a
// token, and to every reader of every seat while a node was still catching up
// after a restart — a claim about the READER made by an answer about the NODE.
test("the model fact names a refusal's grant, and an unavailable read claims nothing about the reader", async () => {
  mount("#/company/people/ceo", answering, () => refusing);
  await settle();
  expect(headerFacts()).toContain("needs state:read");
  expect(headerFacts()).not.toContain("operator token");
  cleanup();

  mount(
    "#/company/people/ceo",
    answering,
    () => () => json({ error: "unavailable", message: "Try again shortly." }, 503),
  );
  await settle();
  expect(headerFacts()).toContain("could not be read just now");
  expect(headerFacts()).not.toContain("needs");
});

// THE RAIL ASKS NOBODY, SO IT CLAIMS NOTHING.
//
// `SeatPeek` reads nothing guarded on purpose — a per-peek fetch of the whole
// company document would make every `[`/`]` step through a list an
// operator-gated read. It passed a null role for that, and the fact line
// rendered the null as "needs an operator token", so a rail that had asked
// nobody told every reader, token or not, that they were missing one.
test("the seat rail states no model rather than claiming a missing token", async () => {
  const store = new Store();
  store.applyOrg(projection);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.reject(new Error("the rail asks nothing guarded"));
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatPeek handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  await settle();

  expect(screen.queryByText(/needs an operator token/)).toBeNull();
  // AND NO MODEL ROW AT ALL: `FactLine` drops a fact whose value is empty, the
  // same rule a human seat's runtime follows.
  expect(headerFacts()).not.toContain("Model");
});

// A HUMAN SEAT'S CONFIGURED PANEL CARRIES NO MODEL ROW.
//
// `org.Role.humanForbidden` refuses `llm` and every per-phase chain on a human
// seat, so those rows could only ever draw their own fallbacks: "default
// provider" is a MODEL for a seat that runs none, and "none, reflection uses the
// default" is a reflection pass that never happens — both on the one card whose
// whole job is to say what somebody chose.
test("a human seat's configured panel carries no model row", async () => {
  mount("#/company/people/ada", answering);
  await settle();
  // The address row is there, for either kind.
  expect(screen.getByText("A literal value is set (hidden)")).toBeTruthy();
  expect(screen.queryByText("default provider")).toBeNull();
  expect(screen.queryByText("none, reflection uses the default")).toBeNull();
  expect(screen.queryByText("Auxiliary model")).toBeNull();
  expect(screen.getByText(/A human seat runs no model/)).toBeTruthy();
});

// AND ITS OVERVIEW MEASURES NO RUNTIME, and asks for no phase history.
test("a human seat shows no spend or turn tile, and asks for no phase history", async () => {
  let askedAgent = false;
  mount("#/company/people/ada", (what: string) => {
    if (what === "agent") askedAgent = true;
    return answering(what);
  });
  await settle();
  expect(screen.queryByText("Turns in the record")).toBeNull();
  expect(screen.queryByText(/^Tokens/)).toBeNull();
  expect(askedAgent).toBe(false);
  // NOT EMPTIED WHOLESALE: the two tiles that describe a seat of any kind stay.
  expect(screen.getByText("Direct reports")).toBeTruthy();
});

// AND ITS ACCESS TAB HAS NO TOOL-CREDENTIAL CARD. `mcp_env` is refused on a
// human seat and a human member inherits none of its unit's, so that card could
// only ever draw an empty state whose sentence — "this seat uses whatever the
// shared MCP servers were configured with" — is false of a seat that runs no
// tools at all.
test("a human seat's access tab has no tool-credential card", async () => {
  mount("#/company/people/ada?tab=access", answering);
  await settle();
  expect(screen.getByText("U0ADA")).toBeTruthy();
  expect(screen.queryByText("Tool credentials")).toBeNull();
  expect(screen.queryByText("No per-seat tool credentials")).toBeNull();
});
