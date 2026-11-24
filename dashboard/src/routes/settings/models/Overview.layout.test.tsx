/**
 * THE MODELS PAGE, LAID OUT: who-runs rows a hairline apart, each picker as
 * wide as its value and standing in the one model column, and the empty state
 * held inside its card on a phone.
 */
import "~/styles/models.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { EVERY_JOB, MODELS_NOW, MODELS_ORG, modelEntry, modelsData, runs } from "./fixtures.ts";
import type { ModelsData } from "./data.ts";
import { Overview } from "./Overview.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
  pinZoneForTest("UTC");
  vi.stubGlobal("WebSocket", InertWebSocket);
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  pinZoneForTest("");
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const TWO = modelsData({
  entries: [
    modelEntry({ key: "default", used_by: runs("backend-engineer", ...EVERY_JOB) }),
    modelEntry({ key: "spare" }),
  ],
});

/** The Models page `width` wide, settled. */
async function mount(width: number, data: ModelsData) {
  await page.viewport(Math.max(1440, width + 40), 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={MODELS_ORG as never}>
        <Overview data={data} onFlow={vi.fn()} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  for (let i = 0; i < 10; i++) await new Promise((r) => requestAnimationFrame(r));
}

const rows = () => [...host!.querySelectorAll<HTMLElement>(".crewlet-assignment-list__row")];

test("every who-runs row after a group's first stands under a hairline", async () => {
  await mount(1000, TWO);
  for (const list of host!.querySelectorAll<HTMLElement>(".crewlet-assignment-list__rows")) {
    const [first, ...rest] = [
      ...list.querySelectorAll<HTMLElement>(":scope > .crewlet-assignment-list__row"),
    ];
    expect(getComputedStyle(first!).borderTopWidth).toBe("0px");
    expect(rest.length).toBeGreaterThan(0);
    for (const row of rest) expect(getComputedStyle(row).borderTopWidth).toBe("1px");
  }
});

test("each picker starts at one edge, right after the names, never at the row's far half", async () => {
  await mount(1000, TWO);
  const starts = new Set<number>();
  for (const row of rows()) {
    const column = row.querySelector<HTMLElement>(":scope > .crewlet-assignment-list__value")!;
    const picker = column.querySelector<HTMLElement>('[role="combobox"]')!;
    const r = row.getBoundingClientRect();
    const p = picker.getBoundingClientRect();
    starts.add(Math.round(p.left));
    // THE VALUE BY ITS NAME: no wide band between a name and its model.
    expect(p.left - r.left).toBeLessThan(r.width * 0.45);
  }
  // Every row's picker, helpers and seats alike, begins at the same edge.
  expect(Math.max(...starts) - Math.min(...starts)).toBeLessThanOrEqual(1);
});

test("on a phone the empty state holds its title, line and add inside its card", async () => {
  await mount(358, modelsData({ entries: [] }));
  const card = host!.querySelector<HTMLElement>(".models-page > .crewlet-card")!;
  expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth);
  const c = card.getBoundingClientRect();
  for (const el of card.querySelectorAll(".crewlet-empty-state *")) {
    const r = el.getBoundingClientRect();
    expect(r.left).toBeGreaterThanOrEqual(c.left);
    expect(r.right).toBeLessThanOrEqual(c.right);
  }
});
