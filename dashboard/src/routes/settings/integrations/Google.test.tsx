/**
 * The Google page as an admin meets it: the connection, the company reader and
 * the sign-in client said once, a switch per product, and each product's page
 * keeping its agents while pointing back to Google for what it shares.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router, useRoute } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { IntegrationsAnswer, ReconcileFinding } from "~/contract/integrations.ts";
import type { SetupListing, SetupRequirement, SetupToolState } from "~/protocol/types.ts";
import { withDerived } from "~/test/org.ts";
import { appRoute } from "~/lib/appRoute.ts";
import { Integrations } from "./Directory.tsx";
import { googleAddress } from "./address.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const ADMIN = { login: "ana", owner: "ana", handle: "ana", grants: ["admin"], acts: [] };
const PROVISIONER = "crewlet-provisioner@acme-crewlet.iam.gserviceaccount.com";
const READER = "crewlet-gr-acme-1@acme-crewlet.iam.gserviceaccount.com";
const CALLBACK = "https://engine.example.com/webhooks/mcp-oauth";

type Connection = "none" | "unfinished" | "connected";

/** Google Cloud's company section: the connection's fields, then the sign-in client's. */
function cloud(connection: Connection, client: boolean): SetupToolState {
  const on = connection !== "none";
  const done = connection === "connected";
  const req = (r: Partial<SetupRequirement> & Pick<SetupRequirement, "field" | "kind">) =>
    ({
      label: r.field,
      config_path: "",
      required: false,
      present: false,
      ...r,
    }) as SetupRequirement;
  return {
    key: "google_cloud",
    configured: on,
    enabled: on,
    satisfied: done,
    form_complete: done,
    can_provision: true,
    requirements: [
      req({
        field: "provisioner",
        label: "Provisioner service account",
        kind: "id",
        required: true,
        present: on,
        ...(on ? { value: PROVISIONER } : {}),
      }),
      req({
        field: "provider",
        label: "Workload identity provider",
        kind: "id",
        required: on,
        present: done,
      }),
      req({
        field: "redirect_uri",
        label: "Authorized redirect URI",
        kind: "redirect",
        group: "sign_in",
        redirect: { url: CALLBACK, key: "api.external_url" },
      }),
      req({
        field: "sign_in_client_id",
        label: "OAuth client ID",
        kind: "id",
        group: "sign_in",
        present: client,
        ...(client ? { value: "1234-abc.apps.googleusercontent.com" } : {}),
      }),
      req({
        field: "sign_in_client_secret",
        label: "OAuth client secret",
        kind: "secret",
        group: "sign_in",
        present: client,
      }),
    ],
  };
}

/** A Google product's section: its hidden switch, its reader where it reads, its sign-in where it has one. */
function product(
  key: string,
  on: boolean,
  parts: { reader?: "present" | "missing" | "unknown"; signIn?: "required" | "optional" },
): SetupToolState {
  const reqs: SetupRequirement[] = [
    {
      field: "enabled",
      label: "Let agents reach it",
      kind: "toggle",
      config_path: `integrations.${key}.enabled`,
      required: true,
      hidden: true,
      present: on,
      ...(on ? { value: "true" } : {}),
    },
  ];
  if (parts.reader)
    reqs.push({
      field: "reader",
      label: "Company reader",
      kind: "add_identity",
      config_path: "",
      required: false,
      present: false,
      identity: {
        email: READER,
        role: { id: "viewer", name: "Viewer" },
        where: [{ text: "Admin" }],
        level: "company",
        state: parts.reader,
      },
    });
  if (parts.signIn)
    reqs.push({
      field: "google_sign_in",
      label: parts.signIn === "required" ? "Sign in with Google" : "Admin sign-in",
      kind: "consent",
      config_path: "",
      required: parts.signIn === "required",
      present: false,
    });
  return {
    key,
    configured: on,
    enabled: on,
    satisfied: on,
    form_complete: on,
    can_provision: true,
    requirements: reqs,
  };
}

interface Case {
  connection: Connection;
  client?: boolean;
  /** Each product on, with what its pass found. */
  on?: Partial<Record<string, ReconcileFinding[] | "busy">>;
  reader?: "present" | "missing" | "unknown";
}

function listingOf(c: Case): SetupListing {
  const on = (k: string) => c.on?.[k] !== undefined;
  return {
    tools: [
      cloud(c.connection, c.client ?? false),
      product("google_analytics", on("google_analytics"), {
        reader: c.reader ?? "unknown",
        signIn: "optional",
      }),
      product("search_console", on("search_console"), { reader: c.reader ?? "unknown" }),
      product("google_ads", on("google_ads"), { reader: c.reader ?? "unknown" }),
      product("admob", on("admob"), { signIn: "required" }),
      product("adsense", on("adsense"), { signIn: "required" }),
      product("google_play", on("google_play"), {
        reader: c.reader ?? "unknown",
        signIn: "optional",
      }),
    ],
    external_url: { value: "https://engine.example.com", config_path: "api.external_url" },
  } as unknown as SetupListing;
}

