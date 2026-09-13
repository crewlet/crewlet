/**
 * The keyboard half of a listbox that follows a text field.
 *
 * WHY IT EXISTS. A field that offers a list while somebody types (the secret
 * completion, a searchable multi-select) keeps focus in the text box and
 * points at the highlighted option with `aria-activedescendant`. The keys
 * that move that highlight, take an option and dismiss the list are the same
 * in every one of them, and a second hand-written copy is how one list starts
 * wrapping at the ends while the other stops, or how Escape in one closes the
 * dialog around it.
 *
 * THE RULES IT KEEPS.
 *
 * - THE HIGHLIGHT WRAPS: Down from the last option goes to the first, Up from
 *   the first to the last.
 * - IT IS CLAMPED, NOT RESET, when the list shrinks under somebody's typing, so
 *   the highlight stays on a row that exists instead of jumping to the top.
 * - ENTER TAKES THE HIGHLIGHTED OPTION and is prevented, because the field is
 *   usually inside a form that submits on Enter, and choosing is not saving.
 *   Tab takes it too where the list is a completion (`tabCommits`); a
 *   multi-select leaves Tab to move focus, as every form control does.
 * - ESCAPE CLOSES THE LIST AND NOTHING ELSE. It stops there, so the dialog or
 *   drawer the field sits in stays open; a second Escape reaches it. That
 *   holds for a list that is showing with nothing in it too ("Nothing
 *   matches"), and for nothing else: `open` means the reader can see a list,
 *   so an Escape with none on screen goes straight to the surface around it.
 * - THE LIST IS A POPUP ON THE LAYER STACK (`usePopup`), so a press outside
 *   it closes the list before anything beneath it: a press on a dialog's veil
 *   while the list is open closes the list and leaves the dialog. The
 *   component attaches `listRef` to the list it draws and `anchorRef` to the
 *   element around the field, whose presses are not outside.
 * - A PRESS ON AN OPTION IS TAKEN ON `mousedown`, with the default prevented:
 *   the field would otherwise blur first, and a list that closes on blur takes
 *   the row out from under the click that was choosing it.
 *
 * It owns no markup and no filtering: the component renders the list, decides
 * what is offered and what taking an option means.
 */

import { useState, type KeyboardEvent, type MouseEvent } from "react";
import { usePopup } from "./useModal.ts";

export interface ListboxOptions {
  /** A stable base for element ids, usually from `useId`. */
  id: string;
  /** Whether a list is on screen, including one that says nothing matches. */
  open: boolean;
  /** How many options are offered right now. */
  count: number;
  /** Taking the option at `index`. */
  onCommit: (index: number) => void;
  /** Escape. */
  onClose: () => void;
  /** Whether Tab takes the highlighted option (a completion) or moves focus (a form control). */
  tabCommits?: boolean;
}

export interface Listbox {
  /** The highlighted option's index, clamped to what is offered; -1 when nothing is. */
  active: number;
  setActive: (index: number) => void;
  /** The listbox element's id. */
  listId: string;
  /** The list on screen: what a press outside is measured against. */
  listRef: (el: HTMLElement | null) => void;
  /** The element around the field: a press on it is not outside the list. */
  anchorRef: (el: HTMLElement | null) => void;
  /** The id of the option element at `index`. */
  optionId: (index: number) => string;
  /** The field's keydown handler. Returns whether it handled the key. */
  onKeyDown: (e: KeyboardEvent<HTMLElement>) => boolean;
  /** Mouse wiring for the option element at `index`. */
  optionHandlers: (index: number) => {
    onMouseDown: (e: MouseEvent<HTMLElement>) => void;
    onMouseEnter: () => void;
  };
}

export function useListbox({
  id,
  open,
  count,
  onCommit,
  onClose,
  tabCommits = false,
}: ListboxOptions): Listbox {
  const [at, setAt] = useState(0);
  const active = count === 0 ? -1 : Math.min(Math.max(at, 0), count - 1);
  const listId = `${id}-listbox`;
  const popup = usePopup({ open, onDismiss: () => onClose() });

  function onKeyDown(e: KeyboardEvent<HTMLElement>): boolean {
    if (!open) return false;
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      onClose();
      return true;
    }
    if (count === 0) return false;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setAt((active + 1) % count);
        return true;
      case "ArrowUp":
        e.preventDefault();
        setAt((active - 1 + count) % count);
        return true;
      case "Enter":
        e.preventDefault();
        onCommit(active);
        return true;
      case "Tab":
        if (!tabCommits || e.shiftKey) return false;
        e.preventDefault();
        onCommit(active);
        return true;
      default:
        return false;
    }
  }

  return {
    active,
    setActive: setAt,
    listId,
    listRef: popup.panelRef,
    anchorRef: popup.insideRef,
    optionId: (index) => `${listId}-${index}`,
    onKeyDown,
    optionHandlers: (index) => ({
      onMouseDown: (e) => {
        e.preventDefault();
        onCommit(index);
      },
      onMouseEnter: () => setAt(index),
    }),
  };
}
