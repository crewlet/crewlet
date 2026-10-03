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

import { PeopleAndAccess, BINDING_WORDS } from "./Access.tsx";
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
  auth: { disabled: false, anonymous_read: true, allowed_origins: [] },
  tokens: [
    { id: "ci", scope: "operator", seat: null, yours: false },
    { id: "founder", scope: "person", seat: { handle: "ana", name: "Ana Diaz" }, yours: true },
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

test("a token acts as the person who binds it, and a token nobody binds acts as nobody", async () => {
  mount(async (what) => (what === "access" ? answer : null));
  // The token row names the seat it acts as; the one nobody binds says so.
  const founder = (await screen.findByText("yours")).closest(".grid-row") as HTMLElement;
  expect(within(founder).getByText("Ana Diaz")).toBeDefined();
  expect(within(founder).getByText("Person")).toBeDefined();
  const ci = screen.getByText(/Nobody — writes under its own label/).closest(".grid-row");
  expect(within(ci as HTMLElement).getByText("Operator")).toBeDefined();
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

test("a refused reader sees the refusal alone — no tiles, no empty lists", async () => {
  const view = mount((what) =>
    what === "access" ? Promise.reject(new QueryError("unauthorized")) : Promise.resolve(null),
  );
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
  expect(view.container.querySelector(".crewlet-statcard")).toBeNull();
  expect(screen.queryByText("People", { selector: "*" })).toBeNull();
  expect(screen.queryByText(/No token is configured/)).toBeNull();
});
