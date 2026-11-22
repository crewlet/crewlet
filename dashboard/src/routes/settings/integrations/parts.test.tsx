/**
 * What an app's page lists: what to have ready before connecting, and on a
 * connected integration's Settings tab what each setting holds, in the face
 * its kind is read in.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { SetupRequirement, SetupToolState } from "~/protocol/index.ts";
import { BeforeYouStart, SettingsSummary } from "./parts.tsx";
import { CATALOG } from "./catalogue.tsx";
import { deriveTool, type Tool } from "./model.ts";

afterEach(cleanup);

function req(over: Partial<SetupRequirement>): SetupRequirement {
  return {
    field: "f",
    label: "F",
    kind: "text",
    config_path: "integrations.github.f",
    required: false,
    present: true,
    ...over,
  };
}

// A CHOICE IS READ BY ITS LABEL, IN WORDS; an identifier stays monospace.
// Mutation: wrap every value in `.mono` again and the choice's label is code.
test("a choice reads in words and an identifier in monospace", () => {
  const tool = {
    sections: [
      {
        name: "GitHub",
        tool: {
          key: "github",
          configured: true,
          enabled: true,
          satisfied: true,
          form_complete: true,
          requirements: [
            req({ field: "org", label: "Organization", kind: "id", value: "acme" }),
            req({
              field: "reach",
              label: "Which activity reaches your agents",
              kind: "choice",
              value: "installed",
              choices: [
                { value: "installed", label: "Repositories where an agent's app is installed" },
              ],
            }),
          ],
        },
      },
    ],
  } as unknown as Tool;
  render(<SettingsSummary tool={tool} />);
  expect(screen.getByText("acme").className).toContain("mono");
  expect(
    screen.getByText("Repositories where an agent's app is installed").className,
  ).not.toContain("mono");
});

const ROLES: [string, string][] = [
  ["marketing", "Marketing"],
  ["admin", "Admin"],
  ["app_manager", "App Manager"],
  ["developer", "Developer"],
  ["finance", "Finance"],
  ["sales", "Sales"],
  ["customer_support", "Customer Support"],
  ["access_to_reports", "Access to Reports"],
];

const HINT = "A key ID and .p8 file per Apple role: at least one, Marketing first.";

/** App Store Connect as the engine lists it: per connection an issuer, a vendor number and a key set by role. */
function appStore(connections: string[], held: string[] = []): SetupToolState {
  const requirements = connections.flatMap((id) => {
    const base = `integrations.app_store_connect.connections.${id}`;
    return [
      req({
        field: `${id}_issuer_id`,
        label: "Issuer ID",
        kind: "id",
        connect: true,
        required: true,
        config_path: `${base}.issuer_id`,
        present: held.length > 0,
        value: held.length > 0 ? "57246542-96fe-1a63-e053-0824d011072a" : undefined,
      }),
      req({
        field: `${id}_vendor_number`,
        label: "Vendor number",
        kind: "id",
        present: false,
        config_path: `${base}.vendor_number`,
      }),
      ...ROLES.flatMap(([slot, role]) => {
        const set = {
          id: `${id}_keys`,
          label: "API keys",
          item: "key",
          hint: HINT,
          add: "Add a key",
          pick: "Role",
          min: 1,
          entry: { value: slot, label: role },
        };
        const present = held.includes(slot);
        return [
          req({
            field: `${id}_${slot}_key_id`,
            label: "Key ID",
            kind: "id",
            connect: true,
            set,
            config_path: `${base}.keys.${slot}.key_id`,
            present,
            value: present ? "ABC123DEFG" : undefined,
          }),
          req({
            field: `${id}_${slot}_private_key`,
            label: "Private key (.p8)",
            kind: "secret",
            connect: true,
            set,
            config_path: `${base}.keys.${slot}.private_key`,
            present,
          }),
        ];
      }),
    ];
  });
  return {
    key: "app_store_connect",
    configured: held.length > 0,
    enabled: true,
    satisfied: false,
    form_complete: false,
    requirements,
  };
}

function appStoreTool(state: SetupToolState): Tool {
  return deriveTool(
    CATALOG.find((e) => e.key === "app_store_connect")!,
    {
      rows: new Map(),
      rollups: undefined,
      setup: new Map([["app_store_connect", state]]),
      guarded: false,
      trafficKnown: true,
      seatOf: () => undefined,
      avatarOf: () => null,
    },
  );
}

// A SET IS ONE ITEM, in the words it heads the form with, and no item is said
// twice, however many connections the company names. Mutation: list the
// connect fields one by one, and Have ready reads Issuer ID, then Key ID and
// Private key (.p8) once per role.
test("Have ready lists a key set as one item that says what it needs, and nothing twice", () => {
  for (const connections of [["main"], ["main", "eu"]]) {
    render(<BeforeYouStart tool={appStoreTool(appStore(connections))} />);
    const ready = screen.getByRole("region", { name: "What you need" });
    const items = within(ready).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(["Issuer ID", `API keys${HINT}`]);
    expect(within(ready).getByText(HINT).className).toContain("t-caption");
    expect(ready.textContent).not.toMatch(/Key ID|Private key/);
    cleanup();
  }
});

// THE SETTINGS TAB SAYS A SET ONCE TOO: one row naming the keys held, or the
// keys drawn by the caller in that row. Mutation: list each member's label,
// and the tab reads Key ID and Private key (.p8) eight times.
test("a set is one Settings row naming the entries held, or drawn by the caller", () => {
  const tool = appStoreTool(appStore(["main"], ["marketing", "finance"]));
  const { container } = render(<SettingsSummary tool={tool} />);
  const terms = [...container.querySelectorAll("dt")].map((dt) => dt.textContent);
  expect(terms).toEqual(["Issuer ID", "Vendor number", "API keys"]);
  expect(screen.getByText("Marketing and Finance")).toBeDefined();
  cleanup();

  render(
    <SettingsSummary
      tool={tool}
      drawSet={(set, held) => <span>{`${set.item}: ${held.map((e) => e.value).join(",")}`}</span>}
    />,
  );
  expect(screen.getByText("key: marketing,finance")).toBeDefined();
});
