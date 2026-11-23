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

/** App Store Connect as listed: the hidden toggle, and per connection its team's Issuer ID. */
function appStore(connections: string[]): SetupToolState {
  return {
    key: "app_store_connect",
    configured: false,
    enabled: true,
    satisfied: false,
    form_complete: false,
    requirements: [
      req({
        field: "enabled",
        label: "Connect App Store Connect",
        kind: "toggle",
        hidden: true,
        required: true,
        present: false,
        default: "true",
        config_path: "integrations.app_store_connect.enabled",
      }),
      ...connections.map((id) =>
        req({
          field: `${id}_issuer_id`,
          label: "Issuer ID",
          kind: "id",
          connect: true,
          required: true,
          present: false,
          config_path: `integrations.app_store_connect.connections.${id}.issuer_id`,
        }),
      ),
    ],
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

// NO QUESTION IS SAID TWICE in Have ready, however many connections the
// company names, and a hidden field is never one to have ready. Mutation: list
// each connection's fields one by one, and Have ready reads Issuer ID twice.
test("Have ready says each question once, however many connections ask it", () => {
  for (const connections of [["main"], ["main", "eu"]]) {
    render(<BeforeYouStart tool={appStoreTool(appStore(connections))} />);
    const ready = screen.getByRole("region", { name: "What you need" });
    const items = within(ready).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(["Issuer ID"]);
    cleanup();
  }
});
