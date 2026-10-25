/**
 * Writing in the design system's rich editor from a screen's suite.
 *
 * The editor is a contenteditable document, not a field, so a suite cannot
 * set its value: it edits the document's text the way typing does, by
 * changing a text node, and the editor reads the change off the DOM exactly
 * as it reads a keystroke's. The body the screen receives is then whatever
 * the editor writes for that change, which is the claim worth testing.
 *
 * Only a suite imports this, and a suite that draws the editor loads it
 * through [loadRichEditor], which also applies the editor's stylesheet.
 */

import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
// TESTING-LIBRARY'S `act`, NOT REACT'S: it declares the act environment for
// its length, which React's own leaves to a global this suite does not set
// (the runner's hooks are imported, not globals), and the editor's update
// from inside one then drew React's "The current testing environment is not
// configured to support act(...)" into the output of whichever suite wrote.
import { act } from "@testing-library/react";
import { preloadRichEditor } from "@crewlethq/ui";

/**
 * Fetch the editor's lazily loaded code, and apply its stylesheet, before a
 * suite draws one.
 *
 * THE STYLESHEET IS PART OF THE EDITOR. ProseMirror reads its content's
 * computed `white-space` when it makes a view, and Vitest hands every `.css`
 * import back empty, so it found `normal` where the kit sets `break-spaces`:
 * it warned ("ProseMirror expects the CSS white-space property to be set"),
 * which the console gate fails, and edited as it would in a browser that
 * never loaded the stylesheet, which is not the browser anybody runs. So the
 * kit's own stylesheet for the editor is applied, whatever its build named
 * it, rather than a copy of the one rule that would drift from it.
 *
 * HERE, NOT IN `vitest.config.ts` (`css.include`): that applies the sheet to
 * every suite that imports the kit, and jsdom matches every rule of every
 * sheet against an element each time a query asks whether it is visible,
 * which measured several percent slower on suites that never draw an editor.
 */
export async function loadRichEditor(): Promise<void> {
  applyEditorStylesheet();
  await preloadRichEditor();
}

/**
 * The kit's build, where its stylesheets sit beside its code. From
 * `process.cwd()`, as `source.ts` finds the tree: under the jsdom environment
 * a module's URL is the test server's rather than a file, and Vitest runs from
 * `dashboard/`.
 */
const KIT_DIST = join(process.cwd(), "node_modules", "@crewlethq", "ui", "dist");

function applyEditorStylesheet(): void {
  if (document.head.querySelector("style[data-kit-sheet='rich-editor']")) return;
  const sheet = readdirSync(KIT_DIST).find((name) => /^RichEditor-[^/]*\.css$/.test(name));
  if (!sheet) {
    throw new Error(
      `no RichEditor-*.css in ${KIT_DIST}: the kit's build moved the editor's stylesheet, ` +
        "so find it there and name it here",
    );
  }
  const style = document.createElement("style");
  style.dataset.kitSheet = "rich-editor";
  style.textContent = readFileSync(join(KIT_DIST, sheet), "utf8");
  document.head.append(style);
}

/** The first text node of `box` whose text holds `text`. */
function textNode(box: HTMLElement, text: string): Text {
  const walker = box.ownerDocument.createTreeWalker(box, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if ((node.textContent ?? "").includes(text)) return node as Text;
  }
  throw new Error(`no text in the editor reads “${text}”`);
}

/** Replace the first run of `from` with `to`, as a writer retyping it would. */
export async function retype(box: HTMLElement, from: string, to: string): Promise<void> {
  const node = textNode(box, from);
  await act(async () => {
    node.data = node.data.replace(from, to);
    // The editor reads a change off its mutation observer, a microtask later.
    await Promise.resolve();
  });
}

/** Add a paragraph at the end, as a writer pressing Enter and typing would. */
export async function append(box: HTMLElement, text: string): Promise<void> {
  const p = box.ownerDocument.createElement("p");
  p.textContent = text;
  await act(async () => {
    box.appendChild(p);
    await Promise.resolve();
  });
}

/** Whether the editor is read only, as it is while a merge waits. */
export function readOnly(box: HTMLElement): boolean {
  return box.getAttribute("contenteditable") === "false";
}

/** One block a suite writes: a paragraph, or a heading at its Markdown level. */
export interface Written {
  text: string;
  heading?: number;
}

/**
 * Replace everything in the editor with `blocks`, as a writer selecting all
 * and typing would. A heading is drawn one level down from its Markdown (a
 * `#` is an `h2`) and names its level, as the editor's own headings do.
 */
export async function fill(box: HTMLElement, blocks: Written[]): Promise<void> {
  const doc = box.ownerDocument;
  const elements = blocks.map(({ text, heading }) => {
    const el = doc.createElement(heading ? `h${Math.min(6, heading + 1)}` : "p");
    if (heading) el.setAttribute("data-level", String(heading));
    el.textContent = text;
    return el;
  });
  await act(async () => {
    box.replaceChildren(...elements);
    await Promise.resolve();
  });
}
