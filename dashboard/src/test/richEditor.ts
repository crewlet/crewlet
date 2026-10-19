/**
 * Writing in the design system's rich editor from a screen's suite.
 *
 * The editor is a contenteditable document, not a field, so a suite cannot
 * set its value: it edits the document's text the way typing does, by
 * changing a text node, and the editor reads the change off the DOM exactly
 * as it reads a keystroke's. The body the screen receives is then whatever
 * the editor writes for that change, which is the claim worth testing.
 *
 * Only a suite imports this.
 */

import { act } from "react";
import { preloadRichEditor } from "@crewlethq/ui";

/** Fetch the editor's lazily loaded code before a suite draws one. */
export function loadRichEditor(): Promise<void> {
  return preloadRichEditor();
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
