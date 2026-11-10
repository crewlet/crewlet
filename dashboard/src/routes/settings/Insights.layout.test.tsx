/**
 * USAGE INSIGHTS, LAID OUT: every answer to a record waiting for review is a
 * control a person can reach, on a wide screen and on a phone.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Insights } from "./Insights.tsx";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin"],
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

const RECORD = {
  schema: 1,
  class: "wake.noop_share",
  week: "2030-W02",
  version: "v1.4.0",
  facts: { seats: "1", share: "35-49" },
};

const ANSWERS: Record<string, unknown> = {
  viewer: VIEWER,
  // THE FRAME'S OWN READS, empty: this case is about one card.
  inbox: { handle: "jane", notices: [], sources: [], unread: 0, primary: 0, complete: true },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  answered_decisions: { handle: "jane", items: [] },
  diagnostics_insights: {
    share: "review",
    accepted: 1,
    schema: 1,
    always: [],
    relay: "https://reports.crewlet.ai/v1/insights",
    catalogue: {
      schema: 1,
      classes: [
        {
          class: "wake.noop_share",
          title: "Agents were woken for turns that changed nothing",
          summary: "The share of turns that changed nothing.",
          facts: [{ key: "share", values: ["25-34", "35-49", "50+"] }],
        },
      ],
    },
    records: [{ class: "wake.noop_share", week: "2030-W02", status: "queued", record: RECORD }],
  },
};

/** The four answers a queued record offers, by their visible words. */
const ACTIONS = ["View", "Send", "Don’t send", "Always send this pattern"];

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  location.hash = "#/settings/diagnostics";
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

async function mount(width: number) {
  await page.viewport(width, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={ORG}>
        <Insights />
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(queue()).not.toBeNull());
  await document.fonts.ready;
}

const queue = () =>
  document.querySelector<HTMLElement>('[aria-label="Usage insights waiting for your review"]');

/** The answers a person cannot press: cut off by the grid, outside it, or covered. */
function unreachable(): string[] {
  const grid = queue()!;
  const out: string[] = [];
  for (const words of ACTIONS) {
    const button = [...grid.querySelectorAll("button")].find((b) => b.textContent === words);
    if (!button) {
      out.push(`${words}: not drawn`);
      continue;
    }
    button.scrollIntoView({ block: "center", inline: "center" });
    const r = button.getBoundingClientRect();
    const box = grid.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    if (r.width === 0 || r.left < box.left - 0.5 || r.right > box.right + 0.5) {
      out.push(`${words}: outside the grid`);
    } else if (!hit || !button.contains(hit)) {
      out.push(`${words}: covered or clipped`);
    } else if (button.scrollWidth > button.clientWidth + 1) {
      out.push(`${words}: its label is cut off`);
    }
  }
  return out;
}

test.each([1440, 390])("every answer to a queued record is reachable at %ipx", async (width) => {
  await mount(width);
  expect(unreachable()).toEqual([]);
});
