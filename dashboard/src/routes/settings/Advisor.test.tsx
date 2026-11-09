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
  await mount({ revision: "r1", findings: [HEALTH, ENGINE] });
  expect(screen.getByText(HEALTH.title)).toBeTruthy();
  expect(screen.getByText("What to change")).toBeTruthy();
  expect(screen.getByText("What the engine is handling")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Review the change" })).toHaveLength(1);
});

test("a company with nothing to change says so", async () => {
  await mount({ revision: "r1", findings: [] });
  expect(screen.getByText("Nothing to change")).toBeTruthy();
});

test("the preview is the engine's dry run of the entity the change writes", async () => {
  await mount({ revision: "r1", findings: [HEALTH] });
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
  await mount({ revision: "r1", findings: [HEALTH] });
  await reviewSre();
  expect(screen.getByText("The change adds this.")).toBeTruthy();
  expect(screen.queryByText("Already there.")).toBeNull();
});

test("apply writes that entity under its own tag, with the change's summary", async () => {
  await mount({ revision: "r1", findings: [HEALTH] });
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
  await mount({ revision: "r1", findings: [HEALTH] });
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
  await mount({ revision: "r1", findings: [HEALTH] });
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
  await mount({ revision: "r1", findings: [BACKUP] });
  expect(screen.getByText(BACKUP.fix.sentence)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull();
});

test("two findings of one class about two seats sharing a name are two rows", async () => {
  const pm = { ...HEALTH, subject: { kind: "seat", seat: "pm", name: "Engineer" } };
  const qa = { ...HEALTH, subject: { kind: "seat", seat: "qa", name: "Engineer" } };
  await mount({ revision: "r1", findings: [pm, qa] });
  expect(screen.getAllByText(HEALTH.title)).toHaveLength(2);
});
