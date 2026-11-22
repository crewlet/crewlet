/**
 * A growth product's page reads its connections and roles from the company
 * answer, leaving out a role it cannot match and drawing every connection, and
 * offers the caps editor exactly the accounts the vendor reports, once each.
 */

import { cleanup, render, screen } from "@testing-library/react";
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
