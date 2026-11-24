/**
 * Settings › Models & Fleets is open to every member and writable by an admin alone.
 *
 * The invariant: no write control reaches a reader without admin, however the
 * page is reached. The list, the alarms and the seats' grid each hide their own
 * controls (their suites read that); this reads the page whole, at every
 * address a flow opens at, so a member who follows a link or edits the URL to
 * `?flow=` gets the reading and nothing else. The engine decides every write
 * again on its own (`internal/authz`), so this is the page saying what is
 * true, not the only guard.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { ScreenBoundary } from "~/app/boundaries.tsx";
import { loadChunk } from "~/app/lazyScreen.ts";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
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
  { hash: "#/settings/models?fleet=new" },
  { hash: "#/settings/models?fleet=default-fleet&step=members" },
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
      /^(Add model|Create fleet|Use a different model for these seats|Retry now|Test now|Remove|Edit)$/.test(
        name,
      ) ||
      name.startsWith("Actions for") ||
      name.startsWith("Add members to") ||
      /^Remove .+ from /.test(name)
    )
      found.push(`button "${name}"`);
  }
  // A PICKER A READER CANNOT OPEN is a value, as Who runs draws it for a member.
  const open = screen
    .queryAllByRole("combobox")
    .filter((c) => c.getAttribute("aria-disabled") !== "true" && !c.hasAttribute("disabled"));
  if (open.length > 0) found.push("a model picker");
  if (document.querySelector(".crewlet-setup-wizard")?.textContent?.includes("fleet"))
    found.push("the fleet flow");
  return found;
}

async function mountAt(address: { hash: string; id?: string }) {
  location.hash = address.hash;
  // AWAITED: the wizard is a chunk of its own, drawn once its region has read it.
  await act(async () => {
    render(
      <TestClient>
        <LayerHost>
          <Router>
            <ScreenBoundary resetKey="models">
              <Models {...(address.id ? { id: address.id } : {})} />
            </ScreenBoundary>
          </Router>
        </LayerHost>
      </TestClient>,
    );
  });
}

// THE WIZARDS ARE CHUNKS OF THEIR OWN, in memory before a case draws one, as the idle prefetch leaves them.
beforeAll(async () => {
  await loadChunk("model-wizard");
  await loadChunk("fleet-wizard");
});

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

test("a member meets no write control at any address, a flow's included", async () => {
  current = company(false);
  for (const address of ADDRESSES) {
    await mountAt(address);
    expect(writeControls(), address.hash).toEqual([]);
    cleanup();
  }
});

// THE OTHER SIDE, so the reading above cannot pass by finding nothing at all:
// the same addresses draw an admin's controls.
test("an admin meets the controls and the flows at the same addresses", async () => {
  current = company(true);
  for (const address of ADDRESSES) {
    await mountAt(address);
    expect(writeControls().length, address.hash).toBeGreaterThan(0);
    cleanup();
  }
});

// A FLOW IS A CLAIM ABOUT THE MODEL: a stale or hand-edited link that asks to
// retry a model that is not sitting out, or to sign a Claude model in with a
// Codex device code, opens nothing, leaves the address, and says why.
test("a flow the model does not fit is dropped from the address with its reason, not drawn", async () => {
  current = company(true);
  const misfits = [
    { hash: "#/settings/models?flow=retry&entry=haiku", says: /haiku is not sitting out/ },
    {
      hash: "#/settings/models?flow=device&entry=haiku",
      says: /haiku does not sign in with a device code/,
    },
    { hash: "#/settings/models/haiku?flow=retry", id: "haiku", says: /haiku is not sitting out/ },
  ];
  for (const m of misfits) {
    location.hash = m.hash;
    render(
      <TestClient>
        <LayerHost>
          <ToastProvider>
            <Router>
              <Models {...(m.id ? { id: m.id } : {})} />
            </Router>
          </ToastProvider>
        </LayerHost>
      </TestClient>,
    );
    expect(screen.queryAllByRole("dialog"), m.hash).toEqual([]);
    expect(location.hash, m.hash).not.toContain("flow=");
    expect(document.body.textContent, m.hash).toMatch(m.says);
    cleanup();
  }
  // The same address on a model it fits still opens its dialog.
  await mountAt({ hash: "#/settings/models?flow=retry&entry=default" });
  expect(screen.getByRole("dialog")).toBeTruthy();
});

// A MODEL JUST ADDED IS BEING ADDED, never missing: the wizard opens its page
// before the engine's reading holds it, and the mark clears once it does.
test("a model just added reads as being added until the reading holds it", async () => {
  current = company(true);
  await mountAt({ hash: "#/settings/models/fresh?added=fresh", id: "fresh" });
  expect(screen.getByText("Adding fresh")).toBeTruthy();
  expect(screen.queryByText("No model called fresh")).toBeNull();
  cleanup();

  await mountAt({ hash: "#/settings/models/fresh", id: "fresh" });
  expect(screen.getByText("No model called fresh")).toBeTruthy();
  cleanup();

  current = modelsData({ admin: true, entries: [modelEntry({ key: "fresh" })] });
  await mountAt({ hash: "#/settings/models/fresh?added=fresh", id: "fresh" });
  expect(location.hash).not.toContain("added=");
});

// ONE PAGE, NO LENSES: under the table of models, Ask Crewlet and the
// Advisor, then the seats, in that order.
test("the page has no lens tabs, and under the models come the fleets, then who runs", async () => {
  current = company(true);
  await mountAt({ hash: "#/settings/models" });
  expect(screen.queryByRole("tablist", { name: "Lens" })).toBeNull();
  expect(screen.queryByRole("tab", { name: "By job" })).toBeNull();
  const models = screen.getByRole("table");
  const fleets = screen.getByRole("treegrid", { name: "Fleets and their members" });
  const who = screen.getByRole("treegrid", { name: "Consumers and what each runs on" });
  const follows = (a: Node, b: Node) =>
    (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
  expect(follows(models, fleets)).toBe(true);
  expect(follows(fleets, who)).toBe(true);
});

// THE OLD LENS ADDRESS still works: it opens the one page, whose grid it asked for.
test("an address for the old By job lens opens the one page", async () => {
  current = company(true);
  await mountAt({ hash: "#/settings/models?view=jobs" });
  expect(location.hash).not.toContain("view=");
  expect(screen.getByRole("treegrid", { name: "Consumers and what each runs on" })).toBeTruthy();
  expect(screen.getByRole("table")).toBeTruthy();
});
