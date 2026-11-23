/**
 * A DECISION'S WORDS KEEP THE ROW, whatever its answers say: beside long
 * answers the words once wrapped every few words in a sliver of the row. The
 * answers sit beside the words only while both fit, under them otherwise; no
 * answer's label breaks; and the item a row quotes is held to two lines.
 */
import "~/test/layout.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, actsApplied } from "~/test/layout.tsx";
import type { DecisionSubject } from "~/lib/decisions.ts";
import { DecisionList } from "./DecisionRow.tsx";

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Product Manager", handle: "pm", kind: "agent" },
    { name: "QA Engineer", handle: "qa", kind: "agent" },
    { name: "Senior App Engineer", handle: "app", kind: "agent" },
  ],
  units: [],
};

const WORDS =
  "GP-748 comment: Build: swap FlashcardIcon for the spec's line icon in the Library and Learning decks headers, then delete it, and keep the old icon until the release notes go out (frontend)";

const FOLD_IN = {
  kind: "fold_in",
  at: new Date().toISOString(),
  foldIn: {
    id: "f-1",
    turn_id: "run-1",
    launch_id: "l-1",
    seat: "qa",
    requester: "app",
    asker: "pm",
    asker_kind: "agent",
    conversation: "",
    words: WORDS,
    cut: 0,
    put_to: ["jane"],
    asked_at: new Date().toISOString(),
    decision: {
      question: "Add this to the running work?",
      options: [
        { id: "approve", label: "Add it to the running work" },
        { id: "separate", label: "Run it on its own" },
      ],
      recommended: "approve",
    },
    work_item: { backend: "native", id: "t-708", key: "ENG-708", project: "ENG" },
  },
} as unknown as DecisionSubject;

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal("fetch", actsApplied());
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
});

async function mount(width: number) {
  host = document.createElement("div");
  host.style.width = `${width}px`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <DecisionList subjects={[FOLD_IN]} decider={{ handle: "jane" }} now={Date.now()} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  for (let i = 0; i < 4; i++) await new Promise((r) => requestAnimationFrame(r));
  const row = host.querySelector<HTMLElement>(".decision-item")!;
  const words = row.querySelector<HTMLElement>(".decision-text")!;
  const sub = row.querySelector<HTMLElement>(".decision-sub")!;
  const buttons = Array.from(row.querySelectorAll<HTMLElement>(".decision-actions button"));
  const box = (el: Element) => el.getBoundingClientRect();
  const inner = box(row).width - parseFloat(getComputedStyle(row).paddingLeft) * 2;
  return { row, words, sub, buttons, box, inner };
}

// Just past the width a fixed breakpoint once stacked at: the reported case.
test("in a row too narrow for words and answers, the answers go under the words, which take the row", async () => {
  const { words, buttons, box, inner } = await mount(500);
  expect(buttons).toHaveLength(2);
  expect(box(words).width).toBeGreaterThan(inner - 1);
  for (const b of buttons) expect(box(b).top).toBeGreaterThanOrEqual(box(words).bottom);
});

test("in a wide row the answers sit beside words that keep a reading width", async () => {
  const { words, buttons, box } = await mount(900);
  for (const b of buttons) expect(box(b).left).toBeGreaterThan(box(words).right);
  expect(box(words).width).toBeGreaterThanOrEqual(320);
});

test.each([360, 500, 900])(
  "at %ipx no answer's label breaks, and the quoted item holds two lines",
  async (width) => {
    const { sub, buttons } = await mount(width);
    for (const b of buttons) {
      // The label's own words, one line of them.
      const tops = new Set<number>();
      const texts = document.createTreeWalker(b, NodeFilter.SHOW_TEXT);
      for (let t = texts.nextNode(); t; t = texts.nextNode()) {
        const range = document.createRange();
        range.selectNodeContents(t);
        for (const r of range.getClientRects()) if (r.width > 0) tops.add(Math.round(r.top));
      }
      expect(tops.size, b.textContent ?? "").toBe(1);
    }
    const line = parseFloat(getComputedStyle(sub).lineHeight);
    expect(sub.getBoundingClientRect().height).toBeLessThanOrEqual(2 * line + 1);
    expect(sub.title).toBe(WORDS);
  },
);
