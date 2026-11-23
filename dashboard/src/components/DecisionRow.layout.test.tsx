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

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "update_work_item"],
};

/** An ask put to the reader by `by`, with a recommendation, so it offers Accept and Change. */
function ask(n: number, by: string): DecisionSubject {
  const at = new Date(Date.now() - n * 60_000).toISOString();
  return {
    kind: "ask",
    at,
    ask: {
      id: `t-${n}`,
      key: `ENG-${n}`,
      project: "ENG",
      title: `Item ${n}`,
      type: "task",
      status: "todo",
      comment: `c-${n}`,
      asked_by: by,
      asked_at: at,
      body: "",
      decision: {
        question: `Ship change ${n} to every host tonight, or wait for staging?`,
        options: [
          { id: "ship", label: "Ship it tonight" },
          { id: "wait", label: "Wait for staging" },
        ],
        recommended: "ship",
        role: "approver",
      },
      answer_with: "",
    },
  } as unknown as DecisionSubject;
}

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

// A FITTED LIST BEING ANSWERED IN: the options an ask opens grow its block,
// and fitted it was hidden mid-answer, its field and focus with it.
test("answering in a fitted list keeps the ask being answered in view, alone", async () => {
  host = document.createElement("div");
  host.style.cssText = "width: 520px; height: 240px; display: flex; flex-direction: column;";
  document.body.append(host);
  root = createRoot(host);
  const fits: number[] = [];
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{ viewer: VIEWER }} org={ORG}>
        <DecisionList
          subjects={[ask(1, "pm"), ask(2, "qa"), ask(3, "app")]}
          decider={{ handle: "jane" }}
          now={Date.now()}
          onFit={(n) => fits.push(n)}
        />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  for (let i = 0; i < 4; i++) await new Promise((r) => requestAnimationFrame(r));
  const change = await vi.waitFor(() => {
    const b = Array.from(host!.querySelectorAll("button")).find((x) => x.textContent === "Change");
    if (!b || b.hasAttribute("disabled") || b.getAttribute("aria-disabled") === "true")
      throw new Error("Change is not ready");
    return b;
  });
  change.click();
  await vi.waitFor(() => expect(host!.querySelector(".decision-answer.is-open")).not.toBeNull());
  const groups = host.querySelectorAll(".decision-group");
  expect(groups).toHaveLength(1);
  expect(getComputedStyle(groups[0]!).visibility).toBe("visible");
  expect(fits.at(-1)).toBe(1);
});
