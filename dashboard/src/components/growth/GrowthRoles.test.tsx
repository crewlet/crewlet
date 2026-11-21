/**
 * Every vendor role is shown with what it allows and what Crewlet offers
 * through it, the held one marked; every connection with who Crewlet is
 * there, the roles it holds and each account whole.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { GrowthConnection, GrowthRoleOffer } from "~/contract/growth.ts";
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

test("every role is a row with what it allows and what Crewlet offers, the held one marked", () => {
  render(<GrowthRoles product="Google Analytics" roles={ROLES} held={["marketer"]} />);
  const rows = screen.getAllByRole("row").slice(1);
  expect(rows).toHaveLength(4);
  expect(within(rows[1]!).getByText("Held")).toBeTruthy();
  expect(within(rows[0]!).queryByText("Held")).toBeNull();
  expect(within(rows[1]!).getByText("Read, Operate")).toBeTruthy();
  expect(within(rows[2]!).getByText("Administer, each act a decision")).toBeTruthy();
  expect(within(rows[3]!).getByText("Not used by Crewlet")).toBeTruthy();
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
  },
  {
    id: "eu",
    name: "",
    identity: "key 2X9R4HXF34",
    state: "connecting",
    roles_held: [],
    accounts: [],
  },
];

test("each connection says who Crewlet is there, the roles it holds and every account it reaches", () => {
  render(<GrowthConnections product="Google Analytics" connections={CONNECTIONS} roles={ROLES} />);
  const main = screen.getByRole("region", { name: "Nimbus team" });
  expect(within(main).getByText("crewlet-g-growth-ab12@identity.example.com")).toBeTruthy();
  expect(within(main).getByText("Marketer")).toBeTruthy();
  expect(within(main).getByText("Nimbus web")).toBeTruthy();
  expect(within(main).getAllByText("987654321")).toHaveLength(1);
  expect(within(main).getByText("Suspended by the vendor")).toBeTruthy();
  const eu = screen.getByRole("region", { name: "eu" });
  expect(within(eu).getByText("Holds no role Crewlet can read.")).toBeTruthy();
  expect(within(eu).getByText("Reaches no account yet.")).toBeTruthy();
  expect(accountStateWords("serving")).toBe("Serving");
  expect(accountStateWords("frozen")).toBe("frozen");
});

test("a product with no connection says so", () => {
  render(<GrowthConnections product="Meta Ads" connections={[]} roles={[]} />);
  expect(screen.getByText("Meta Ads has no connection yet.")).toBeTruthy();
});

test("an identity too long for a phone may break after its own @, dots and dashes, and reads whole", () => {
  const { container } = render(<span>{breakable("crewlet-g-lead@id.example.com")}</span>);
  expect(container.textContent).toBe("crewlet-g-lead@id.example.com");
  expect(container.querySelectorAll("wbr")).toHaveLength(5);
});
