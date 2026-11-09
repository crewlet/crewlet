/**
 * Settings › Advisor. The invariants worth breaking a build over: the owner's
 * findings are listed and the engine's never read as the owner's to-do; a change
 * is previewed from the engine's own dry run of exactly the entity it writes,
 * with only the warnings the change adds; nothing is written until Apply, and
 * Apply writes that entity under its own tag with the change's summary; a
 * refused write is said, never swallowed; and a reader who may not write
 * cannot apply.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import type { AdvisorAnswer, AdvisorFinding } from "~/protocol/advisor.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Advisor } from "./Advisor.tsx";

const getEntity = vi.hoisted(() => vi.fn());
const dryRunEntity = vi.hoisted(() => vi.fn());
const putEntity = vi.hoisted(() => vi.fn());
vi.mock("~/protocol/configWrite.ts", async (original) => ({
  ...(await original<typeof import("~/protocol/configWrite.ts")>()),
  getEntity,
  dryRunEntity,
  putEntity,
}));

const access = vi.hoisted(() => vi.fn());
vi.mock("~/lib/useWriteAccess.ts", async (original) => ({
  ...(await original<typeof import("~/lib/useWriteAccess.ts")>()),
  useConfigWriteAccess: access,
}));

const HEALTH: AdvisorFinding = {
  class: "grant.health_unheld",
  severity: "costs",
  actor: "owner",
  source: "static",
  area: "access",
  subject: { kind: "company" },
  title: "No agent watches the engine's own alarms.",
  why: "An alarm that fires on a server reaches nobody.",
  path: "roles[0].health_tools",
  evidence: [],
  fix: {
    kind: "config_change",
    sentence: "Let one agent watch the engine's health and wake on its alarms.",
    unchanged: "No agent gets any other new access.",
    options: [
      {
        label: "Let PM watch the engine's health",
        change: { entity: "roles", id: "pm", set: { health_tools: true }, summary: "Let pm watch" },
      },
      {
        label: "Let SRE watch the engine's health",
        change: {
          entity: "roles",
          id: "sre",
          set: { health_tools: true },
          summary: "Let sre watch the engine's health",
        },
      },
    ],
  },
};

const ENGINE: AdvisorFinding = {
  ...HEALTH,
  class: "wake.noop_share",
  actor: "engine",
  source: "evidence",
  title: "Many turns changed nothing.",
  fix: { kind: "none", sentence: "The engine is reducing this.", options: [] },
};

const SRE = { name: "SRE", handle: "sre", llm: "default" };
const VALID = { kind: "valid", baseRevisionId: "r1", warnings: [], derived: null };

async function mount(answer: AdvisorAnswer) {
  location.hash = "#/settings/advisor";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? ADMIN_VIEWER : answer);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Advisor />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  await settled();
}

/** Opens the health finding's preview and picks the SRE option. */
async function reviewSre() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Review the change" }));
    await settled();
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: /Let SRE watch/ }));
    await settled();
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  access.mockReturnValue({ can: true });
  getEntity.mockResolvedValue({ kind: "entity", entity: SRE, etag: '"roles-abc"' });
  dryRunEntity.mockResolvedValue(VALID);
  putEntity.mockResolvedValue({
    kind: "saved",
    revisionId: "r2",
    epoch: 3,
    warnings: [],
    derived: null,
  });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

test("the owner's findings are listed, and the engine's never as the owner's to-do", async () => {
  await mount({ revision: "r1", findings: [HEALTH, ENGINE], unread: [] });
  expect(screen.getByText(HEALTH.title)).toBeTruthy();
  expect(screen.getByText("What to change")).toBeTruthy();
  expect(screen.getByText("What the engine is handling")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Review the change" })).toHaveLength(1);
});

test("a company with nothing to change says so", async () => {
  await mount({ revision: "r1", findings: [], unread: [] });
  expect(screen.getByText("Nothing to change")).toBeTruthy();
});

test("the preview is the engine's dry run of the entity the change writes", async () => {
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect(getEntity).toHaveBeenCalledWith("roles", "sre", expect.anything());
  const sent = dryRunEntity.mock.calls.map((call) => call[2] as Record<string, unknown>);
  expect(sent).toContainEqual(SRE);
  expect(sent).toContainEqual({ ...SRE, health_tools: true });
  expect(dryRunEntity.mock.calls.every((call) => call[3] === '"roles-abc"')).toBe(true);
  expect(screen.getByRole("group", { name: "The change" }).textContent).toMatch(
    /health_tools: not set[\s\S]*health_tools: on/,
  );
  expect(putEntity).not.toHaveBeenCalled();
});

test("only the warnings the change adds are shown", async () => {
  const old = { kind: "advisory", path: "roles", message: "Already there." };
  const added = { kind: "advisory", path: "roles[1]", message: "The change adds this." };
  dryRunEntity
    .mockResolvedValueOnce({ ...VALID, warnings: [old] })
    .mockResolvedValueOnce({ ...VALID, warnings: [old, added] });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect(screen.getByText("The change adds this.")).toBeTruthy();
  expect(screen.queryByText("Already there.")).toBeNull();
});

test("apply writes that entity under its own tag, with the change's summary", async () => {
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settled();
  });
  expect(putEntity).toHaveBeenCalledOnce();
  const [kind, id, body, etag, summary] = putEntity.mock.calls[0] ?? [];
  expect([kind, id, etag, summary]).toEqual([
    "roles",
    "sre",
    '"roles-abc"',
    "Let sre watch the engine's health",
  ]);
  expect(body).toEqual({ ...SRE, health_tools: true });
});

