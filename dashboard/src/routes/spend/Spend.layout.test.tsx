/**
 * TOKEN SPEND, LAID OUT, over a window no agent spent in. A row is one height,
 * so an empty card is stretched to its neighbour's; its empty state then
 * stands in the middle of that height rather than at the top over a band of
 * nothing, which read as a card that failed to fill.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Spend } from "./Spend.tsx";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet"],
  admin: true,
  configWriter: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const NOTHING = { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 };
// A window that spent tokens draws its breakdown cards; one that spent none draws only the chart's empty state.
const SPENT = { input_tokens: 3, output_tokens: 2, total_tokens: 5, calls: 1 };
const WINDOW = {
  since: "2026-08-31T00:00:00Z",
  until: "2026-09-30T00:00:00Z",
  from: "2026-08-31",
  to: "2026-09-29",
  days: 30,
  horizon: { days: 181, floor: "2026-04-01" },
};

const ANSWERS: Record<string, unknown> = {
  viewer: VIEWER,
  inbox: { handle: "jane", notices: [], sources: [], unread: 0, primary: 0, complete: true },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  answered_decisions: { handle: "jane", items: [] },
  tokens: {
    ...WINDOW,
    totals: SPENT,
    by_phase: [],
    by_model: [],
    by_provider: [],
    by_worker: [],
    by_agent: [],
  },
  token_series: {
    ...WINDOW,
    group: "phase",
    bucket: "day",
    series: [],
    by_group: [],
    totals: NOTHING,
    grouped: NOTHING,
  },
  turn_outcomes: { from: "2026-08-31", to: "2026-09-29", rows: [] },
  work_items: { items: [], total_hint: 0, complete: true },
};

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  location.hash = "#/settings/spend?window=30d";
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

test("an empty card's state stands in the middle of the height its row gives it", async () => {
  await page.viewport(1440, 1200);
  host = document.createElement("div");
  // THE PAGE COLUMN the spend rows are measured from (`@container page`).
  host.style.cssText = "width: 960px; container: page / inline-size;";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={ORG}>
        <Spend />
      </LayoutClient>,
    ),
  );
  const card = () => document.querySelector<HTMLElement>(".spend-agents");
  await eventually(() => expect(card()?.querySelector(".crewlet-empty-state")).toBeTruthy());
  await document.fonts.ready;
  const agents = card()!;
  const side = document.querySelector<HTMLElement>(".spend-side")!;
  // STRETCHED, as every row is: the card stands as tall as the lists beside it.
  expect(
    Math.abs(agents.getBoundingClientRect().height - side.getBoundingClientRect().height),
  ).toBeLessThan(2);
  const head = agents.querySelector<HTMLElement>(".crewlet-card__header")!.getBoundingClientRect();
  const box = agents.getBoundingClientRect();
  const state = agents.querySelector<HTMLElement>(".crewlet-empty-state")!.getBoundingClientRect();
  // THE STATE'S MIDDLE IS THE MIDDLE OF WHAT THE HEAD AND THE FOOT LEAVE.
  const foot = agents.querySelector<HTMLElement>(".spend-card-foot")?.getBoundingClientRect();
  const room = (head.bottom + (foot?.top ?? box.bottom)) / 2;
  expect(Math.abs((state.top + state.bottom) / 2 - room)).toBeLessThan(4);
});
