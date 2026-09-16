/**
 * The seat screen draws both of its sources honestly.
 *
 * Who a seat is comes from the anonymous org projection; how it is configured
 * comes from the operator-gated company document. Both fixtures below are
 * written out field for field from their emitters (`internal/api`'s
 * `OrgProjection` with its public `derived` block, and the redacted
 * `config.Company`), because the failures this guards were shapes: a per-phase
 * `llm` mapping reached React as an object child and blanked the page, and a
 * credential field is exactly where a careless render would print a mask as if
 * it were a value.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { fmtCount } from "~/lib/format.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CompanyDocument, OrgProjection } from "~/protocol/index.ts";

const projection: OrgProjection = {
  name: "Acme",
  roles: [{ name: "CEO", handle: "ceo", goal: "Set direction", manages: ["Engineering"] }],
  units: [{ name: "Engineering", lead: "Dev A", roles: [{ name: "Dev A", goal: "Ship" }] }],
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
        onboarding_chain: ["Engineering"],
      },
    ],
    units: [
      {
        name: "Engineering",
        type: "team",
        lead: "dev-a",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["dev-a"],
      },
    ],
  },
};

const companyDoc: CompanyDocument = {
  name: "Acme",
  providers: { llm: { fast: {}, big: {}, backup: {} } },
  roles: [
    {
      name: "CEO",
      handle: "ceo",
      email: "ceo@example.com",
      llm: { default: ["fast", "backup"], review: "big" },
      llm_auxiliary: "fast",
      token_budget: 250000,
      schedules: [{ name: "weekly-review", cron: "0 9 * * 1", task: "Review the week" }],
    },
  ],
  units: [
    {
      name: "Engineering",
      mcp_env: { github: { GITHUB_TOKEN: "${ENGINEERING_GITHUB_TOKEN}" } },
      roles: [
        {
          name: "Dev A",
          contact: {},
          // AUTH_HEADER is what an engine whose redaction took any value
          // containing `${` for a reference sent: its literal half intact.
          mcp_env: {
            tracker: { API_TOKEN: "__redacted__", AUTH_HEADER: "Bearer sk-live-${SUFFIX}" },
          },
          integrations: {
            slack: {
              bot_token: "${DEV_A_SLACK_BOT_TOKEN}",
              signing_secret: "__redacted__",
              channel: "C0DEV",
            },
            jira: { project: "ENG" },
          },
        },
      ],
    },
  ],
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mountWithStore(
  hash: string,
  answer: (what: string) => Promise<unknown>,
  org = projection,
) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = answer;
  const handle = hash.split("/")[2]!.split("?")[0]!;
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle={handle} />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, view };
}

const mount = (hash: string, answer: (what: string) => Promise<unknown>, org = projection) =>
  mountWithStore(hash, answer, org).view;

const answering = (what: string) =>
  what === "config" ? Promise.resolve(companyDoc) : Promise.resolve({ llm_history: [], next: "" });

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("a per-phase model mapping renders as phase and chain, not as a crash", async () => {
  mount("#/seats/ceo", answering);
  // The whole page used to go blank here: an object is not a React child.
  expect(await screen.findByText("fast, then backup")).toBeDefined();
  expect(screen.getByText("big")).toBeDefined();
  expect(screen.getByText(/Default:/)).toBeDefined();
  expect(screen.getByText(/Review:/)).toBeDefined();
  expect(screen.getByText("ceo@example.com")).toBeDefined();
  // A flat override is listed as the field it is, not merged into a guess.
  expect(screen.getByText("llm_auxiliary")).toBeDefined();
});

test("reporting lines come from the engine's derived hierarchy", async () => {
  mount("#/seats/dev-a", answering);
  expect(await screen.findByText("Reports to")).toBeDefined();
  const chips = screen.getAllByRole("link", { name: "CEO" });
  expect(chips[0]?.getAttribute("href")).toBe("#/seats/ceo");
});

test("a credential is never printed, and a reference is shown as the name it is", async () => {
  mount("#/seats/dev-a?tab=access", answering);
  expect(await screen.findByText("${DEV_A_SLACK_BOT_TOKEN}")).toBeDefined();
  // The mask says only that something is set. It is never the value, and a
  // credential field that is not one whole reference is hidden the same way.
  expect(screen.getAllByText("A literal value is set (hidden)").length).toBe(3);
  expect(document.body.textContent).not.toContain("__redacted__");
  expect(document.body.textContent).not.toContain("sk-live");
  // What the home unit gives the seat is listed under the unit, not merged.
  expect(screen.getByText("${ENGINEERING_GITHUB_TOKEN}")).toBeDefined();
  expect(screen.getByText(/Set on its unit, Engineering/)).toBeDefined();
  expect(screen.getByText("ENG")).toBeDefined();
});

test("without a token the configured half is the guarded banner, and the rest still renders", async () => {
  mount("#/seats/ceo", (what) =>
    what === "config" ? Promise.reject(new Error("unauthorized")) : answering(what),
  );
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
  expect(screen.getByRole("button", { name: "Set token" })).toBeDefined();
  // Identity is anonymous-readable, and still on the page.
  expect(screen.getByText("Who this is")).toBeDefined();
  expect(screen.getAllByText("Set direction").length).toBeGreaterThan(0);

  fireEvent.click(screen.getByRole("tab", { name: "Access" }));
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
});

// A REFUSAL CLEARS WHAT THE TOKEN HAD READ. The query keeps its last good
// answer through a failed ask, so a token cleared or refused mid-session left
// the seat's email, budget and schedules on screen beside the banner saying
// they need a token.
test("a refused re-read takes the guarded settings off the page", async () => {
  let refuse = false;
  const { store } = mountWithStore("#/seats/ceo", (what) =>
    what === "config" && refuse ? Promise.reject(new Error("unauthorized")) : answering(what),
  );
  expect(await screen.findByText("ceo@example.com")).toBeDefined();
  expect(screen.getByText("weekly-review")).toBeDefined();
  expect(screen.getByText(fmtCount(250000))).toBeDefined();

  refuse = true;
  // A reconnect asks every query again, as a token change does.
  act(() => store.setConnected(true));
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
  expect(screen.queryByText("ceo@example.com")).toBeNull();
  expect(screen.queryByText("weekly-review")).toBeNull();

  fireEvent.click(screen.getByRole("tab", { name: "Cost" }));
  expect(screen.queryByText(fmtCount(250000))).toBeNull();
  expect(screen.getByText("Unknown")).toBeDefined();
});

test("an engine that sends no derived hierarchy is reported, not guessed at", async () => {
  const { derived: _omitted, ...older } = projection;
  mount("#/seats/ceo", answering, older);
  expect(await screen.findByText("Who this is")).toBeDefined();
  expect(screen.getAllByText("not reported by this engine").length).toBeGreaterThan(0);
  expect(screen.getByText("Not reported")).toBeDefined();
});
