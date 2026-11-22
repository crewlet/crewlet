/**
 * A learned skill's summary and a session's address stand straight under the
 * skill and the device, as every two-line table cell draws them: cut to the
 * column on a desktop, whole on a phone's card rows.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { DataGrid } from "@crewlethq/ui";
import "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { learnedSkillColumns } from "./memory.tsx";
import { SessionDevice } from "./SessionDevice.tsx";
import { deviceOf } from "~/lib/userAgent.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

const SUMMARY =
  "Reconcile the nightly backup report against the retention policy, name every bucket whose copies drifted past it, and file one task per drift for the owner";
const AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) Gecko/20100101 Firefox/131.0";
const ADDRESS =
  "2001:db8:85a3::8a2e:370:7334 through 198.51.100.24, 203.0.113.7 and 192.0.2.146, forwarded by the edge proxy";

const skills = (
  <DataGrid
    aria-label="Skills"
    phoneRows="compact"
    rows={[
      {
        id: "s1",
        key: "backup-audit",
        title: "Audit the backups",
        summary: SUMMARY,
        version: 2,
        updated_at: "2026-11-20T10:00:00Z",
        uses: 12,
        loads: 5,
      },
    ]}
    rowKey={(r) => r.id}
    columns={learnedSkillColumns(Date.parse("2026-11-22T10:00:00Z"))}
  />
);

const sessions = (
  <DataGrid
    aria-label="Sessions"
    rows={[{ id: "a", user_agent: AGENT, remote: ADDRESS }]}
    rowKey={(r) => r.id}
    columns={[
      { key: "device", header: "Device", cell: (s) => <SessionDevice row={s} /> },
      { key: "started", header: "Started", align: "right", shrink: true, cell: () => "2h ago" },
    ]}
  />
);

async function mount(ui: ReactNode, viewport: number, width: number) {
  await page.viewport(viewport, 900);
  host = document.createElement("div");
  host.style.width = `${width}px`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() => root!.render(ui));
  await document.fonts.ready;
  await eventually(() =>
    expect(host!.querySelector(".crewlet-data-grid[data-stacked]") !== null).toBe(viewport <= 390),
  );
}

/** The innermost element drawing exactly this text. */
function drawing(text: string): HTMLElement {
  const all = [...host!.querySelectorAll<HTMLElement>(".crewlet-data-grid__row *")];
  return all.filter((el) => el.textContent === text).at(-1)!;
}

const SHAPES: [string, ReactNode, string, string][] = [
  ["a learned skill's summary", skills, "Audit the backups", SUMMARY],
  ["a session's address", sessions, deviceOf(AGENT), ADDRESS],
];

test.each(
  SHAPES.flatMap(([what, ui, value, detail]) => [
    [`${what}, on a desktop`, ui, value, detail, 1440, 700],
    [`${what}, on a phone`, ui, value, detail, 390, 358],
  ]) as [string, ReactNode, string, string, number, number][],
)("%s, stands straight under its value", async (_, ui, value, detail, viewport, width) => {
  await mount(ui, viewport, width);
  const under = drawing(detail);
  // The value's line box: a value wrapped in a span of its own draws only its glyphs.
  const line = drawing(value).closest(".crewlet-table-cell__lines > *") ?? drawing(value);
  const gap = under.getBoundingClientRect().top - line.getBoundingClientRect().bottom;
  expect(gap).toBeGreaterThanOrEqual(-0.5);
  expect(gap).toBeLessThanOrEqual(1);
  // Cut to its column on a desktop; whole on a phone, where no hover shows the rest.
  expect(under.scrollWidth > under.clientWidth).toBe(viewport > 390);
});
