/**
 * The integration peek draws its tag from BOTH halves, and says why when one
 * is missing.
 *
 * Whether anything converges a surface is the setup listing's to say, and it
 * decides between "Connected" and "the loop has not reported yet". The peek
 * read that listing and drew none of its states: while it loaded, and when it
 * was refused or failed, the tag was computed from the socket's half alone —
 * an empty list of tools — so Slack, whose loop writes no status row ever,
 * said "Connecting" in amber with nothing on the rail to say why. The main
 * screen's cards did the same under a refused or failed listing.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { act } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { IntegrationPeek, Integrations } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { IntegrationRow, SetupToolState } from "~/protocol/types.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** Slack as the socket reports it: configured, routing, and no reconcile row — ever. */
const SLACK: IntegrationRow = { key: "slack", configured: true, enabled: true, routes: true };

/** Slack as the listing reports it: nothing provisions it, so nothing will report. */
const SLACK_TOOL: SetupToolState = {
  key: "slack",
  configured: true,
  enabled: true,
  satisfied: true,
  can_provision: false,
  requirements: [],
};

/** What `/setup/integrations` answers, as a function of nothing: set per case. */
let listing: () => Promise<Response> = () => new Promise(() => {});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === "/setup/integrations") return listing();
      return new Response(JSON.stringify({ events: [] }), { status: 200 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  listing = () => new Promise(() => {});
  location.hash = "#/";
});

function mount(ui: React.ReactElement) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) =>
    what === "integrations" ? { integrations: [SLACK], traffic_known: true } : {};
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{ui}</Router>
    </ClientContext.Provider>,
  );
}

const json = (body: unknown, status: number) => () =>
  Promise.resolve(new Response(JSON.stringify(body), { status }));

/** Lets every answer that has arrived be drawn. */
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

// WHILE THE LISTING IS LOADING the tag that turns on it is not known, so there
// is none — and the wait is SAID in its place, never a blank slot that reads as
// a tool with nothing to report. Once it answers, the tag is the one the
// listing decides.
test("the peek says its tag is on its way before the listing answers, and Connected after", async () => {
  let answer: (response: Response) => void = () => {};
  listing = () => new Promise((resolve) => (answer = resolve));
  mount(<IntegrationPeek kind="slack" />);
  expect(await screen.findByText("Slack")).toBeDefined();
  await settle();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Connected")).toBeNull();
  expect(screen.getByText("Reading what this integration needs")).toBeDefined();

  await act(async () => answer(new Response(JSON.stringify({ tools: [SLACK_TOOL] }))));
  await settle();
  expect(screen.getByText("Connected")).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Reading what this integration needs")).toBeNull();
});

// A REFUSED LISTING IS SAID, with the grant it named and the door to it, as
// the screen's own banner says it — never a tag computed as though the listing
// had answered that nothing is configured to report.
test("the peek says a refused listing names its grant, and draws no Connecting tag", async () => {
  listing = json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403);
  mount(<IntegrationPeek kind="slack" />);
  expect(await screen.findByText(/config:read/)).toBeDefined();
  expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
  // NOR A PLACEHOLDER: the listing has answered, and no tag is on its way.
  expect(screen.queryByText("Reading what this integration needs")).toBeNull();
});

// AND A FAILED ONE TOO, in the words a socket question's failure is drawn in.
test("the peek says a failed listing failed, and draws no Connecting tag", async () => {
  listing = json({ error: "internal_error" }, 500);
  mount(<IntegrationPeek kind="slack" />);
  expect(await screen.findByText(/tried to answer and failed/)).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Reading what this integration needs")).toBeNull();
});

// THE SCREEN'S CARD IS THE SAME: under a refused or failed listing it draws no
// Connecting tag either, beside the banner that says why.
test.each([
  ["refused", json({ error: "unauthorized", grants: ["config:read"] }, 403), /config:read/],
  ["failed", json({ error: "internal_error" }, 500), /tried to answer and failed/],
])("a %s listing leaves the Slack card with no Connecting tag", async (_, answer, banner) => {
  listing = answer;
  location.hash = "#/admin/integrations";
  mount(<Integrations />);
  expect(await screen.findByText(banner)).toBeDefined();
  expect(await screen.findByText("Slack")).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
});
