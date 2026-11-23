/**
 * A growth product's page reads its connections and roles from the company
 * answer, leaving out a role it cannot match and drawing every connection, and
 * offers the caps editor exactly the accounts the vendor reports, once each.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { IntegrationRow } from "~/contract/integrations.ts";
import { GrowthProductSection, capAccountsOf, growthDetailsOf } from "./GrowthProduct.tsx";

const viewer = vi.hoisted(() => ({ admin: false }));
vi.mock("~/lib/viewer.ts", async (original) => ({
  ...(await original<typeof import("~/lib/viewer.ts")>()),
  useViewer: () => viewer,
}));
vi.mock("./RecentChanges.tsx", () => ({
  RecentChanges: ({ product, accounts }: { product: string; accounts: Map<string, string> }) => (
    <p>
      changes of {product} in {[...accounts.values()].join(",")}
    </p>
  ),
}));

afterEach(cleanup);

const ROW = {
  key: "google_analytics",
  configured: true,
  connections: [
    {
      id: "main",
      name: "Nimbus",
      identity: "crewlet-gr-nimbus@identity.example.com",
      state: "connected",
      roles_held: ["viewer"],
      accounts: [
        {
          product: "google_analytics",
          account: "123",
          account_name: "Web",
          currency: "EUR",
          time_zone: "Europe/Berlin",
          state: "serving",
        },
        { product: "google_analytics", account: "456", account_name: "App", state: "paused" },
        "not an account",
      ],
    },
    {
      id: "eu",
      name: "",
      identity: "x",
      state: "connected",
      roles_held: [],
      accounts: [
        {
          product: "google_analytics",
          account: "123",
          account_name: "Web again",
          state: "serving",
        },
      ],
    },
    { name: "Sole connection", identity: "reader@example.com", accounts: [] },
  ],
  roles: [
    { id: "viewer", name: "Viewer", note: "Sees reports.", classes: ["read"], levels: ["read"] },
    { name: "no id" },
  ],
} as unknown as IntegrationRow;

test("connections and roles are read from the row; a connection without an id is still drawn, a role without one is not", () => {
  const { connections, roles } = growthDetailsOf(ROW);
  expect(connections.map((c) => c.id)).toEqual(["main", "eu", ""]);
  expect(connections[2]).toMatchObject({ name: "Sole connection", roles_held: [], accounts: [] });
  expect(connections[0]!.accounts.map((a) => a.account)).toEqual(["123", "456"]);
  expect(connections[0]!.accounts[1]).toMatchObject({ state: "paused" });
  expect(roles.map((r) => r.id)).toEqual(["viewer"]);
  expect(growthDetailsOf(undefined)).toEqual({ connections: [], roles: [] });
  expect(
    growthDetailsOf({
      key: "x",
      configured: false,
      connections: "nope",
    } as unknown as IntegrationRow).connections,
  ).toEqual([]);
});

test("the caps editor is offered each reported account once, with the vendor's currency", () => {
  expect(capAccountsOf(growthDetailsOf(ROW).connections)).toEqual([
    { id: "123", name: "Web", currency: "EUR" },
    { id: "456", name: "App", currency: "" },
  ]);
});

test("a measuring product shows its connections and roles, the held one marked, and no caps", () => {
  render(<GrowthProductSection kind="google_analytics" product="Google Analytics" row={ROW} />);
  expect(screen.getByRole("region", { name: "Connections" })).toBeTruthy();
  expect(screen.getByRole("region", { name: "Google Analytics roles" })).toBeTruthy();
  expect(screen.getByText("Held")).toBeTruthy();
  expect(screen.queryByText("Spending caps")).toBeNull();
  expect(screen.getByRole("region", { name: "Sole connection" })).toBeTruthy();
  // Each connection is labelled by its own heading, whatever its id.
  expect(screen.getAllByRole("heading", { level: 4 }).map((h) => h.id)).toHaveLength(3);
  expect(new Set(screen.getAllByRole("heading", { level: 4 }).map((h) => h.id)).size).toBe(3);
});

test("an administrator reads the product's recent agent changes there; nobody else is shown them", () => {
  viewer.admin = false;
  const { unmount } = render(
    <GrowthProductSection kind="google_analytics" product="Google Analytics" row={ROW} />,
  );
  expect(screen.queryByRole("region", { name: "Recent agent changes" })).toBeNull();
  unmount();
  viewer.admin = true;
  render(<GrowthProductSection kind="google_analytics" product="Google Analytics" row={ROW} />);
  const changes = screen.getByRole("region", { name: "Recent agent changes" });
  // For this product, with each reported account's currency.
  expect(changes.textContent).toContain("changes of google_analytics in EUR,");
});

test("Google Analytics' page shows the Google Analytics logo under its connections, linked into Google Analytics", () => {
  render(<GrowthProductSection kind="google_analytics" product="Google Analytics" row={ROW} />);
  const connections = screen.getByRole("region", { name: "Connections" });
  const link = screen.getByRole("link", { name: "Google Analytics (opens in a new tab)" });
  expect(connections.contains(link)).toBe(true);
  expect(link.getAttribute("href")).toBe("https://analytics.google.com/");
  // One file per ground, each at the policy's 50px height.
  const logos = [...link.querySelectorAll("img")];
  expect(logos.map((img) => img.getAttribute("src"))).toEqual([
    expect.stringContaining("lockup_ic_Analytics_horiz_272px_clr"),
    expect.stringContaining("lockup_ic_Analytics_horiz_272px_wht"),
  ]);
  expect(logos.map((img) => [img.getAttribute("width"), img.getAttribute("height")])).toEqual([
    ["189", "50"],
    ["189", "50"],
  ]);
});

test("the logo waits for data, and no other product shows it", () => {
  const { unmount } = render(
    <GrowthProductSection
      kind="google_analytics"
      product="Google Analytics"
      row={{ ...ROW, connections: [] } as IntegrationRow}
    />,
  );
  expect(screen.queryByRole("link", { name: /Google Analytics/ })).toBeNull();
  unmount();
  render(
    <GrowthProductSection
      kind="search_console"
      product="Search Console"
      row={{ ...ROW, key: "search_console" } as IntegrationRow}
    />,
  );
  expect(screen.queryByRole("link", { name: /Google Analytics/ })).toBeNull();
  expect(document.querySelector("img")).toBeNull();
});

// APP STORE CONNECT'S LONE TEAM IS ITS APPS: the issuer and the key's role are
// the settings above, so the section is titled by what it reaches and draws
// only that, and the roles reference waits closed. Mutation: drop `bare`, and
// the issuer and the role's tag are drawn twice on the page.
test("a lone App Store Connect team is titled by its apps, and the roles wait closed", () => {
  const row = {
    key: "app_store_connect",
    configured: true,
    connections: [
      {
        id: "main",
        identity: "57246542-96fe-1a63-e053-0824d011072a",
        roles_held: ["ADMIN"],
        accounts: [{ product: "app_store_connect", account: "6763222842", account_name: "" }],
        unread: "App names need a key of any role but Admin",
      },
    ],
    roles: [
      { id: "ADMIN", name: "Admin", note: "Everything.", classes: ["read"], levels: ["read"] },
    ],
  } as unknown as IntegrationRow;
  render(<GrowthProductSection kind="app_store_connect" product="App Store Connect" row={row} />);
  const apps = screen.getByRole("region", { name: "Apps" });
  expect(within(apps).getByText("6763222842")).toBeTruthy();
  expect(within(apps).getByText("App names need a key of any role but Admin")).toBeTruthy();
  expect(within(apps).queryByText("57246542-96fe-1a63-e053-0824d011072a")).toBeNull();
  expect(within(apps).queryByText("Admin")).toBeNull();
  expect(screen.queryByRole("region", { name: "Connections" })).toBeNull();
  const roles = screen.getByRole("region", { name: "App Store Connect roles" });
  const fold = within(roles).getByRole("button", { name: "What each role allows" });
  expect(fold.getAttribute("aria-expanded")).toBe("false");
  fireEvent.click(fold);
  expect(
    within(roles).getByRole("table", { name: "App Store Connect roles Crewlet uses" }),
  ).toBeTruthy();
});
