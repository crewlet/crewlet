/**
 * THE MODELS PAGE, LAID OUT: who-runs rows a hairline apart, each picker as
 * wide as its value and standing in the one model column, and the first-run
 * card's picture held inside its card on a phone.
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

const rows = () => [...host!.querySelectorAll<HTMLElement>(".models-run")];

test("every who-runs row after a group's first stands under a hairline", async () => {
  await mount(1000, TWO);
  for (const list of host!.querySelectorAll<HTMLElement>(".models-runs__list")) {
    const [first, ...rest] = [...list.querySelectorAll<HTMLElement>(":scope > .models-run")];
    expect(getComputedStyle(first!).borderTopWidth).toBe("0px");
    expect(rest.length).toBeGreaterThan(0);
    for (const row of rest) expect(getComputedStyle(row).borderTopWidth).toBe("1px");
  }
});

test("each picker is as wide as its value and starts at the model column's edge", async () => {
  await mount(1000, TWO);
  const starts = new Set<number>();
  for (const row of rows()) {
    const column = row.querySelector<HTMLElement>(":scope > .models-run__model")!;
    const picker = column.querySelector<HTMLElement>('[role="combobox"]')!;
    const c = column.getBoundingClientRect();
    const p = picker.getBoundingClientRect();
    // As wide as what it says, so its chevron sits by the value, not at the column's far end.
    expect(p.width).toBeLessThan(c.width - 40);
    starts.add(Math.round(p.left - c.left));
  }
  // Every picker begins where its column does.
  expect([...starts].every((s) => Math.abs(s) <= 1)).toBe(true);
});

test("on a phone the first-run card holds its picture inside its edges, six marks to a row", async () => {
  await mount(358, modelsData({ entries: [] }));
  const card = host!.querySelector<HTMLElement>(".models-page > .crewlet-card")!;
  const marks = card.querySelector<HTMLElement>(".models-first-run__marks")!;
  expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth);
  const c = card.getBoundingClientRect();
  const m = marks.getBoundingClientRect();
  expect(m.left).toBeGreaterThanOrEqual(c.left);
  expect(m.right).toBeLessThanOrEqual(c.right);
  const tops = new Set([...marks.children].map((t) => Math.round(t.getBoundingClientRect().top)));
  expect(tops.size).toBe(Math.ceil(marks.children.length / 6));
});
