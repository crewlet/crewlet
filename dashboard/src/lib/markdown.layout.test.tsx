/**
 * A TASK LIST IS ONE LINE PER ITEM wherever the renderer draws it: no marker,
 * the box on the first line of its words, and a wrapped line under the words,
 * not under the box. The reading pane's lead carries no `.prose.md`, which is
 * where the box once stood alone on a line above its words, under a bullet.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { Document, ReadingPaneLead } from "@crewlethq/ui";
import { CHECKED_WORDS, renderMarkdown } from "./markdown.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

const LONG =
  "A phrase that is not indexable is absent from every list on the page, and the example clip falls back to the next enabled clip.";
const SOURCE = `- [ ] ${LONG}\n- [x] Seven shown phrases give \`noindex\`\n- [ ] Short`;

const HOSTS: [string, (words: ReactNode) => ReactNode][] = [
  ["a reading pane's lead", (words) => <ReadingPaneLead>{words}</ReadingPaneLead>],
  ["rendered prose", (words) => <div className="prose md">{words}</div>],
  ["a document", (words) => <Document>{words}</Document>],
];

for (const [name, wrap] of HOSTS) {
  test(`a task item in ${name} sets its box on its first line, beside its words`, () => {
    host = document.createElement("div");
    host.style.width = "420px";
    document.body.append(host);
    root = createRoot(host);
    flushSync(() => root!.render(wrap(renderMarkdown(SOURCE, { ...CHECKED_WORDS, scoped: true }))));

    const list = host.querySelector("ul.md-tasks")!;
    expect(getComputedStyle(list).listStyleType).toBe("none");
    for (const item of Array.from(list.querySelectorAll(":scope > li"))) {
      const box = item.querySelector("input[type=checkbox]")!.getBoundingClientRect();
      const words = item.querySelector(".md-task-body")!;
      const range = document.createRange();
      range.selectNodeContents(words);
      const lines = Array.from(range.getClientRects()).filter((r) => r.width > 0);
      const first = lines[0]!;
      // The box's middle is inside the words' first line, and the words start after it.
      const middle = (box.top + box.bottom) / 2;
      expect(middle).toBeGreaterThan(first.top);
      expect(middle).toBeLessThan(first.bottom);
      expect(words.getBoundingClientRect().left).toBeGreaterThan(box.right);
      // Every line of the words starts at the words' edge, none under the box.
      for (const line of lines) expect(line.left).toBeGreaterThanOrEqual(box.right);
    }
    // The long item wraps, so the check above held for a second line too.
    const longLines = new Set(
      Array.from(
        (() => {
          const r = document.createRange();
          r.selectNodeContents(list.querySelector(".md-task-body")!);
          return r.getClientRects();
        })(),
      ).map((r) => Math.round(r.top)),
    );
    expect(longLines.size).toBeGreaterThan(1);
  });
}
