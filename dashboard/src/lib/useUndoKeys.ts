/**
 * Undo and redo a draft from the keyboard, for a surface that keeps one: the
 * org builder's chart, Home's board while it is arranged.
 *
 * ONE LISTENER, READ FROM THE TABLE. The keys are the table's `draft.undo` and
 * `draft.redo` rows (`app/keymap.ts`), read through `matchesRow`, so the legend that lists them and the listener that
 * answers them cannot disagree. It is its own document listener rather than a
 * window chord (`useKeymap`) because a chord compares no Shift, and Mod+Shift+Z
 * is redo.
 *
 * SCOPED TO THE SURFACE. A press counts from inside `container` (or from the
 * page itself, where focus rests when nothing has it), never from a text field
 * (where the same keys undo typing) and never under a modal layer, which holds
 * a draft of its own.
 */

import { useEffect, useRef, type RefObject } from "react";
import { isComposing, isModalLayerOpen } from "@crewlethq/ui";
import { matchesRow } from "~/app/keymap.ts";

export function useUndoKeys({
  container,
  enabled = true,
  onUndo,
  onRedo,
}: {
  container: RefObject<HTMLElement | null>;
  /** Whether a draft is being kept now. */
  enabled?: boolean;
  onUndo: () => void;
  onRedo: () => void;
}): void {
  // The latest handlers, so a draft that changes every press does not re-add
  // the listener every press.
  const handlers = useRef({ onUndo, onRedo });
  handlers.current = { onUndo, onRedo };
  useEffect(() => {
    if (!enabled) return;
    function onKey(e: KeyboardEvent) {
      if (e.defaultPrevented || isComposing(e) || isModalLayerOpen()) return;
      if (e.altKey) return;
      const redoes = matchesRow("draft.redo", e);
      if (!redoes && !matchesRow("draft.undo", e)) return;
      const root = container.current;
      const target = e.target instanceof HTMLElement ? e.target : null;
      if (!root || (target && target !== document.body && !root.contains(target))) return;
      if (target && isTextEntry(target)) return;
      e.preventDefault();
      if (redoes) handlers.current.onRedo();
      else handlers.current.onUndo();
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [enabled, container]);
}

/** Whether keys pressed in an element edit text there. */
export function isTextEntry(el: HTMLElement): boolean {
  if (el.isContentEditable) return true;
  if (el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) return true;
  if (!(el instanceof HTMLInputElement)) return false;
  return !["checkbox", "radio", "button", "submit", "reset", "range", "color", "file"].includes(
    el.type,
  );
}