function answerOf(c: Case): IntegrationsAnswer {
  const rows: IntegrationsAnswer["integrations"] = [];
  const tools: IntegrationsAnswer["tools"] = [];
  if (c.connection !== "none") {
    rows.push({ key: "google_cloud", configured: true, inbound: 0, reconcile: { phase: "ready" } });
    tools.push({
      key: "google_cloud",
      surfaces: ["google_cloud"],
      state: c.connection === "connected" ? "connected" : "attention",
      label: "",
      reason: "",
    });
  }
  for (const [key, found] of Object.entries(c.on ?? {})) {
    const findings = found === "busy" ? [] : (found ?? []);
    rows.push({
      key,
      configured: true,
      inbound: 0,
      reconcile: { phase: found === "busy" ? "activating" : "ready", findings },
    });
    tools.push({
      key,
      surfaces: [key],
      state: found === "busy" ? "not_connected" : findings.length > 0 ? "attention" : "connected",
      label: "",
      reason: "",
    });
  }
  return { integrations: rows, tools, traffic_known: true, traffic_since: null };
}

function RoutedIntegrations() {
  const { path } = useRoute();
  return <Integrations kind={path[2]} />;
}

/** The page at `hash` for case `c`; every write the page makes is kept. */
function mount(hash: string, c: Case): { writes: { path: string; body: unknown }[] } {
  location.hash = hash;
  const writes: { path: string; body: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const method = (init?.method ?? "GET").toUpperCase();
      if (method !== "GET")
        writes.push({ path: `${method} ${path}`, body: JSON.parse(String(init?.body ?? "{}")) });
      if (path === "/setup/integrations")
        return new Response(JSON.stringify(listingOf(c)), { status: 200 });
      const access = /^\/setup\/integrations\/([^/]+)\/access$/.exec(path);
      if (access && method === "GET")
        return new Response(
          JSON.stringify({
            kind: access[1],
            revision: "rev-1",
            syncing: false,
            options: [],
            seats: [],
          }),
          { status: 200 },
        );
      return new Response(JSON.stringify({}), { status: 201 });
    }),
  );
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer") return Promise.resolve(ADMIN);
    if (what === "integrations") return Promise.resolve(structuredClone(answerOf(c)));
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <RoutedIntegrations />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
    { wrapper: LayerHost },
  );
  return { writes };
}

/** A product's row on the Google page. */
async function row(name: string): Promise<HTMLElement> {
  const label = await screen.findByText(name, { selector: ".int-google-name" });
  return label.closest("li") as HTMLElement;
}

const NO_CLIENT: ReconcileFinding = {
  kind: "credential_missing",
  subject: "integrations.google_cloud.sign_in_client",
  detail: "No Google sign-in client is set, so nobody can sign in yet.",
  phase: "unconfigured",
};
const NOT_SIGNED_IN: ReconcileFinding = {
  kind: "credential_missing",
  detail: "Waiting for a person to sign in with Google and allow access.",
  phase: "unconfigured",
};

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("the directory lists Google once, above Google Cloud, with its connection's state", async () => {
  mount("#/company/integrations", { connection: "connected" });
  const nav = await screen.findByRole("navigation", { name: "Integrations" });
  await within(nav).findByRole("link", { name: /^Google Cloud/ });
  const links = within(nav).getAllByRole("link");
  const google = links.findIndex((a) => a.getAttribute("href") === "#/company/integrations/google");
  expect(google).toBeGreaterThan(-1);
  expect(links[google + 1]?.getAttribute("href")).toBe("#/company/integrations/google_cloud");
  expect(links[google]?.textContent).toContain("Connected");
});

