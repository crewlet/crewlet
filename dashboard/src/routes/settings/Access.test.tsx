/**
 * People & access draws the engine's join of people and credentials, and holds
 * no credential's value.
 *
 * The invariants, in the order they cost when they go: no value reaches the
 * page — not an answer's (the answer has no member for one) and not the one
 * this browser holds; a reader the engine refuses sees the refusal and
 * nothing that looks like an empty company; and each binding failure reads as
 * its own state, because each has its own remedy.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { PeopleAndAccess, ANONYMOUS_WORDS, BINDING_WORDS, ROLE_WORDS } from "./Access.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store, storeToken } from "~/protocol/index.ts";
import type { AccessAnswer } from "~/contract/access.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The token this browser presents — which must appear nowhere on the page. */
const HELD = "held-token-value-8Hq2";

const answer: AccessAnswer = {
  auth: { disabled: false, anonymous: "public", allowed_origins: [], company_writers: [] },
  tokens: [
    { id: "ci", role: "admin", seat: null, yours: false },
    { id: "founder", role: "admin", seat: { handle: "ana", name: "Ana Diaz" }, yours: true },
    { id: "kiosk", role: "member", seat: null, yours: false },
  ],
  people: [
    {
      handle: "ana",
      name: "Ana Diaz",
      email: "ana@example.com",
      availability: "",
      operator_id: "founder",
      binding: "bound",
      contacts: [{ key: "slack_user_id", value: "U0ANA", reference: false, resolves: true }],
    },
    {
      handle: "bo",
      name: "Bo Lang",
      email: "",
      availability: "",
      operator_id: "",
      binding: "unbound",
      contacts: [
        { key: "gitlab_username", value: "${BO_GITLAB}", reference: true, resolves: false },
      ],
    },
    {
      handle: "cy",
      name: "Cy Moss",
      email: "",
      availability: "",
      operator_id: "${CY_OPERATOR}",
      binding: "unresolved",
      contacts: [{ key: "slack_user_id", value: "U0CY", reference: false, resolves: true }],
    },
    {
      handle: "dee",
      name: "Dee Park",
      email: "",
      availability: "",
      operator_id: "founderr",
      binding: "no_token",
      contacts: [{ key: "slack_user_id", value: "U0DEE", reference: false, resolves: true }],
    },
  ],
};

