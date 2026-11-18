/**
 * THE USAGE LIMITS CELL, LAID OUT: one line per window, its figures at the
 * column's trailing edge, at a 1440px window's list width and a wider one, and
 * on hover a tooltip with one line per window and its reset.
 * Mutation: give the cell's windows a `note`, or drop the tooltip's `lines`.
 */
import "~/styles/models.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { pinZoneForTest } from "~/lib/prefs.ts";
import type { ModelLimit } from "~/protocol/models.ts";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { MODELS_NOW, MODELS_ORG, at, modelEntry, modelsData } from "./fixtures.ts";
import { Overview } from "./Overview.tsx";

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

const FIVE_HOUR = limit({ window: "session", hours: 5, used: 0.32, resets_at: at(106) });
const WEEKLY = limit({ window: "weekly", hours: 168, used: 0.46, resets_at: at(60 * 48) });
const OPUS = limit({ window: "model", hours: 168, model: "opus", state: "reached", until: at(90) });

const ENTRIES = [
  modelEntry({ key: "none", limits: [] }),
  modelEntry({ key: "one", limits: [FIVE_HOUR] }),
  modelEntry({ key: "two", limits: [FIVE_HOUR, WEEKLY] }),
  modelEntry({ key: "three", limits: [FIVE_HOUR, WEEKLY, OPUS] }),
];

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
  pinZoneForTest("UTC");
  vi.stubGlobal("WebSocket", InertWebSocket);
});

afterEach(async () => {
  await userEvent.unhover(document.body);
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  pinZoneForTest("");
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** The Models list `width` wide, settled. */
async function mount(width: number) {
  await page.viewport(Math.max(1440, width + 40), 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={MODELS_ORG as never}>
        <Overview data={modelsData({ entries: ENTRIES })} onFlow={vi.fn()} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  for (let i = 0; i < 10; i++) await new Promise((r) => requestAnimationFrame(r));
}

function cell(key: string): HTMLElement {
  const row = [...host!.querySelectorAll<HTMLElement>('[role="row"]')].find(
    (r) => r.querySelector(".models-entry-cell .mono")?.textContent === key,
  )!;
  return row.querySelector<HTMLElement>(".crewlet-limit-meters")!;
}

const box = (el: Element) => el.getBoundingClientRect();

// 910px is the list's width in a 1440px window, its column at its floor; 1400px gives it more.
for (const width of [910, 1400]) {
  test(`at a ${width}px list each window is one line, its figure at the cell's edge`, async () => {
    await mount(width);
    for (const [key, n] of [
      ["one", 1],
      ["two", 2],
      ["three", 3],
    ] as const) {
      const meters = cell(key);
      const labels = [...meters.querySelectorAll(".crewlet-limit-meters__label")];
      const figures = [...meters.querySelectorAll(".crewlet-limit-meters__figure")];
      expect(labels).toHaveLength(n);
      const line = box(labels[0]!).height;
      // A window's second line would put more than a line and a gap between labels.
      labels.slice(1).forEach((label, i) => {
        expect(box(label).top - box(labels[i]!).top).toBeLessThan(line * 1.5);
      });
      expect(box(meters).height).toBeLessThan(n * line * 1.5);
      for (const figure of figures) {
        expect(Math.abs(box(meters).right - box(figure).right)).toBeLessThan(1);
      }
    }
    expect(cell("none").textContent).toBe("None reported");
  });
}

async function openTip(): Promise<HTMLElement> {
  for (let i = 0; i < 80; i++) {
    const tip = document.querySelector<HTMLElement>('[role="tooltip"]');
    if (tip && getComputedStyle(tip).visibility !== "hidden") return tip;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error("the tooltip never opened");
}

test("hovering any bar opens the tooltip: each window on one line, with its use and reset", async () => {
  await mount(910);
  const bars = [...cell("three").querySelectorAll<HTMLElement>(".crewlet-limit-meters__track")];
  expect(bars).toHaveLength(3);
  for (const bar of bars) {
    await userEvent.hover(bar);
    const tip = await openTip();
    const lines = [...tip.children] as HTMLElement[];
    expect(lines.map((l) => l.textContent)).toEqual([
      "5-hour: 32% used, resets 13:46 UTC",
      "Weekly: 46% used, resets Sat, Oct 31, 12:00 UTC",
      "Weekly Opus: reached, sitting out until 13:30 UTC",
      "Heard 1 minute ago",
    ]);
    const lineHeight = Number.parseFloat(getComputedStyle(lines[0]!).lineHeight);
    for (const line of lines)
      expect(box(line).height, line.textContent!).toBeLessThan(lineHeight * 1.5);
    await userEvent.unhover(bar);
    await userEvent.keyboard("{Escape}");
  }
});
