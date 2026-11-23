/**
 * Every vendor role is shown with what it allows and what Crewlet offers
 * through it, the held one marked; every connection with who Crewlet is
 * there, the roles it holds and each account whole.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { GrowthConnection, GrowthLevel, GrowthRoleOffer } from "~/contract/growth.ts";
import { GrowthConnections, accountStateWords, breakable } from "./GrowthConnections.tsx";
import { GrowthRoles, levelsWords } from "./GrowthRoles.tsx";

afterEach(cleanup);

const ROLES: GrowthRoleOffer[] = [
  {
    id: "viewer",
    name: "Viewer",
    note: "Sees reports and settings.",
    classes: ["read"],
    levels: ["read"],
  },
  {
    id: "marketer",
    name: "Marketer",
    note: "Edits audiences, key events and conversions.",
    classes: ["read", "operate"],
    levels: ["read", "operate"],
  },
  {
    id: "admin",
    name: "Administrator",
    note: "Manages users and the property.",
    classes: ["admin"],
    levels: ["administer"],
  },
  { id: "none", name: "No role", note: "No access to the property.", classes: [], levels: [] },
];

// COMPACT: the roles that back a level are the table, one line each, the held
// one marked; the rest fold away under one line. Mutation: draw every role in
// one table, and the unused one is a row of it.
test("each role Crewlet uses is one line with what it allows and the level it backs; the rest fold", () => {
  render(<GrowthRoles product="Google Analytics" roles={ROLES} held={["marketer"]} />);
  const used = screen.getByRole("table", { name: "Google Analytics roles Crewlet uses" });
  const rows = within(used).getAllByRole("row").slice(1);
  expect(rows).toHaveLength(3);
  expect(within(rows[1]!).getByText("Held")).toBeTruthy();
  expect(within(rows[0]!).queryByText("Held")).toBeNull();
  // The levels by what they let an agent do, the same names every growth product uses.
  expect(within(rows[1]!).getByText("View, Manage")).toBeTruthy();
  expect(within(rows[2]!).getByText("Administer, each act a decision")).toBeTruthy();
  expect(within(used).queryByText("No role")).toBeNull();
  expect(screen.getByText("Roles Crewlet does not use (1)")).toBeTruthy();
  expect(levelsWords([])).toBe("Not used by Crewlet");
});

const CONNECTIONS: GrowthConnection[] = [
  {
    id: "main",
    name: "Nimbus team",
    identity: "crewlet-g-growth-ab12@identity.example.com",
    state: "connected",
    roles_held: ["marketer"],
    accounts: [
      {
        product: "google_analytics",
        account: "123456789",
        account_name: "Nimbus web",
        currency: "EUR",
        time_zone: "Europe/Berlin",
        state: "serving",
      },
      {
        product: "google_analytics",
        account: "987654321",
        account_name: "",
        currency: "JPY",
        time_zone: "Asia/Tokyo",
        state: "suspended",
      },
    ],
    open: false,
  },
  {
    id: "eu",
    name: "",
    identity: "key 2X9R4HXF34",
    state: "connecting",
    roles_held: [],
    accounts: [],
    open: false,
  },
];

test("each connection says who Crewlet is there, the roles it holds and every account it reaches", () => {
  render(
    <GrowthConnections
      kind="google_analytics"
      product="Google Analytics"
      connections={CONNECTIONS}
      roles={ROLES}
    />,
  );
  const main = screen.getByRole("region", { name: "Nimbus team" });
  expect(within(main).getByText("crewlet-g-growth-ab12@identity.example.com")).toBeTruthy();
  expect(within(main).getByText("Marketer")).toBeTruthy();
  expect(within(main).getByText("Nimbus web")).toBeTruthy();
  expect(within(main).getAllByText("987654321")).toHaveLength(1);
  expect(within(main).getByText("Suspended by the vendor")).toBeTruthy();
  // Where each connection stands, in the engine's words.
  expect(within(main).getByText("connected")).toBeTruthy();
  const eu = screen.getByRole("region", { name: "eu" });
  expect(within(eu).getByText("connecting")).toBeTruthy();
  expect(within(eu).getByText("Holds no role Crewlet can read.")).toBeTruthy();
  expect(within(eu).getByText("Reaches no account yet.")).toBeTruthy();
  expect(accountStateWords("serving")).toBe("Serving");
  expect(accountStateWords("frozen")).toBe("frozen");
});

// AN APP STORE CONNECT TEAM SAYS WHAT ITS KEYS HOLD AND REACH: the role each
// key was added under, every app its keys reach where the company names none
// (by name, with the bundle ID and no serving state, which an app has none
// of), and, before Crewlet could list them, that it reaches every app its keys
// reach and why the list is not read. Mutation: drop `open`, and a working
// Admin key reads as reaching nothing.
test("an App Store Connect team names its key's role and the apps it reaches", () => {
  const roles: GrowthRoleOffer[] = [
    { id: "ADMIN", name: "Admin", note: "Everything.", classes: ["read"], levels: ["read"] },
  ];
  const team = (over: Partial<GrowthConnection>): GrowthConnection => ({
    id: "main",
    name: "",
    identity: "57246542-96fe-1a63-e053-0824d011072a",
    state: "",
    roles_held: ["ADMIN"],
    accounts: [],
    open: true,
    ...over,
  });
  const { unmount } = render(
    <GrowthConnections
      kind="app_store_connect"
      product="App Store Connect"
      roles={roles}
      connections={[team({ unread: "Add a Marketing key to list them here" })]}
    />,
  );
  let main = screen.getByRole("region", { name: "main" });
  expect(within(main).getByText("Admin")).toBeTruthy();
  expect(within(main).queryByText("Holds no role Crewlet can read.")).toBeNull();
  expect(within(main).getByText("Reaches every app its keys reach.")).toBeTruthy();
  expect(within(main).getByText("Add a Marketing key to list them here")).toBeTruthy();
  unmount();
  render(
    <GrowthConnections
      kind="app_store_connect"
      product="App Store Connect"
      roles={roles}
      connections={[
        team({
          accounts: [
            {
              product: "app_store_connect",
              connection: "main",
              account: "6763222842",
              account_name: "Field Notes",
              detail: "com.example.notes",
            },
          ],
        }),
      ]}
    />,
  );
  main = screen.getByRole("region", { name: "main" });
  const apps = within(main).getByRole("list", { name: "Apps reached through main" });
  expect(within(apps).getByText("Field Notes")).toBeTruthy();
  expect(within(apps).getByText("6763222842")).toBeTruthy();
  expect(within(apps).getByText("com.example.notes")).toBeTruthy();
  expect(within(apps).queryByText("State not read yet")).toBeNull();
});

test("a product with no connection says so", () => {
  render(<GrowthConnections kind="meta_ads" product="Meta Ads" connections={[]} roles={[]} />);
  expect(screen.getByText("Meta Ads has no connection yet.")).toBeTruthy();
});

test("an identity too long for a phone may break after its own @, dots and dashes, and reads whole", () => {
  const { container } = render(<span>{breakable("crewlet-g-lead@id.example.com")}</span>);
  expect(container.textContent).toBe("crewlet-g-lead@id.example.com");
  expect(container.querySelectorAll("wbr")).toHaveLength(5);
});

test("a level this build does not know is said as sent, never left blank", () => {
  expect(levelsWords(["read", "supervise" as GrowthLevel])).toBe(
    `${levelsWords(["read"])}, supervise`,
  );
});
