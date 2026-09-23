/**
 * The half of a seat only the company document holds, and what the screen says
 * when it cannot read it.
 *
 * `/org` IS ANONYMOUSLY READABLE, so the projection was narrowed to a charter
 * and a tree: `internal/api/orgprojection.go` spells the public shape out field
 * by field, and `orgprojection_test.go` fails the build over a field of
 * `config.Role` or `config.Unit` nobody has classified. A seat's email, model
 * chain, token budget, contact identities and tool credentials are on the other
 * side of that line, behind the operator-gated `config` query.
 *
 * TWO THINGS GO WRONG IF A SCREEN FORGETS THAT, and both are here:
 *
 *  - it reads the fields off the projection, where they are simply absent, and
 *    draws "not set" over settings it was never given — a statement about
 *    somebody's company, and the wrong one;
 *  - it keeps what a token DID read after that token is cleared or refused,
 *    because `useQuery` holds its last good answer through a failed ask. That
 *    suits a poll and is wrong for a guarded read: the email and the budget
 *    stayed on screen beside a banner saying the answer needs a token.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatPeek, SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { fmtCount } from "~/lib/format.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CompanyDocument, OrgProjection } from "~/protocol/index.ts";

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

/**
 * The company document as the guarded surface serves it: REDACTED, so a
 * credential field holds a whole `${VAR}` or the mask, never a value.
 */
const document_: CompanyDocument = {
  name: "Acme",
  roles: [
    {
      name: "CEO",
      handle: "ceo",
      email: "ceo@example.com",
      token_budget: { day: 250000, month: 5000000 },
      llm: { default: ["fast", "backup"], review: "big" },
      schedules: [{ name: "weekly-review", cron: "0 9 * * 1", task: "Review the week" }],
    },
    {
      name: "Ada Founder",
      handle: "ada",
      kind: "human",
      email: "ada@example.com",
      contact: { slack_user_id: "U0ADA" },
    },
  ],
  units: [
    {
      name: "Engineering",
      // A WHOLE REFERENCE, which is the only unmasked form a credential field
      // ever carries: the engine redacts a literal in `mcp_env` server-side.
      mcp_env: { github: { GITHUB_HOST: "${ENGINEERING_GITHUB_HOST}" } },
      roles: [
        {
          name: "Dev A",
          contact: { slack_user_id: "U0FOUNDER" },
          // AUTH_HEADER is what an engine whose redaction took any value
          // CONTAINING `${` for a reference sent: its literal half intact.
          mcp_env: {
            tracker: { API_TOKEN: "__redacted__", AUTH_HEADER: "Bearer sk-live-${SUFFIX}" },
          },
        },
      ],
    },
  ],
};

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(hash: string, answer: (what: string) => Promise<unknown>) {
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
        scope_id: "ceo",
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
        scope_id: "Engineering",
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
  what === "config"
    ? Promise.resolve(document_)
    : // THE SHAPE THE ENGINE ACTUALLY SENDS for a rollup. `totals` is not
      // optional on that answer, and the Cost tab reads it without a guard — so
      // a fixture answering `{}` for it crashed the tab rather than testing it.
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

test("the settings the projection does not carry are read from the document", async () => {
  mount("#/company/people/ceo", answering);

  expect(await screen.findByText("ceo@example.com")).toBeDefined();
  // The chain, in the order the fallback walks it, out of the mapping form —
  // which is the shape that used to throw during render and blank the page.
  expect(screen.getByText("fast")).toBeDefined();
  expect(screen.getByText("backup")).toBeDefined();
});

// A TILE HOLDS ONE NUMBER, AND IT IS THE TIGHTEST WINDOW: the day here, not
// the month the seat also caps, because the day is what stops it first and
// the one the counter holds it to. The caption says there is more than one,
// rather than listing them on a line that ellipsizes.
test("the configured budget tile shows the tightest window it caps", async () => {
  mount("#/company/people/ceo", answering);
  expect(await screen.findByText("ceo@example.com")).toBeDefined();

  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.getByText(`${fmtCount(250000)} a day`)).toBeDefined();
  expect(screen.queryByText(new RegExp(`${fmtCount(5000000)} a month`))).toBeNull();
  expect(
    screen.getByText("the tightest of 2 windows token_budget caps on this role"),
  ).toBeDefined();
});

// A REFUSAL CLEARS WHAT THE TOKEN HAD READ.
test("a refused re-read takes the guarded settings off the page", async () => {
  let refuse = false;
  const { store } = mount("#/company/people/ceo", (what) =>
    what === "config" && refuse ? Promise.reject(new Error("unauthorized")) : answering(what),
  );
  expect(await screen.findByText("ceo@example.com")).toBeDefined();

  refuse = true;
  // A reconnect asks every query again, as a token change does.
  act(() => store.setConnected(true));

  await screen.findByRole("tab", { name: "Cost" });
  expect(screen.queryByText("ceo@example.com")).toBeNull();

  // AND THE RECURRING WORK STAYS, which is the half that must NOT be cleared.
  // It was read out of the guarded document and went with the email; it comes
  // from the pushed rows now, which every reader gets, so a refused token
  // takes the settings the token bought and nothing else.
  fireEvent.click(screen.getByRole("tab", { name: "Schedules" }));
  expect(screen.getByText("weekly-review")).toBeDefined();

  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.queryByText(`${fmtCount(250000)} a day`)).toBeNull();
  // AND SAYS SO. "Unknown" is not "unlimited": a cap nobody was allowed to
  // read and a company with no cap are different facts.
  expect(screen.getByText("Unknown")).toBeDefined();
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
  // and a reference NAMES a secret rather than being one, so it is shown.
  expect(screen.getByText("${ENGINEERING_GITHUB_HOST}")).toBeDefined();
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
          scope_id: "ceo",
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
    if (what === "viewer") return Promise.resolve({ operator_id: "", operator: false, handle: "" });
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
// The guarded document read was gated on `tab === "overview" | "cost" |
// "access"` and the header that reads the model chain out of it renders on all
// eight tabs. So a reader WITH a token was shown the chain on three and told
// "needs an operator token" on Work, Turns, Conversations, Memory and Schedules
// — the same seat, the same header, one click apart.
test("the model fact is the same on every tab", async () => {
  mount("#/company/people/ceo", answering);
  await settle();
  expect(headerFacts()).toContain("fast → backup → big");

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
    expect(headerFacts(), name).toContain("fast → backup → big");
    expect(headerFacts(), name).not.toContain("needs an operator token");
  }
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
  expect(screen.getByText("ada@example.com")).toBeTruthy();
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
