/**
 * The catalogue as a reader meets it: one tile per tool, and one action each.
 *
 * The pure halves — which state a tile draws, which action it offers — are
 * held in `Integrations.test.tsx`. What only a render can hold is that the
 * two meet on screen the way they are meant to: every tile carries exactly
 * ONE control, Manage and Learn more are real links, Rotate token opens the
 * settings form saying why, the filter narrows what is shown, and a reader
 * the engine refuses `/setup` sees every form action disabled with its reason
 * rather than removed.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Integrations } from "./Integrations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { IntegrationsAnswer } from "~/contract/integrations.ts";
import type { SetupListing, SetupToolState } from "~/protocol/types.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const refusal = "The GitLab admin token expires in 3 days.";

/**
 * A company with GitLab connected and its admin token about to lapse, Datadog
 * connected and working, and everything else not in use — the three kinds of
 * tile the catalogue draws, in one answer.
 */
const answer: IntegrationsAnswer = {
  integrations: [
    {
      key: "gitlab",
      configured: true,
      inbound: 4,
      reconcile: {
        phase: "ready",
        findings: [
          {
            kind: "credential_expiring",
            subject: "admin_token",
            detail: refusal,
            expires_at: "2026-10-02T00:00:00Z",
          },
        ],
      },
    },
    { key: "datadog", configured: true, inbound: 0, reconcile: { phase: "ready" } },
  ],
  tools: [
    {
      key: "gitlab",
      surfaces: ["gitlab"],
      state: "attention",
      label: "Credential expiring",
      reason: refusal,
      surface: "gitlab",
    },
    { key: "datadog", surfaces: ["datadog"], state: "connected", label: "Connected", reason: "" },
    ...["slack", "mattermost", "atlassian", "github"].map((key) => ({
      key,
      surfaces: [key],
      state: "not_in_use" as const,
      label: "Not in use",
      reason: "",
    })),
  ],
  traffic_known: true,
  traffic_since: null,
};

function tool(key: string, over: Partial<SetupToolState> = {}): SetupToolState {
  return {
    key,
    configured: false,
    enabled: false,
    satisfied: false,
    requirements: [
      {
        field: "admin_token",
        label: "Admin token",
        kind: "secret",
        config_path: `integrations.${key}.admin_token`,
        required: true,
        present: over.configured ?? false,
        resolved: over.configured ?? false,
      },
    ],
    ...over,
  };
}

const listing: SetupListing = {
  tools: [
    tool("gitlab", { configured: true, enabled: true, satisfied: true }),
    tool("datadog", { configured: true, enabled: true, satisfied: true }),
    ...["slack", "mattermost", "atlassian", "jira", "confluence", "github"].map((k) => tool(k)),
  ],
  external_url: { value: "https://engine.example.com", config_path: "api.external_url" },
} as unknown as SetupListing;

function stubFetch(setup: { status: number; body: unknown }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === "/setup/integrations") {
        return new Response(JSON.stringify(setup.body), { status: setup.status });
      }
      return new Response(JSON.stringify({ secrets: [] }), { status: 200 });
    }),
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

/** Somebody who may read and connect an integration. */
const OPERATOR = {
  login: "ops",
  owner: "ops",
  grants: ["config:read", "config:write", "secrets:write"],
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(viewer: unknown = OPERATOR) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "integrations" ? answer : what === "viewer" ? viewer : []);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <Integrations />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
    { wrapper: LayerHost },
  );
}

/** One tile, found by its heading. */
async function tile(name: string): Promise<HTMLElement> {
  const heading = await screen.findByRole("heading", { name });
  return heading.closest("article") as HTMLElement;
}

/** Every control a tile carries: buttons and links alike. */
function controls(el: HTMLElement): HTMLElement[] {
  return [...el.querySelectorAll<HTMLElement>("button, a[href]")];
}

// ONE ACTION EACH. A tile in a grid of eight is read in one glance, and the
// card it replaced carried a tag, a Connect, a Disconnect, a settings square
// and a chevron — five controls to learn that nothing was owed.
test("every tile carries exactly one action, and which one follows the engine", async () => {
  stubFetch({ status: 200, body: listing });
  mount();

  const gitlab = await tile("GitLab");
  expect(controls(gitlab).map((c) => c.textContent)).toEqual(["Rotate token"]);
  // THE ENGINE'S REASON is the tile's sentence while something is owed.
  expect(within(gitlab).getByText(refusal)).toBeTruthy();
  expect(within(gitlab).getByText("Credential expiring")).toBeTruthy();
  expect(within(gitlab).getByText("1 expiring · 4 deliveries")).toBeTruthy();

  // MANAGE IS A LINK to the tool's own page, where Disconnect lives.
  const datadog = await tile("Datadog");
  const manage = controls(datadog);
  expect(manage.map((c) => c.textContent)).toEqual(["Manage"]);
  expect(manage[0]?.getAttribute("href")).toBe("#/settings/integrations/datadog");
  expect(within(datadog).queryByRole("button", { name: /Disconnect/ })).toBeNull();

  // AND A TOOL NOBODY CONNECTED is Connect, under the engine's own word.
  const slack = await tile("Slack");
  expect(controls(slack).map((c) => c.getAttribute("aria-label"))).toEqual(["Connect Slack"]);
  expect(within(slack).getByText("Not in use")).toBeTruthy();
});

// ROTATE TOKEN OPENS THE SETTINGS FORM, SAYING WHY.
test("rotate token opens the tool's form with the engine's reason", async () => {
  stubFetch({ status: 200, body: listing });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Rotate token for GitLab" }));
  const dialog = await screen.findByRole("dialog", { name: "Rotate the GitLab token" });
  expect(within(dialog).getByText(refusal)).toBeTruthy();
});

// THE FILTER NARROWS WHAT IS SHOWN, and its counts are the engine's rows.
test("the filter shows what the company has, or what it could have", async () => {
  stubFetch({ status: 200, body: listing });
  mount();
  await tile("GitLab");
  fireEvent.click(screen.getByRole("radio", { name: /Connected/ }));
  expect(screen.getAllByRole("heading").map((h) => h.textContent)).toEqual(["Datadog", "GitLab"]);
  fireEvent.click(screen.getByRole("radio", { name: /Available/ }));
  await waitFor(() => expect(screen.queryByRole("heading", { name: "GitLab" })).toBeNull());
  expect(screen.getByRole("heading", { name: "Slack" })).toBeTruthy();
});

// A WRITE CONTROL IS NEVER HIDDEN. `/setup` is guarded in full, so a reader
// the engine refuses has no form to open — and every form action says which
// grants it takes on the control itself, while Manage (which writes nothing)
// still works.
test("a reader refused /setup sees every form action disabled with the grants it takes", async () => {
  stubFetch({
    status: 403,
    body: { error: "unauthorized", reason: "config", grants: ["config:read"] },
  });
  mount({ login: "dee", owner: "dee", grants: ["state:read"] });
  const connect = await screen.findByRole("button", { name: "Connect Slack" });
  expect(connect.getAttribute("aria-disabled")).toBe("true");
  expect(connect.getAttribute("aria-describedby")).toBeTruthy();
  expect(document.body.textContent).toContain(
    "Setting an integration up needs config:read to see what it needs, and config:write and secrets:write to connect it.",
  );
  fireEvent.click(connect);
  expect(screen.queryByRole("dialog")).toBeNull();
  const datadog = await tile("Datadog");
  expect(controls(datadog)[0]?.getAttribute("href")).toBe("#/settings/integrations/datadog");
});
