/**
 * WHO RUNS, LAID OUT: every consumer's face, a helper's product mark and a
 * seat's avatar, is one 20px size inside its icon box, and a seat's face sits
 * in a row as tall as a helper's, in light and dark.
 */
import "~/styles/models.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { SEAT_JOBS } from "~/lib/modelUses.ts";
import type { FleetAssignment } from "~/protocol/fleets.ts";
import {
  ASSISTANT_CHAT,
  DEFAULT_FLEET_EMPTY,
  MODELS_NOW,
  MODELS_ORG,
  fleetsFixture,
  modelEntry,
  modelsData,
  runs,
} from "../fixtures.ts";
import { WhoRunsTable } from "./WhoRuns.tsx";

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
  document.documentElement.removeAttribute("data-theme");
  pinZoneForTest("");
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const ENTRIES = [
  modelEntry({
    key: "default",
    used_by: [ASSISTANT_CHAT, ...runs("backend-engineer", "executor")],
  }),
];
const ASSIGNMENTS: FleetAssignment[] = [
  {
    subject: "assistant",
    kind: "helper",
    name: "Ask Crewlet",
    target: { pinned: "default" },
    now: "default",
    waiting_until: null,
  },
  {
    subject: "advisor",
    kind: "helper",
    name: "Advisor",
    target: { fleet: "default-fleet" },
    now: null,
    waiting_until: null,
  },
  {
    subject: "backend-engineer",
    kind: "seat",
    name: "Backend Engineer",
    target: { pinned: "" },
    now: null,
    waiting_until: null,
    jobs: SEAT_JOBS.map((job) => ({
      job,
      target: { pinned: "default" },
      now: "default",
      waiting_until: null,
    })),
  },
  {
    subject: "pm",
    kind: "seat",
    name: "Product Manager",
    target: { fleet: "default-fleet" },
    now: null,
    waiting_until: null,
  },
];

/** Who runs at 1440, in theme, with its first expandable seat opened. */
async function mount(theme: "light" | "dark") {
  await page.viewport(1440, 900);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  host.style.cssText = "width: 1200px;";
  document.body.append(host);
  root = createRoot(host);
  const fleets = fleetsFixture({
    entries: ENTRIES,
    fleets: [DEFAULT_FLEET_EMPTY],
    assignments: ASSIGNMENTS,
  });
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={MODELS_ORG as never}>
        <WhoRunsTable data={modelsData({ entries: ENTRIES })} fleets={fleets} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  const toggle = host.querySelector<HTMLElement>('[aria-expanded="false"]');
  expect(toggle, "a seat with jobs opens").not.toBeNull();
  toggle!.click();
  for (let i = 0; i < 10; i++) await new Promise((r) => requestAnimationFrame(r));
}

/** Each consumer row's face box and row height, by its name. */
function measure() {
  return [...host!.querySelectorAll<HTMLElement>('[role="row"]')].flatMap((row) => {
    const icon = row.querySelector<HTMLElement>(".crewlet-org-label__icon");
    const face = icon?.firstElementChild;
    const name = row.querySelector(".crewlet-org-label__name")?.textContent ?? "";
    if (
      !icon ||
      !face ||
      !["Ask Crewlet", "Advisor", "Backend Engineer", "Product Manager"].includes(name)
    )
      return [];
    const f = face.getBoundingClientRect();
    const box = icon.getBoundingClientRect();
    return [
      { name, face: [f.width, f.height], box: box.width, row: row.getBoundingClientRect().height },
    ];
  });
}

for (const theme of ["light", "dark"] as const) {
  test(`every consumer's face is one 20px size and every consumer row one height (${theme})`, async () => {
    await mount(theme);
    const seen = measure();
    expect(seen.map((s) => s.name).sort()).toEqual([
      "Advisor",
      "Ask Crewlet",
      "Backend Engineer",
      "Product Manager",
    ]);
    for (const s of seen) {
      expect(s.face, s.name).toEqual([20, 20]);
      expect(s.face[0], `${s.name} fits its icon box`).toBeLessThanOrEqual(s.box);
    }
    expect(new Set(seen.map((s) => s.row)).size, "one row height").toBe(1);
  });
}
