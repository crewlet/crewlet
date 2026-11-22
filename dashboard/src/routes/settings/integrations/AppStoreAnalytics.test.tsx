/**
 * Turn on App Store analytics: the pasted Admin key is sent once and leaves
 * the page, each app's outcome is said, and the person is told to revoke it.
 */
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { AppStoreAnalytics, AppStoreKeys } from "./AppStoreAnalytics.tsx";
import { analyticsConnections, analyticsLine, connectedKeys } from "./appStoreAnalytics.ts";
import type { SetupRequirement, SetupToolState } from "~/contract/setup.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const KEY = "-----BEGIN PRIVATE KEY-----\nMIGT\n-----END PRIVATE KEY-----";

function mount(connections: string[]) {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <AppStoreAnalytics connections={connections} />
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

// THE KEY IS SENT ONCE AND LEAVES THE PAGE: the turn-on posts the connection,
// the key ID and the .p8, the field is empty after, each app's outcome is
// said, and the answer's revoke line is shown. Mutation: keep the key in state
// after the send, and the field still holds it.
test("a pasted Admin key turns analytics on once and is not kept", async () => {
  const sent: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      sent.push(JSON.parse(String(init?.body)));
      return new Response(
        JSON.stringify({
          connection: "main",
          apps: [
            { app: "111", state: "on" },
            { app: "222", state: "failed", detail: "Apple refused the key" },
          ],
          revoke:
            "Revoke this key in App Store Connect now: Crewlet used it once and kept no copy.",
        }),
      );
    }),
  );
  mount(["main"]);
  const open = await screen.findByRole("button", { name: "Turn on App Store analytics" });
  await waitFor(() => expect(open.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(open);
  fireEvent.change(screen.getByLabelText("Admin key ID"), { target: { value: "P1A2S3T4E5" } });
  const key = screen.getByLabelText("Admin key (.p8)") as HTMLTextAreaElement;
  fireEvent.change(key, { target: { value: KEY } });
  fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
  await screen.findByText("App 111: turned on.");
  expect(sent).toEqual([{ connection: "main", key_id: "P1A2S3T4E5", private_key: KEY }]);
  expect(screen.getByText("App 222: not turned on: Apple refused the key")).toBeTruthy();
  expect(screen.getByText(/Revoke this key in App Store Connect now/)).toBeTruthy();
  expect(document.body.textContent).not.toContain("BEGIN PRIVATE KEY");
});

// NOTHING TO TURN ON without a connection: the press says why.
test("with no connection the turn-on is held", async () => {
  mount([]);
  const open = await screen.findByRole("button", { name: "Turn on App Store analytics" });
  await waitFor(() => expect(open.getAttribute("aria-disabled")).toBe("true"));
});

function requirement(path: string, present: boolean): SetupRequirement {
  return {
    field: path,
    label: path,
    kind: "id",
    config_path: path,
    required: true,
    present,
  } as SetupRequirement;
}

// THE CONNECTIONS ARE THE FORM'S, each named once with an issuer ID present.
test("the connections are the ones the form names with an issuer ID", () => {
  const state = {
    key: "app_store_connect",
    configured: true,
    enabled: true,
    requirements: [
      requirement("integrations.app_store_connect.connections.main.issuer_id", true),
      requirement("integrations.app_store_connect.connections.main.vendor_number", true),
      requirement("integrations.app_store_connect.connections.eu.issuer_id", false),
    ],
  } as SetupToolState;
  expect(analyticsConnections([state, state])).toEqual(["main"]);
  expect(analyticsLine({ app: "9", state: "already_on" })).toBe("App 9: already on.");
});

// ONE ACTION REMOVES A KEY, and a connection's last key stays: Remove asks
// once, then sends one DELETE naming the connection and the slot. Mutation:
// let the last key's Remove press, and the connection would be left keyless.
test("a key is removed in one action, and a connection's last key is held", async () => {
  const sent: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      sent.push(`${init?.method} ${new URL(String(input), "http://engine.test").pathname}`);
      return new Response(JSON.stringify({ removed: "admin" }));
    }),
  );
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <AppStoreKeys
          keys={[
            { connection: "main", slot: "marketing", label: "Marketing key" },
            { connection: "main", slot: "admin", label: "Admin key" },
            { connection: "eu", slot: "finance", label: "Finance key" },
          ]}
        />
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  const removes = await screen.findAllByRole("button", { name: "Remove" });
  await waitFor(() => expect(removes[1]!.getAttribute("aria-disabled")).not.toBe("true"));
  expect(removes[2]!.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(removes[1]!);
  const confirm = await screen.findByRole("alertdialog");
  fireEvent.click(within(confirm).getByRole("button", { name: "Remove" }));
  await screen.findByText("Admin key removed. Revoke it in App Store Connect too.");
  expect(sent).toEqual(["DELETE /setup/integrations/app_store_connect/keys/main/admin"]);
});

test("the connected keys are the ones the form shows stored", () => {
  const state = {
    key: "app_store_connect",
    configured: true,
    enabled: true,
    requirements: [
      requirement("integrations.app_store_connect.connections.main.keys.admin.private_key", true),
      requirement("integrations.app_store_connect.connections.main.keys.sales.private_key", false),
      requirement("integrations.app_store_connect.connections.main.keys.admin.key_id", true),
    ],
  } as SetupToolState;
  state.requirements[0]!.label = "Admin key (.p8)";
  expect(connectedKeys([state])).toEqual([
    { connection: "main", slot: "admin", label: "Admin key" },
  ]);
});
