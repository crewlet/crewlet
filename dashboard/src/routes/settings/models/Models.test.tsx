/**
 * Settings › Models is open to every member and writable by an admin alone.
 *
 * The invariant: no write control reaches a reader without admin, however the
 * page is reached. The list, the alarms and By job each hide their own
 * controls (their suites read that); this reads the page whole, at every
 * address a flow opens at, so a member who follows a link or edits the URL to
 * `?flow=` gets the reading and nothing else. The engine decides every write
 * again on its own (`internal/authz`), so this is the page saying what is
 * true, not the only guard.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import {
  EVERY_JOB,
  MODELS_NOW,
  at,
  modelEntry,
  modelsData,
  runs,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "./data.ts";
import { Models } from "./Models.tsx";

let current: ModelsData;
vi.mock("./data.ts", async (actual) => ({
  ...(await actual<typeof import("./data.ts")>()),
  useModelsData: () => current,
}));
// NOTHING IS SENT: every write is a request that never answers, so a flow
// that opens stays open for the test to see, and none completes.
vi.mock("./api.ts", async (actual) => {
  const real = await actual<typeof import("./api.ts")>();
  const pending = () => new Promise(() => {});
  return {
    ...real,
    testCredential: vi.fn(pending),
    storeCredential: vi.fn(pending),
    checkModel: vi.fn(pending),
    retryModel: vi.fn(pending),
    checkHostLogin: vi.fn(pending),
    startDeviceSignIn: vi.fn(pending),
    cancelDeviceSignIn: vi.fn(pending),
    removeModel: vi.fn(pending),
    writeDocument: vi.fn(pending),
  };
});
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({ data: null, loading: false, error: null, refusal: null, refetch: () => {} }),
}));

/** A company whose one limit holds seats, so every control the page has is drawn for an admin. */
function company(admin: boolean): ModelsData {
  return modelsData({
    admin,
    entries: [
      modelEntry({
        key: "default",
        health: "sitting_out",
        plan: "plan-a",
        sitting_out: {
          until: at(90),
          resets_at: null,
          window: "session",
          since: at(-10),
          nodes: ["node-1"],
        },
        used_by: runs("backend-engineer", ...EVERY_JOB),
      }),
      modelEntry({ key: "haiku", model: "claude-haiku-4-5" }),
    ],
    alarms: admin
      ? [
          {
            kind: "llm_limit",
            detail: "default sits out its session limit",
            remedy: "Wait.",
            entries: [{ key: "default", detail: "default sits out its session limit" }],
          },
        ]
      : [],
  });
}

/** Every address a flow opens at, on the list and on a model's page. */
const ADDRESSES: { hash: string; id?: string }[] = [
  { hash: "#/settings/models" },
  { hash: "#/settings/models?view=jobs" },
  { hash: "#/settings/models?flow=add&step=cli" },
  { hash: "#/settings/models?flow=add&step=assign&add=coding:pm" },
  { hash: "#/settings/models?flow=edit&entry=haiku&step=assign&add=coding:backend-engineer" },
  { hash: "#/settings/models?flow=test&entry=haiku" },
  { hash: "#/settings/models?flow=retry&entry=default" },
  { hash: "#/settings/models?flow=remove&entry=haiku" },
  { hash: "#/settings/models?flow=device&entry=haiku" },
  { hash: "#/settings/models/haiku", id: "haiku" },
  { hash: "#/settings/models/haiku?flow=edit&step=signin&focus=credential", id: "haiku" },
  { hash: "#/settings/models/haiku?flow=test", id: "haiku" },
  { hash: "#/settings/models/haiku?flow=remove", id: "haiku" },
];

/** What only a writer may meet: a flow's surface, or a control that opens one. */
function writeControls(): string[] {
  const found: string[] = [];
  if (document.querySelector(".crewlet-setup-wizard")) found.push("the model wizard");
  for (const dialog of screen.queryAllByRole("dialog"))
    found.push(`dialog "${dialog.getAttribute("aria-label") ?? dialog.textContent?.slice(0, 40)}"`);
  for (const button of screen.queryAllByRole("button")) {
    const name = button.getAttribute("aria-label") ?? button.textContent ?? "";
    if (
      /^(Add model|Use a different model for these seats|Retry now|Test now|Remove|Edit)$/.test(
        name,
      ) ||
      name.startsWith("Actions for")
    )
      found.push(`button "${name}"`);
  }
  if (screen.queryAllByRole("combobox").length > 0) found.push("a model picker");
  return found;
}

function mountAt(address: { hash: string; id?: string }) {
  location.hash = address.hash;
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <Models {...(address.id ? { id: address.id } : {})} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
}

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

test("a member meets no write control at any address, a flow's included", () => {
  current = company(false);
  for (const address of ADDRESSES) {
    mountAt(address);
    expect(writeControls(), address.hash).toEqual([]);
    cleanup();
  }
});

// THE OTHER SIDE, so the reading above cannot pass by finding nothing at all:
// the same addresses draw an admin's controls.
test("an admin meets the controls and the flows at the same addresses", () => {
  current = company(true);
  for (const address of ADDRESSES) {
    mountAt(address);
    expect(writeControls().length, address.hash).toBeGreaterThan(0);
    cleanup();
  }
});
