/**
 * A model's usage limits: each window named, the exact figures in its name, the
 * list's cell one line per window and the model page's card dated in words,
 * and a model whose harness reports none says so.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { TestClient } from "~/test/client.tsx";
import type { ModelLimit } from "~/protocol/models.ts";
import { MODELS_NOW, at, memberEntry, modelEntry, modelsData } from "./fixtures.ts";
import { Overview } from "./Overview.tsx";
import { UsageLimitsCard, limitLabel, limitWindow } from "./UsageLimits.tsx";

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

// Mutation: put `note` back in limitWindow, and the list's cell draws a second line per window.
test("each bar's name carries its state, its exact figure and the absolute reset, in the reader's zone", () => {
  const w = limitWindow(FIVE_HOUR, MODELS_NOW);
  expect(w).toMatchObject({ label: "5-hour", state: "near", used: 0.83 });
  expect(w.note).toBeUndefined();
  expect(w.name).toBe("5-hour limit, near its limit, 83% used, resets 17:00 UTC");
  const weekly = limitWindow(WEEKLY, MODELS_NOW);
  expect(weekly.name).toMatch(/^Weekly limit, OK, 41% used, resets Sun, Nov 1, 12:00 UTC$/);
});

test("a window known only by its state says the state, never a percentage", () => {
  const w = limitWindow(OPUS, MODELS_NOW);
  expect(w).toMatchObject({ state: "refusing", used: undefined, figure: "Reached" });
  expect(w.name).toBe("Weekly Opus limit, reached, sitting out until 13:30 UTC");
});

const TURNED_OVER = limit({
  window: "weekly",
  hours: 168,
  resets_at: at(-30),
  seen: at(-120),
  stale: true,
});

test("a window that turned over since it was heard says so, and a stale one says how old it is", () => {
  const over = limitWindow(TURNED_OVER, MODELS_NOW);
  expect(over).toMatchObject({ figure: "OK" });
  expect(over.name).toBe(
    "Weekly limit, OK, turned over since it was last heard, reset at 11:30 UTC, as of 2 hours ago",
  );
});

// Mutation: drop the card's `note`, and the model page loses when each window turns over.
test("the model page's card writes when each window turns over under its bar", () => {
  const { models } = modelsData({
    entries: [
      modelEntry({ key: "opus-main", limits: [FIVE_HOUR, WEEKLY, OPUS] }),
      modelEntry({ key: "turned", limits: [TURNED_OVER] }),
    ],
  });
  render(
    <TestClient>
      {models.map((m) => (
        <UsageLimitsCard key={m.key} model={m} />
      ))}
    </TestClient>,
  );
  const notes = [...document.querySelectorAll(".crewlet-limit-meters__note")].map(
    (n) => n.textContent,
  );
  expect(notes).toEqual(["resets 17:00", "resets Sun 12:00", "until 13:30", "reset at 11:30"]);
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
  expect(
    within(screen.getByRole("table")).getByRole("columnheader", { name: /Usage limits/ }),
  ).toBeTruthy();
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
  // A stale window draws no caption in the list: its "as of" is in the link's description.
  expect(row("stale").querySelector(".crewlet-limit-meters__caption")).toBeNull();
  expect(row("stale").querySelector("a [hidden]")?.textContent).toContain("as of 2 hours ago");
  expect(row("local").textContent).toContain("None reported");
  expect(within(row("local")).queryByRole("meter")).toBeNull();
});

// One line per window: when it resets is on hover, on focus and in the link's description.
test("the list's cell draws no reset line, and says when each window resets on focus and to a screen reader", () => {
  mount();
  const cell = within(row("opus-main"));
  expect(cell.getByRole("group", { name: "Usage limits of opus-main" }).textContent).toBe(
    "5-hour83%Weekly41%Weekly OpusReached",
  );
  expect(row("opus-main").querySelector(".crewlet-limit-meters__note")).toBeNull();
  const link = cell.getByRole("link", { description: /resets 17:00 UTC/ });
  expect(link.getAttribute("href")).toBe("#/settings/models/opus-main");
  fireEvent.keyDown(document.body, { key: "Tab" });
  act(() => link.focus());
  expect(tipLines()).toEqual(OPUS_MAIN_LINES);
});

const OPUS_MAIN_LINES = [
  "5-hour: 83% used, resets 17:00 UTC",
  "Weekly: 41% used, resets Sun, Nov 1, 12:00 UTC",
  "Weekly Opus: reached, sitting out until 13:30 UTC",
  "Heard 1 minute ago",
];

/** The open tooltip's lines, one per child of its panel. */
const tipLines = () =>
  [...screen.getByRole("tooltip", { hidden: true }).children].map((line) => line.textContent);

// Mutation: drop the tooltip's `lines`, or a window's `when`, and a case fails.
test("a pointer resting on any bar opens the tooltip, one line per window with its use and reset", async () => {
  mount();
  const bars = [
    ...within(row("opus-main")).getAllByRole("meter"),
    within(row("opus-main")).getByRole("img", { name: /^Weekly Opus limit/ }),
  ];
  for (const bar of bars) {
    fireEvent.pointerOver(bar);
    const tip = await screen.findByRole("tooltip", { hidden: true });
    expect(tip.classList.contains("crewlet-tooltip--lines")).toBe(true);
    expect(tipLines()).toEqual(OPUS_MAIN_LINES);
    fireEvent.pointerOut(bar);
    fireEvent.keyDown(document.body, { key: "Escape" });
  }
});

test("windows heard at different times each say when, on their own line", async () => {
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <Overview
            data={modelsData({
              entries: [
                modelEntry({
                  key: "mixed",
                  limits: [FIVE_HOUR, { ...WEEKLY, seen: at(-120), stale: true }],
                }),
              ],
            })}
            onFlow={vi.fn()}
          />
        </Router>
      </LayerHost>
    </TestClient>,
  );
  fireEvent.pointerOver(within(row("mixed")).getAllByRole("meter")[0]!);
  await screen.findByRole("tooltip", { hidden: true });
  expect(tipLines()).toEqual([
    "5-hour: 83% used, resets 17:00 UTC, heard 1 minute ago",
    "Weekly: 41% used, resets Sun, Nov 1, 12:00 UTC, heard 2 hours ago",
  ]);
});

test("a member reads the same windows", () => {
  const entry = modelEntry({ key: "opus-main", limits: [FIVE_HOUR] });
  expect(memberEntry(entry).limits).toEqual([FIVE_HOUR]);
  mount(false);
  expect(within(row("opus-main")).getAllByRole("meter")).toHaveLength(2);
});
