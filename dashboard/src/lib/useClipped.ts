/**
 * Whether any line-clamped text under an element is cut short.
 *
 * FOR A CARD THAT OFFERS "SHOW ALL" ONLY WHEN THERE IS MORE TO SHOW. A clamp
 * (`.clamp`, `-webkit-line-clamp`) hides the rest of a paragraph without
 * saying so, and whether it hid anything is a fact about the laid-out box —
 * the words, the column's width and the face — that no count of characters
 * can stand in for: the same goal is three lines in a 340px side and one on a
 * phone held sideways. So it is MEASURED, after layout and again whenever the
 * box is resized or what it holds (`content`) changes: a clamped box that is
 * cut short is taller inside than it is drawn.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

/** A box whose content is taller than the box by more than a rounding pixel. */
export function isClipped(el: Element): boolean {
  return el.scrollHeight > el.clientHeight + 1;
}

export function useClipped(root: RefObject<HTMLElement | null>, content: unknown): boolean {
  const [clipped, setClipped] = useState(false);
  useLayoutEffect(() => {
    const node = root.current;
    if (!node) return;
    const measure = () => setClipped([...node.querySelectorAll(".clamp")].some(isClipped));
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    return () => watch.disconnect();
  }, [root, content]);
  return clipped;
}
