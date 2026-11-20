/**
 * Work the Advisor moves, in place: the press re-checks the reassignment,
 * shows each item going to the seat the finding suggests, and Confirm moves
 * each through the work item's own write at the version the re-check read,
 * before the finding is reported applied. Over the engine's routes stubbed at
 * `fetch`, as a reader who may reassign work.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { ClientContext, useConnection } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ApplyPanel } from "./ApplyPanel.tsx";
import { REASSIGN } from "./view.ts";
import type { AdvisorKeptFinding, AdvisorRecheck } from "~/protocol/advisorFindings.ts";

vi.mock("~/lib/store-hooks.ts", async (original) => ({
  ...(await original<typeof import("~/lib/store-hooks.ts")>()),
  useConnection: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", async (original) => ({
  ...(await original<typeof import("~/lib/viewer.ts")>()),
  useViewer: vi.fn(),
}));

const ADMIN = {
  login: "founder",
  grants: ["admin", "worklet"],
  admin: true,
  worklet: true,
  doclet: true,
  chatlet: true,
  gitlet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: ["update_work_item"],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  failed: null,
  failedRefusal: null,
  configWriter: true,
  configManagedBy: [],
  assistant: "no_model",
  assistantModel: null,
  assistantReadsImages: false,
  assistantCanAssign: false,
  chatVendors: [],
} as ViewerState;

const ID = "5a1c9d0e-2b3f-4c5d-8e7f-90a1b2c3d4e5";

const FINDING = {
  id: ID,
  class: "work.imbalance",
  state: "open",
  title:
    "One seat holds most of its job's open work while another seat doing that job holds little.",
  headline: "Dev holds 27 open items while Dev 2, on the same job, holds 2.",
  why: "Work queued on the busy seat waits its turn there.",
  subject: { kind: "seat", id: "dev", name: "Dev", seat: "dev", unit: "", schedule: "" },
  impact: { hours_per_week: null, tokens_per_week: null },
  fix: { kind: "open_page", sentence: "Move 2 items not yet started to Dev 2.", options: [] },
  actions: { primary: null, secondary: [] },
  close_reason: null,
  effect_due_at: null,
} as unknown as AdvisorKeptFinding;

const MOVES = [
  {
    item: "ENG-1",
    from: "dev",
    to: "dev-2",
    version: 4,
    reason: "Moved here on the Advisor's finding.",
  },
  {
    item: "ENG-4",
    from: "dev",
    to: "dev-2",
    version: 6,
    reason: "Moved here on the Advisor's finding.",
  },
];

const CHECK: AdvisorRecheck = {
  outcome: "holds",
  finding: FINDING,
  moved: [],
  previous_fix: null,
  preview: {
    rows: [
      { field: "ENG-1.assignee", label: "ENG-1 Fix the login", before: "Dev", after: "Dev 2" },
      { field: "ENG-4.assignee", label: "ENG-4 Add the export", before: "Dev", after: "Dev 2" },
    ],
    notes: [],
  },
  send: null,
  sends: [],
  moves: MOVES,
  digest: "sha256:move",
  checked_at: new Date().toISOString(),
  expires_at: new Date(Date.now() + 10 * 60_000).toISOString(),
  refusal: null,
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

let sent: { path: string; body: unknown }[];

beforeEach(() => {
  sent = [];
  vi.mocked(useViewer).mockReturnValue(ADMIN);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const url = new URL(input);
      sent.push({ path: url.pathname, body: init.body ? JSON.parse(String(init.body)) : null });
      if (url.pathname.endsWith("/recheck")) return json(CHECK);
      if (url.pathname.endsWith("/applied")) {
        return json({ ...FINDING, state: "applied", effect_due_at: "2026-11-27T09:00:00Z" });
      }
      if (url.pathname === "/operator/act/update_work_item") {
        return json({
          tool: "update_work_item",
          outcome: "applied",
          position: "T@1:9",
          receipt: {},
        });
      }
      return json({ error: "not_found" }, 404);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function mount() {
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: [
        { name: "Dev", handle: "dev", kind: "agent" },
        { name: "Dev 2", handle: "dev-2", kind: "agent" },
        { name: "QA", handle: "qa", kind: "agent" },
      ],
    }) as never,
  );
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve(FINDING);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <LayerHost>
        <ApplyPanel finding={FINDING} option={REASSIGN} onClose={() => {}} onSettled={vi.fn()} />
      </LayerHost>
    </ClientContext.Provider>,
  );
}

test("work to move is re-checked, shown going to the suggested seat, and moved item by item", async () => {
  mount();
  await screen.findByText("It still holds");
  expect(sent[0]).toEqual({
    path: `/advisor/findings/${ID}/recheck`,
    body: { action: "reassign" },
  });
  expect(screen.getByText("ENG-1 Fix the login")).toBeDefined();
  expect(screen.getAllByText("Dev 2").length).toBeGreaterThan(0);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  });
  await screen.findByText(/checks its effect/);
  const moved = sent
    .filter((s) => s.path === "/operator/act/update_work_item")
    .map((s) => (s.body as { args: unknown }).args);
  expect(moved).toEqual([
    { item: "ENG-1", assignee: "dev-2", if_match: 4, reason: MOVES[0]!.reason },
    { item: "ENG-4", assignee: "dev-2", if_match: 6, reason: MOVES[1]!.reason },
  ]);
  expect(sent.at(-1)!.path).toBe(`/advisor/findings/${ID}/applied`);
});