test("a refused write is said, and the dialog stays open", async () => {
  putEntity.mockResolvedValue({ kind: "conflict", reason: "stale", currentRevisionId: "r9" });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settled();
  });
  expect(screen.getByText("Couldn't save that")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
});

test("a reader who may not write cannot apply", async () => {
  access.mockReturnValue({
    can: false,
    block: "grant",
    reason: "Only an administrator can change this.",
  });
  await mount({ revision: "r1", findings: [HEALTH], unread: [] });
  await reviewSre();
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText("Only an administrator can change this.")).toBeTruthy();
});

const BACKUP: AdvisorFinding = {
  ...HEALTH,
  class: "durability.no_verified_backup",
  severity: "blocks",
  source: "evidence",
  area: "durability",
  title: "Your company has no recent verified backup.",
  path: "",
  fix: {
    kind: "external_step",
    sentence: "Take a backup now, then schedule one to a place off this server.",
    options: [],
  },
};

test("a step outside the engine says what to do on the row, and offers no change", async () => {
  await mount({ revision: "r1", findings: [BACKUP], unread: [] });
  expect(screen.getByText(BACKUP.fix.sentence)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull();
});

test("two findings of one class about two seats sharing a name are two rows", async () => {
  const pm = { ...HEALTH, subject: { kind: "seat", seat: "pm", name: "Engineer" } };
  const qa = { ...HEALTH, subject: { kind: "seat", seat: "qa", name: "Engineer" } };
  await mount({ revision: "r1", findings: [pm, qa], unread: [] });
  expect(screen.getAllByText(HEALTH.title)).toHaveLength(2);
});

const APPROVAL: AdvisorFinding = {
  class: "integration.approval_required",
  severity: "blocks",
  actor: "owner",
  source: "reconcile",
  area: "integrations",
  subject: { kind: "integration", name: "GitHub" },
  title: "GitHub is waiting for you to approve the app's permissions.",
  why: "Until you do, the agents' GitHub tools stay off.",
  path: "integrations.github",
  evidence: [],
  fix: {
    kind: "external_step",
    sentence: "Accept the new permissions on GitHub.",
    options: [],
    setting: "github",
    url: "https://github.com/settings/installations/1",
  },
};

test("a step outside the engine says what to do and links where to do it", async () => {
  await mount({ revision: "r1", findings: [APPROVAL], unread: [] });
  expect(screen.getByText("Accept the new permissions on GitHub.")).toBeTruthy();
  const link = screen.getByRole("link", { name: /Open GitHub/ });
  expect(link.getAttribute("href")).toBe("https://github.com/settings/installations/1");
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull();
});

test("integrations the server couldn't read are said, so a short list is not all clear", async () => {
  await mount({ revision: "r1", findings: [], unread: ["integrations"] });
  expect(screen.getByText("Some of the setup couldn't be read")).toBeTruthy();
  expect(screen.getByText(/integrations' last check couldn't be read/)).toBeTruthy();
});

// EVERY UNREAD SOURCE IS SAID IN ITS OWN WORDS, and while any is unread an empty
// owner list never reads "Nothing to change": the source may hold the blocker.
test.each([
  ["work", /last 7 days of work couldn't be read/],
  ["ledger", /agents' turns couldn't be read/],
  ["alarms", /whether a backup is missing can't be said yet/],
  ["future_source", /couldn't read future_source/],
])("an unread %s source is said, and nothing reads as all clear", async (key, words) => {
  await mount({ revision: "r1", findings: [ENGINE], unread: [key] });
  expect(screen.getByText("Some of the setup couldn't be read")).toBeTruthy();
  expect(screen.getByText(words)).toBeTruthy();
  expect(screen.queryByText("Nothing to change")).toBeNull();
});

/** The go-live check's steps as drawn, label and state. */
function goLive() {
  const list = screen.getByRole("list", { name: "Go-live check" });
  return [...list.querySelectorAll("li")].map((li) => li.textContent ?? "");
}

test("the go-live check counts what is left by what it costs", async () => {
  const blocker = { ...HEALTH, severity: "blocks" as const, path: "integrations.datadog" };
  await mount({ revision: "r1", findings: [blocker, HEALTH, ENGINE], unread: [] });
  const steps = goLive();
  expect(steps[0]).toMatch(/Fix what blocks the company \(1\)/);
  expect(steps[0]).toMatch(/To do/);
  expect(steps[1]).toMatch(/Look at what costs it \(1\)/);
  expect(steps[2]).toMatch(/Nothing else to read/);
  expect(steps[2]).toMatch(/Done/);
});

test("a source the answer could not read leaves its steps unsaid, never done", async () => {
  await mount({ revision: "r1", findings: [], unread: ["integrations"] });
  const steps = goLive();
  expect(steps[0]).toMatch(/Nothing that could be read blocks the company/);
  expect(steps[0]).toMatch(/Can't say yet/);
  expect(steps[0]).not.toMatch(/Done/);
});
