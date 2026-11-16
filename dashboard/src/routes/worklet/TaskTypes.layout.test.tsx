/**
 * A task type's row at a phone's width: the slug is drawn whole and "built in"
 * stays on one line beside the Rename and Archive actions.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import "~/test/layout.tsx";
import { TypeRow } from "./TaskTypes.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

async function mount(width: number) {
  await page.viewport(390, 844);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <TypeRow
        type={{ slug: "milestone", name: "Milestone", builtin: true }}
        blocked={undefined}
        onRename={() => {}}
        onArchive={() => {}}
      />,
    ),
  );
  await document.fonts.ready;
}

// 326px is the type list inside the dialog on a 390px screen.
test("at a phone's width a type's slug is whole and built in is one line", async () => {
  await mount(326);
  const slug = host!.querySelector<HTMLElement>(".task-type-slug")!;
  expect(slug.scrollWidth).toBeLessThanOrEqual(slug.clientWidth);
  const builtIn = [...host!.querySelectorAll<HTMLElement>("span")].find(
    (s) => s.textContent === "built in",
  )!;
  const line = parseFloat(getComputedStyle(builtIn).lineHeight);
  expect(builtIn.getBoundingClientRect().height).toBeLessThan(line * 1.5);
  // Both actions stay on the row, to the words' right.
  const words = host!.querySelector<HTMLElement>(".task-type-words")!.getBoundingClientRect();
  const actions = host!.querySelector<HTMLElement>(".task-type-actions")!.getBoundingClientRect();
  expect(actions.left).toBeGreaterThanOrEqual(words.right);
  expect(actions.right).toBeLessThanOrEqual(326 + 0.5);
});
