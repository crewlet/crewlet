/**
 * A WIDGET THE READER SIZED NEVER SCROLLS: its card shows the rows that fit
 * whole, and its foot opens the page and counts the rest. Laid out in a real
 * browser, through the board's own frame and stylesheets.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { ArrangeFrame, ArrangeGrid, ListItem } from "@crewlethq/ui";
import { WidgetCard, WidgetList, WidgetSized } from "./kit.tsx";
import { ROW } from "./model.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

async function frames() {
  for (let i = 0; i < 4; i++) await new Promise((resolve) => requestAnimationFrame(resolve));
}

/** A board cell `rows` tall (null: its content's) holding a widget of `items` rows. */
async function mount(rows: number | null, items: number) {
  host = document.createElement("div");
  host.style.width = "1200px";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ArrangeGrid columns={12}>
        <ArrangeFrame name="Executors" editing={false} columns={4} rows={rows} rowHeight={ROW}>
          <WidgetSized sized={rows !== null}>
            <WidgetCard title="Executors" from="live-executors">
              <WidgetList rows={5}>
                {Array.from({ length: items }, (_, i) => (
                  <ListItem key={i} href="#/live/executors" trailing="2m">
                    {i % 4 === 1
                      ? `Run ${i + 1}, whose title is long enough to wrap`
                      : `Run ${i + 1}`}
                  </ListItem>
                ))}
              </WidgetList>
            </WidgetCard>
          </WidgetSized>
        </ArrangeFrame>
      </ArrangeGrid>,
    ),
  );
  await document.fonts.ready;
  await frames();
  const frame = host.querySelector<HTMLElement>(".crewlet-arrange-frame")!;
  const card = host.querySelector<HTMLElement>(".home-widget")!;
  const list = host.querySelector<HTMLElement>(".crewlet-list")!;
  const all = Array.from(list.children) as HTMLElement[];
  const shown = all.filter((r) => getComputedStyle(r).visibility !== "hidden");
  const foot = host.querySelector<HTMLElement>(".home-widget-foot-link")!;
  return { frame, card, list, all, shown, foot };
}

test("a sized widget shows only whole rows, never scrolls, and counts the rest in its foot", async () => {
  const { frame, card, list, all, shown, foot } = await mount(18, 30);
  expect(getComputedStyle(card).overflowY).toBe("clip");
  expect(shown.length).toBeGreaterThan(1);
  expect(shown).toEqual(all.slice(0, shown.length));
  const edge = list.getBoundingClientRect().bottom + 0.5;
  for (const row of shown) expect(row.getBoundingClientRect().bottom).toBeLessThanOrEqual(edge);
  expect(foot.textContent).toBe(`Open Executors+${all.length - shown.length} more`);
  expect(foot.getBoundingClientRect().bottom).toBeLessThanOrEqual(
    frame.getBoundingClientRect().bottom + 0.5,
  );
});

test("a taller cell shows more rows, and a cell of its content's height draws its first five", async () => {
  const short = (await mount(14, 30)).shown.length;
  root!.unmount();
  host!.remove();
  const tall = (await mount(36, 30)).shown.length;
  expect(tall).toBeGreaterThan(short);
  root!.unmount();
  host!.remove();
  const content = await mount(null, 30);
  expect(content.all).toHaveLength(5);
  expect(content.shown).toHaveLength(5);
  expect(content.foot.textContent).toBe("Open Executors+25 more");
});
