/**
 * A consent product's redirect step: the address to copy, and, while the
 * product's client last signed in through an earlier engine address, a line
 * saying so.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { RedirectStep } from "./RedirectStep.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type SetupRequirement } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";

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

afterEach(cleanup);

const ADMIN = {
  login: "U0",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
};

const URI = "https://crewlet.example.com/webhooks/mcp-oauth";
const BEFORE = "https://old-tunnel.example.com/webhooks/mcp-oauth";

const STEP: SetupRequirement = {
  field: "redirect_uri",
  label: "Authorized redirect URI",
  kind: "redirect",
  config_path: "",
  required: false,
  present: false,
  hint: [{ text: "Add it to the client's Authorized redirect URIs." }],
  redirect: { url: URI, key: "api.public.external_url" },
};

/** The engine's redirect notice, naming AdSense's and AdMob's shared client. */
const MOVED = {
  notices: [
    {
      class: "oauth_redirect_moved",
      subject: { kind: "company" },
      redirect: {
        uri: URI,
        clients: [
          {
            vendor: "google",
            id: "123456789012-abcdefghij.apps.googleusercontent.com",
            label: "123456789012-abcdefgh…",
            uses: [
              { name: "adsense", title: "AdSense" },
              { name: "admob", title: "AdMob" },
            ],
            redirect: BEFORE,
            at: "2026-01-01T09:00:00Z",
          },
        ],
      },
      finding: null,
    },
  ],
  unread: [],
};

function mount(integration: string, notices: unknown = MOVED, viewer: unknown = ADMIN) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve(viewer)
      : what === "advisor_notices"
        ? Promise.resolve(notices)
        : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <FrameReadings>
        <Router>
          <RedirectStep r={STEP} integration={integration} />
        </Router>
      </FrameReadings>
    </ClientContext.Provider>,
  );
}

test("a product whose client last signed in elsewhere says so under the address", async () => {
  mount("admob");
  await afterMicrotasks();
  expect(screen.getByRole("button", { name: "Copy Authorized redirect URI" })).toBeTruthy();
  expect(screen.getByText("Redirect URI out of date")).toBeTruthy();
  expect(screen.getByText(BEFORE)).toBeTruthy();
  expect(document.body.textContent).toContain(
    "Add the current redirect URI to its Authorized redirect URIs in Google Cloud",
  );
});

test("a product whose client lists the address now, or a reader not told notices, reads none", async () => {
  mount("google_play");
  await afterMicrotasks();
  expect(screen.queryByText("Redirect URI out of date")).toBeNull();
  cleanup();
  mount("admob", MOVED, { ...ADMIN, grants: ["worklet"] });
  await afterMicrotasks();
  expect(screen.queryByText("Redirect URI out of date")).toBeNull();
});
