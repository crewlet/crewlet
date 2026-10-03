/**
 * An integration's tag is the ENGINE's roll-up, and a tag is drawn only from
 * an answer that arrived.
 *
 * Which surface's word wins, and whether a configured surface reads Connected
 * or Connecting, is decided once, in `integration.Rollup`, and arrives as
 * `tools` on the `integrations` answer. So the one thing left for this screen
 * to get wrong is drawing a tag it was not given: while that answer is
 * loading, refused or failed, the peek's header holds no tag at all — never a
 * state computed from half an answer, which is what put an amber "Connecting"
 * on Slack, whose loop writes no status row ever, with nothing on the rail to
 * say why.
 *
 * The setup listing is the OTHER half: it decides only which form a card can
 * open. A listing refused or failed is said in a banner of its own, naming the
 * grant or the failure — and the card's tag, the engine's, stands beside it.
 */

import { act, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { ReactElement } from "react";
import { IntegrationPeek, Integrations } from "./Integrations.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryRefusedError, Store } from "~/protocol/index.ts";
import type { IntegrationsAnswer, IntegrationRow } from "~/contract/integrations.ts";
import type { SetupToolState } from "~/protocol/types.ts";

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

/** The engine's answer: Slack's rows, and its roll-up — Connected. */
const ANSWER: IntegrationsAnswer = {
  integrations: [SLACK],
  tools: [
    {
      key: "slack",
      surfaces: ["slack"],
      state: "connected",
      label: "Connected",
      reason: "",
      surface: "slack",
    },
  ],
  traffic_known: true,
  traffic_since: null,
};

/** Slack as the listing reports it. */
const SLACK_TOOL: SetupToolState = {
  key: "slack",
  configured: true,
  enabled: true,
  satisfied: true,
  can_provision: false,
  requirements: [],
};

const VIEWER = {
  login: "jane.doe",
  grants: ["state:read", "config:read"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
  acts: [],
};

/** What the `integrations` question answers, set per case. */
let integrations: () => Promise<unknown> = () => Promise.resolve(ANSWER);
/** What `/setup/integrations` answers, set per case. */
let listing: () => Promise<Response> = () =>
  Promise.resolve(new Response(JSON.stringify({ tools: [SLACK_TOOL] }), { status: 200 }));

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === "/setup/integrations") return listing();
      return new Response(JSON.stringify({ runs: [] }), { status: 200 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  integrations = () => Promise.resolve(ANSWER);
  listing = () =>
    Promise.resolve(new Response(JSON.stringify({ tools: [SLACK_TOOL] }), { status: 200 }));
  location.hash = "#/";
});

function mount(ui: ReactElement) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer") return Promise.resolve(VIEWER);
    if (what === "integrations") return integrations();
    if (what === "events") return Promise.resolve({ events: [] });
    return new Promise(() => {});
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>{ui}</Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
}

const json = (body: unknown, status: number) => () =>
  Promise.resolve(new Response(JSON.stringify(body), { status }));

/** Lets every answer that has arrived be drawn. */
async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

// WHILE THE ANSWER IS LOADING there is no tag, and no state computed from
// nothing; once it answers, the tag is the engine's.
test("the peek draws no tag before the engine's answer, and the engine's tag after", async () => {
  let answer: (value: unknown) => void = () => {};
  integrations = () => new Promise((resolve) => (answer = resolve));
  mount(<IntegrationPeek kind="slack" />);
  expect(await screen.findByText("Slack")).toBeDefined();
  await settle();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Connected")).toBeNull();

  await act(async () => answer(ANSWER));
  await settle();
  expect(screen.getByText("Connected")).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
});

// A REFUSED ANSWER IS SAID, with the grant it named — never a tag drawn as
// though the engine had rolled the tool up.
test("the peek says a refused answer names its grant, and draws no tag", async () => {
  integrations = () =>
    Promise.reject(
      new QueryRefusedError("unauthorized", { reason: "no_grant", grants: ["state:read"] }),
    );
  mount(<IntegrationPeek kind="slack" />);
  expect(await screen.findByText(/state:read/)).toBeDefined();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Connected")).toBeNull();
});

// AND A FAILED ONE TOO, in the words a socket question's failure is drawn in.
test("the peek says a failed answer failed, and draws no tag", async () => {
  integrations = () => Promise.reject(new QueryRefusedError("query_failed", null));
  mount(<IntegrationPeek kind="slack" />);
  await settle();
  expect(screen.queryByText("Connecting")).toBeNull();
  expect(screen.queryByText("Connected")).toBeNull();
  expect(document.body.textContent).not.toBe("");
});

// THE LISTING'S OWN REFUSAL OR FAILURE IS A BANNER ON THE SCREEN, and the
// card's tag — the engine's, which never turned on the listing — stands
// beside it rather than being recomputed from a listing nobody read.
test.each([
  [
    "refused",
    json({ error: "unauthorized", grants: ["config:read"] }, 403),
    // THE BANNER'S OWN WORDS: every disabled form action beside it names
    // config:read too.
    /Reading the integrations' setup state needs config:read/,
  ],
  ["failed", json({ error: "internal_error" }, 500), /tried to answer and failed/],
])("a %s listing is a banner, and Slack keeps the engine's tag", async (_, answer, banner) => {
  listing = answer;
  location.hash = "#/settings/integrations";
  mount(<Integrations />);
  expect(await screen.findByText(banner)).toBeDefined();
  expect(await screen.findByText("Slack")).toBeDefined();
  expect(screen.getAllByText("Connected").length).toBeGreaterThan(0);
  expect(screen.queryByText("Connecting")).toBeNull();
});

// A REFUSED LISTING OFFERS THE DOOR TO THE GRANT IT NAMED.
test("a refused listing's banner names its grant and offers a sign-in", async () => {
  listing = json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403);
  location.hash = "#/settings/integrations";
  mount(<Integrations />);
  expect(
    await screen.findByText(/Reading the integrations' setup state needs config:read/),
  ).toBeDefined();
  expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
});