function mount(query: (what: string) => Promise<unknown>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = query;
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <PeopleAndAccess />
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  storeToken(HELD);
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

test("no credential's value is rendered, not even the one this browser holds", async () => {
  const view = mount(async (what) => (what === "access" ? answer : null));
  await screen.findAllByText("Ana Diaz");
  expect(view.container.innerHTML).not.toContain(HELD);
  expect(document.body.innerHTML).not.toContain(HELD);
  // The labels are what is drawn, and the caller's own is marked.
  expect(screen.getAllByText("founder").length).toBeGreaterThan(0);
  expect(screen.getByText("yours")).toBeDefined();
});

// THE ROLE AND THE LINK ARE TWO FACTS, drawn as two columns: the role says
// how far a key reaches, the link who it acts as. An admin key nobody binds
// still writes the configuration under its own label; a member key nobody
// binds writes nothing, because a member changes things only as a person.
test("a token shows its role beside the person it acts as, or nobody", async () => {
  mount(async (what) => (what === "access" ? answer : null));
  const founder = (await screen.findByText("yours")).closest(".grid-row") as HTMLElement;
  expect(within(founder).getByText("Ana Diaz")).toBeDefined();
  expect(within(founder).getByText(ROLE_WORDS.admin.label)).toBeDefined();
  const ci = screen.getByText(/Nobody — writes under its own label/).closest(".grid-row");
  expect(within(ci as HTMLElement).getByText(ROLE_WORDS.admin.label)).toBeDefined();
  const kiosk = screen.getByText(/Nobody — reads only/).closest(".grid-row");
  expect(within(kiosk as HTMLElement).getByText(ROLE_WORDS.member.label)).toBeDefined();
  // AND THE COUNT says how many of each the guard accepts.
  const tile = screen.getByText("2 admin · 1 member").closest(".crewlet-statcard") as HTMLElement;
  expect(within(tile).getByText("API tokens")).toBeDefined();
});

test("each binding failure reads as its own state, with the binding as written", async () => {
  mount(async (what) => (what === "access" ? answer : null));
  for (const state of ["bound", "unbound", "unresolved", "no_token"] as const) {
    expect(await screen.findByText(BINDING_WORDS[state].label)).toBeDefined();
  }
  // THE REFERENCE, never a value: `${CY_OPERATOR}` is what the chart says.
  expect(screen.getByText("${CY_OPERATOR}")).toBeDefined();
  // An unset contact variable is drawn as a warning, and says why.
  const gitlab = screen.getByText("${BO_GITLAB}").closest("[title]");
  expect(gitlab?.getAttribute("title")).toMatch(/not set in the engine's environment/);
  // Two failed bindings, counted.
  const tile = screen.getByText("Broken bindings").closest(".crewlet-statcard") as HTMLElement;
  expect(within(tile).getByText("2")).toBeDefined();
});

test("a disabled guard is said out loud", async () => {
  mount(async (what) =>
    what === "access" ? { ...answer, auth: { ...answer.auth, disabled: true }, tokens: [] } : null,
  );
  expect((await screen.findByRole("alert")).textContent).toMatch(/api\.auth\.disabled/);
  expect(screen.getByText(/accepts no token at all/)).toBeDefined();
});

// A MANAGED DOCUMENT (ADR-0030) IS PART OF THE POSTURE: the writers are named
// where the deployment's api.auth is read, and nothing is said when every
// admin key may write.
test("a managed document names its writers beside the tokens", async () => {
  mount(async (what) =>
    what === "access"
      ? { ...answer, auth: { ...answer.auth, company_writers: ["gitops", "ci"] } }
      : null,
  );
  const line = (await screen.findByText(/The company document is managed/)).closest("span");
  expect(line?.textContent).toMatch(
    /only the admin keys gitops, ci may change it \(api\.auth\.company_writers\)/,
  );
  cleanup();

  mount(async (what) => (what === "access" ? answer : null));
  await screen.findAllByText("Ana Diaz");
  expect(screen.queryByText(/The company document is managed/)).toBeNull();
});

// THE POSTURE IS SAID IN WORDS, one sentence per kind of caller: what a member
// key reaches, what an admin key adds, and what a caller with no key reaches
// under THIS deployment's `api.auth.anonymous` — the one a person deciding
// which key to hand a teammate has to read beside the keys.
test.each(["public", "none"] as const)(
  "the posture says what each role and a caller with no key reach (anonymous: %s)",
  async (anonymous) => {
    mount(async (what) =>
      what === "access" ? { ...answer, auth: { ...answer.auth, anonymous } } : null,
    );
    const said = (await screen.findByText(ANONYMOUS_WORDS[anonymous], { exact: false }))
      .textContent;
    expect(said).toContain(`(anonymous: ${anonymous})`);
    const roles = screen.getByText(/A member key:/).textContent ?? "";
    expect(roles).toMatch(/what the company published/);
    expect(roles).toMatch(/An admin key: everything a member reads/);
    for (const surface of ["/config", "/secrets", "/setup"]) expect(roles).toContain(surface);
  },
);

// AND THE TWO POSTURES ARE TWO SENTENCES: a stranger reading the chart and a
// stranger reading nothing are not to be told apart by a reader squinting.
test("the two anonymous postures say different things", () => {
  expect(ANONYMOUS_WORDS.public).not.toBe(ANONYMOUS_WORDS.none);
  expect(ANONYMOUS_WORDS.public).toMatch(/chart/);
  expect(ANONYMOUS_WORDS.none).toMatch(/nothing of the company/);
});

test("a refused reader sees the refusal alone — no tiles, no empty lists", async () => {
  const view = mount((what) =>
    what === "access" ? Promise.reject(new QueryError("unauthorized")) : Promise.resolve(null),
  );
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
  expect(view.container.querySelector(".crewlet-statcard")).toBeNull();
  expect(screen.queryByText("People", { selector: "*" })).toBeNull();
  expect(screen.queryByText(/No token is configured/)).toBeNull();
});
