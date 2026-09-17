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

import { SeatScreen } from "./Seat.tsx";
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
  roles: [{ name: "CEO", handle: "ceo" }],
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
      token_budget: 250000,
      llm: { default: ["fast", "backup"], review: "big" },
      schedules: [{ name: "weekly-review", cron: "0 9 * * 1", task: "Review the week" }],
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
  what === "config" ? Promise.resolve(document_) : Promise.resolve({ llm_history: [], next: "" });

test("the settings the projection does not carry are read from the document", async () => {
  mount("#/company/people/ceo", answering);

  expect(await screen.findByText("ceo@example.com")).toBeDefined();
  expect(screen.getByText("weekly-review")).toBeDefined();
  // The chain, in the order the fallback walks it, out of the mapping form —
  // which is the shape that used to throw during render and blank the page.
  expect(screen.getByText("fast")).toBeDefined();
  expect(screen.getByText("backup")).toBeDefined();
});

// A REFUSAL CLEARS WHAT THE TOKEN HAD READ.
test("a refused re-read takes the guarded settings off the page", async () => {
  let refuse = false;
  const { store } = mount("#/company/people/ceo", (what) =>
    what === "config" && refuse ? Promise.reject(new Error("unauthorized")) : answering(what),
  );
  expect(await screen.findByText("ceo@example.com")).toBeDefined();
  expect(screen.getByText("weekly-review")).toBeDefined();

  refuse = true;
  // A reconnect asks every query again, as a token change does.
  act(() => store.setConnected(true));

  await screen.findByRole("tab", { name: "Cost" });
  expect(screen.queryByText("ceo@example.com")).toBeNull();
  expect(screen.queryByText("weekly-review")).toBeNull();

  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.queryByText(fmtCount(250000))).toBeNull();
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
