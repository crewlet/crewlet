/**
 * A model's usage limits: each window named and dated in words, the exact
 * figures in its name, and a model whose harness reports none says so.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { TestClient } from "~/test/client.tsx";
import type { ModelLimit } from "~/protocol/models.ts";
import { MODELS_NOW, at, memberEntry, modelEntry, modelsData } from "./fixtures.ts";
import { Overview } from "./Overview.tsx";
import { limitLabel, limitWindow } from "./UsageLimits.tsx";

function limit(over: Partial<ModelLimit> & Pick<ModelLimit, "window">): ModelLimit {
  return {
    hours: 0,
    state: "ok",
    used: null,
    resets_at: null,
    until: null,
    seen: at(-1),
    stale: false,
    ...over,
  };
}

const FIVE_HOUR = limit({
  window: "session",
  hours: 5,
  used: 0.83,
  state: "warning",
  resets_at: at(300),
});
const WEEKLY = limit({ window: "weekly", hours: 168, used: 0.41, resets_at: at(60 * 24 * 3) });
const OPUS = limit({ window: "model", hours: 168, model: "opus", state: "reached", until: at(90) });

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
  pinZoneForTest("UTC");
});

afterEach(() => {
  cleanup();
  pinZoneForTest("");
  vi.useRealTimers();
});

test("a window is named by its length and its model, or by its kind where the vendor gives none", () => {
  expect(limitLabel(FIVE_HOUR)).toBe("5-hour");
  expect(limitLabel(WEEKLY)).toBe("Weekly");
  expect(limitLabel(OPUS)).toBe("Weekly Opus");
  expect(limitLabel(limit({ window: "model", model: "sonnet" }))).toBe("Sonnet");
  expect(limitLabel(limit({ window: "session", hours: 24 }))).toBe("Daily");
  expect(limitLabel(limit({ window: "session", hours: 3 }))).toBe("3-hour");
  expect(limitLabel(limit({ window: "session" }))).toBe("Session");
  expect(limitLabel(limit({ window: "unknown" }))).toBe("Usage");
});

test("each bar's name carries its state, its exact figure and the absolute reset, in the reader's zone", () => {
  const w = limitWindow(FIVE_HOUR, MODELS_NOW);
  expect(w).toMatchObject({ label: "5-hour", state: "near", used: 0.83, note: "resets 17:00" });
  expect(w.name).toBe("5-hour limit, near its limit, 83% used, resets 17:00 UTC");
  const weekly = limitWindow(WEEKLY, MODELS_NOW);
  expect(weekly.note).toBe("resets Sun 12:00");
  expect(weekly.name).toMatch(/^Weekly limit, OK, 41% used, resets Sun, Nov 1, 12:00 UTC$/);
});

test("a window known only by its state says the state, never a percentage", () => {
  const w = limitWindow(OPUS, MODELS_NOW);
  expect(w).toMatchObject({
    state: "refusing",
    used: undefined,
    figure: "Reached",
    note: "until 13:30",
  });
  expect(w.name).toBe("Weekly Opus limit, reached, sitting out until 13:30 UTC");
});

test("a window that turned over since it was heard says so, and a stale one says how old it is", () => {
  const over = limitWindow(
    limit({ window: "weekly", hours: 168, resets_at: at(-30), seen: at(-120), stale: true }),
    MODELS_NOW,
  );
  expect(over).toMatchObject({ figure: "OK", note: "reset at 11:30" });
  expect(over.name).toBe(
    "Weekly limit, OK, turned over since it was last heard, reset at 11:30 UTC, as of 2h ago",
  );
});

function mount(admin = true) {
  const entries = [
    modelEntry({ key: "opus-main", limits: [FIVE_HOUR, WEEKLY, OPUS] }),
    modelEntry({ key: "stale", limits: [{ ...WEEKLY, seen: at(-120), stale: true }] }),
    modelEntry({ key: "local", limits: [] }),
  ];
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <Overview data={modelsData({ admin, entries })} onFlow={vi.fn()} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
}

const row = (key: string) =>
  screen
    .getAllByRole("row")
    .find((r) => r.querySelector(".models-entry-cell .mono")?.textContent === key)!;

test("the list's Usage limits column draws a bar per window, says when one is stale, and None reported", () => {
  mount();
  expect(screen.getByRole("columnheader", { name: /Usage limits/ })).toBeTruthy();
  const group = within(row("opus-main")).getByRole("group", { name: "Usage limits of opus-main" });
  expect(
    within(group)
      .getAllByRole("meter")
      .map((m) => m.getAttribute("aria-label")),
  ).toEqual([
    "5-hour limit, near its limit, 83% used, resets 17:00 UTC",
    "Weekly limit, OK, 41% used, resets Sun, Nov 1, 12:00 UTC",
  ]);
  expect(within(group).getByRole("img").getAttribute("aria-label")).toBe(
    "Weekly Opus limit, reached, sitting out until 13:30 UTC",
  );
  expect(row("stale").textContent).toContain("as of 2h ago");
  expect(row("local").textContent).toContain("None reported");
  expect(within(row("local")).queryByRole("meter")).toBeNull();
});

test("a member reads the same windows", () => {
  const entry = modelEntry({ key: "opus-main", limits: [FIVE_HOUR] });
  expect(memberEntry(entry).limits).toEqual([FIVE_HOUR]);
  mount(false);
  expect(within(row("opus-main")).getAllByRole("meter")).toHaveLength(2);
});
