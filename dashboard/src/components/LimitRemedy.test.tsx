/**
 * The way to the ceiling that refused something: offered to whoever may raise
 * it, at the section that edits it, and to nobody else.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { LimitRemedy } from "./LimitRemedy.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { ADMIN_RAISES } from "~/lib/remedy.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";
import type { Remedy } from "~/protocol/remedy.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { healthFrame } from "~/test/health.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

/** An administrator, who holds every grant. */
const ADMIN = {
  login: "U0",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
  config_writer: true,
  config_managed_by: [],
};

/** A person with no Admin, who leads no unit. */
const MEMBER = { ...ADMIN, login: "dee", grants: ["worklet"], handle: "", owner: "dee" };

function mount(remedy: Remedy | null, viewer: unknown = ADMIN) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.setConnected(true);
  store.applyOrg(CHART_ORG as OrgProjection);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <FrameReadings>
        <Router>
          <LimitRemedy remedy={remedy} />
        </Router>
      </FrameReadings>
    </ClientContext.Provider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

// EACH CEILING OPENS ITS OWN SECTION OF BUDGETS, in the reader's words, at an
// address the router resolves to that screen: a link that resolved elsewhere
// would send the person to change a setting that does not refuse them.
test.each([
  [{ setting: "assistant_token_budget" }, "Open assistant budget", "assistant"],
  [{ setting: "company_token_budget" }, "Open company budget", "company"],
  [{ setting: "seat_token_budget", seat: "devrel" }, "Open seat budget", "seat:devrel"],
] as const)(
  "%o opens Budgets at its section for a reader who may raise it",
  async (remedy, label, section) => {
    mount(remedy);
    await settle();
    const link = screen.getByRole("link", { name: label });
    const to = link.getAttribute("href") ?? "";
    const [path, query = ""] = to.replace(/^#\//, "").split("?");
    expect(resolve(path!.split("/"))).toMatchObject({ resolved: true, screen: "budgets" });
    expect(new URLSearchParams(query).get("section")).toBe(section);
  },
);

// WHO MAY NOT RAISE IT IS TOLD WHO CAN, and handed no link to a screen whose
// controls are all locked for them; a managed document names who manages it.
//
// Mutation: offer the link to every reader, and both cases find one.
test.each([
  ["a reader without Admin", MEMBER, ADMIN_RAISES],
  [
    "an administrator of a document another system manages",
    { ...ADMIN, config_writer: false, config_managed_by: ["gitops"] },
    managedSentence(["gitops"]),
  ],
])("%s is told who can raise it, with no link", async (_name, viewer, sentence) => {
  mount({ setting: "assistant_token_budget" }, viewer);
  await settle();
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText(sentence)).toBeDefined();
});

test("a refusal no limit caused draws nothing, and asks no gate", () => {
  // NO VIEWER IS MOUNTED: a remedy-less refusal must not need one.
  render(<LimitRemedy remedy={null} />);
  expect(document.body.textContent).toBe("");
});