test("with nothing set, Google lands on what it gives and Connect opens the connection's form in place", async () => {
  mount("#/company/integrations/google?product=google_analytics", { connection: "none" });
  expect(await screen.findByText("Google Analytics runs on your Google connection")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Connect Google" }));
  await waitFor(() =>
    expect(location.hash).toBe(
      "#/company/integrations/google?edit=settings&product=google_analytics",
    ),
  );
  expect(await screen.findByLabelText(/Provisioner service account/)).toBeTruthy();
  // THE SIGN-IN CLIENT IS ITS OWN FORM, never asked with the connection.
  expect(screen.queryByLabelText(/OAuth client ID/)).toBeNull();
});

test("a half-set connection opens on its form, and Products leads with finishing it", async () => {
  mount("#/company/integrations/google", { connection: "unfinished" });
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/google?edit=settings"));
  location.hash = "#/company/integrations/google?tab=products";
  expect(await screen.findByText("Finish connecting Google")).toBeTruthy();
});

test("a product turns on with its switch and off only after asking", async () => {
  const { writes } = mount("#/company/integrations/google", {
    connection: "connected",
    on: { google_analytics: [] },
    reader: "present",
  });
  const search = await row("Search Console");
  fireEvent.click(within(search).getByRole("switch", { name: "Search Console" }));
  await waitFor(() =>
    expect(writes).toContainEqual({
      path: "POST /setup/integrations/search_console/inputs",
      body: { values: { enabled: "true" } },
    }),
  );
  const analytics = await row("Google Analytics");
  fireEvent.click(within(analytics).getByRole("switch", { name: "Google Analytics" }));
  const dialog = await screen.findByRole("alertdialog", { name: "Turn off Google Analytics?" });
  expect(writes.some((w) => w.path.includes("google_analytics"))).toBe(false);
  fireEvent.click(within(dialog).getByRole("button", { name: "Turn off" }));
  await waitFor(() =>
    expect(writes).toContainEqual({
      path: "POST /setup/integrations/google_analytics/inputs",
      body: { values: { enabled: "false" } },
    }),
  );
});

test("a sign-in product's switch waits for the sign-in client", async () => {
  mount("#/company/integrations/google", { connection: "connected" });
  const admob = await row("AdMob");
  const sw = within(admob).getByRole("switch", { name: "AdMob" });
  expect(sw.getAttribute("aria-describedby")).toBeTruthy();
  expect(within(admob).getByRole("button", { name: "add the sign-in client" })).toBeTruthy();
});

test("each product says its one next step: the client, the sign-in, the reader", async () => {
  mount("#/company/integrations/google", {
    connection: "connected",
    client: false,
    reader: "missing",
    on: { admob: [NO_CLIENT], adsense: [NOT_SIGNED_IN], google_analytics: [] },
  });
  expect(
    within(await row("AdMob")).getByRole("button", { name: "Add the sign-in client" }),
  ).toBeTruthy();
  expect(within(await row("AdSense")).getByRole("button", { name: "Sign in" })).toBeTruthy();
  expect(within(await row("Google Analytics")).getByText(READER)).toBeTruthy();
});

test("a product turning on says so, and the address's product is marked", async () => {
  mount("#/company/integrations/google?product=google_ads", {
    connection: "connected",
    on: { google_ads: "busy" },
  });
  const ads = await row("Google Ads");
  expect(within(ads).getByText("Turning on")).toBeTruthy();
  expect(ads.hasAttribute("data-focused")).toBe(true);
  // GOOGLE ADS' API ACCESS IS THE IDENTITY PROJECT'S: the Explorer step links its page.
  const link = within(ads).getByRole("link", { name: "its Google Ads API page" });
  expect(link.getAttribute("href")).toContain("project=acme-crewlet");
});

test("Settings says the reader once per product and opens the sign-in client's own form", async () => {
  mount("#/company/integrations/google?tab=settings", {
    connection: "connected",
    reader: "missing",
    on: { google_analytics: [], google_ads: [] },
  });
  expect(await screen.findByRole("heading", { name: "Company reader" })).toBeTruthy();
  // THE EMAIL ONCE, and each product's role under it.
  expect(screen.getAllByText(READER)).toHaveLength(1);
  expect(screen.getAllByText("At Viewer")).toHaveLength(2);
  fireEvent.click(screen.getByRole("button", { name: "Add client" }));
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/google?edit=client"));
  expect(await screen.findByLabelText(/OAuth client ID/)).toBeTruthy();
  expect(screen.queryByLabelText(/Provisioner service account/)).toBeNull();
});

test("an off product turns on in place once Google is connected", async () => {
  const { writes } = mount("#/company/integrations/search_console", { connection: "connected" });
  fireEvent.click(await screen.findByRole("button", { name: /Turn on/ }));
  await waitFor(() =>
    expect(writes).toContainEqual({
      path: "POST /setup/integrations/search_console/inputs",
      body: { values: { enabled: "true" } },
    }),
  );
  expect(location.hash).toBe("#/company/integrations/search_console");
});

test("before Google is connected, an off product's Turn on goes to its row on Google", async () => {
  mount("#/company/integrations/search_console", { connection: "unfinished" });
  const link = await screen.findByRole("link", { name: "Turn on in Google" });
  expect(link.getAttribute("href")).toBe(googleAddress({ product: "search_console" }));
});

test("an on product names its connection and keeps only its own settings", async () => {
  mount("#/company/integrations/google_ads?tab=settings", {
    connection: "connected",
    on: { google_ads: [] },
    reader: "present",
  });
  const fact = await screen.findByRole("link", { name: "Google" });
  expect(fact.getAttribute("href")).toBe(googleAddress({ product: "google_ads" }));
  expect(screen.queryByRole("button", { name: "Edit settings" })).toBeNull();
  expect(screen.getByRole("link", { name: /The connection, the company reader/ })).toBeTruthy();
});

test("a product's own settings form address goes to Google", async () => {
  mount("#/company/integrations/google_analytics?edit=settings", {
    connection: "connected",
    on: { google_analytics: [] },
  });
  await waitFor(() =>
    expect(location.hash).toBe("#/company/integrations/google?product=google_analytics"),
  );
});

test("a written link keeps the Google page's tab and product, and nothing else", () => {
  expect(
    appRoute("#/company/integrations/google?tab=settings&product=admob&edit=client")?.canonical,
  ).toBe("#/company/integrations/google?tab=settings&product=admob");
  expect(appRoute("#/company/integrations/google?product=nope")?.canonical).toBe(
    "#/company/integrations/google",
  );
  expect(appRoute("#/company/integrations/admob?product=admob")?.canonical).toBe(
    "#/company/integrations/admob",
  );
});
