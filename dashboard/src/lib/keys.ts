/**
 * The keyboard: one predicate for "is somebody typing", and one way to bind a
 * chord.
 *
 * # The predicate
 *
 * Every shortcut in this product is a bare letter, so every one of them has to
 * ask whether the reader is in a field first — and it was written out at each
 * of five listeners, byte for byte. Five copies of a rule is five chances for
 * one to be missed when the rule changes, and the failure is silent and
 * specific: a `j` typed into a task's title scrolls the grid behind it.
 *
 * The rule is also INCOMPLETE as those copies wrote it. `<select>` takes the
 * keyboard, and so does any element a screen gave `tabindex` and its own key
 * handling — a tree row, a slider, a listbox. Both are caught here, and
 * catching them once is the whole argument for the file.
 *
 * # The binding
 *
 * ONE LISTENER PER HOOK, not per chord: a component with four shortcuts adds
 * one `keydown` handler rather than four, and the set it matches is a value
 * rather than a chain of ifs. The `g`-prefix grammar is here too, because a
 * prefix is state between two events and every component that wanted one grew
 * its own timer.
 */

import { useEffect, useRef } from "react";
import { isModalLayerOpen } from "@crewlethq/ui";

/**
 * Whether the keyboard currently belongs to something the reader is typing in.
 *
 * READS THE ACTIVE ELEMENT rather than the event's target, because the two
 * differ exactly when it matters: a `keydown` that bubbles to `window` from
 * inside a field has the field as its target, but a handler bound to the
 * window during an IME composition or after a programmatic focus sees the
 * document. The active element is the browser's own answer to "where is the
 * keyboard".
 */
export function isTyping(): boolean {
  const el = document.activeElement;
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  switch (el.tagName) {
    case "INPUT":
    case "TEXTAREA":
    // A SELECT TAKES THE KEYBOARD TOO — letters jump to an option — so a
    // bare-letter shortcut fired over an open one is a shortcut that also
    // changed a value the reader was choosing.
    case "SELECT":
      return true;
  }
  // AND ANYTHING A SCREEN MADE FOCUSABLE AND HANDLES KEYS FOR: a tree row, a
  // slider, a listbox. `tabindex` alone is not enough — a div made focusable
  // to receive a click is common and handles nothing — so it is the pair,
  // which is what an interactive role means.
  return el.hasAttribute("tabindex") && el.hasAttribute("role");
}

/** One chord and what it does. */
export interface Chord {
  /** The key, as `KeyboardEvent.key` lowercased. `"escape"`, `"["`, `"j"`. */
  key: string;
  /** Run it. */
  run: (e: KeyboardEvent) => void;
  /** Require the platform's command modifier (⌘ on a Mac, Ctrl elsewhere). */
  meta?: boolean;
  /** Fire even while the reader is typing. For `escape`, and little else. */
  whileTyping?: boolean;
  /** Skip this binding without changing the shape of the list. */
  when?: boolean;
}

/** A chord that only fires after a prefix key. */
export interface Sequence extends Chord {
  /** The prefix, e.g. `"g"`. */
  after: string;
}

/**
 * How long a prefix stays armed.
 *
 * One second: long enough that `g` then `w` is comfortable at a normal typing
 * speed, short enough that a `g` typed by accident does not swallow the next
 * letter a reader presses with some other intent. It is the value every
 * product with this grammar converged on, and the cost of being wrong in
 * either direction is one ignored keystroke.
 */
export const PrefixWindow = 1_000;

/**
 * Bind a set of chords for as long as the component is mounted.
 *
 * THE LIST IS READ THROUGH A REF, so a caller may pass a fresh array every
 * render — which every caller does, since the handlers close over props —
 * without tearing the listener down and rebuilding it on each one. The
 * alternative is a dependency array over an array literal, which is never
 * equal and so re-subscribes on every render.
 */
export function useKeyChords(chords: (Chord | Sequence)[]): void {
  const held = useRef(chords);
  held.current = chords;

  useEffect(() => {
    let armed = "";
    let timer: number | undefined;
    function disarm(): void {
      armed = "";
      if (timer) window.clearTimeout(timer);
      timer = undefined;
    }

    function onKey(e: KeyboardEvent): void {
      // A SURFACE ON THE LAYER STACK OWNS THE KEYBOARD, and this is the one
      // rule the page cannot infer from the event. The stack calls
      // `preventDefault()` on a press it handled and deliberately NOT
      // `stopPropagation()` — which is the defensible choice for a library,
      // and means every press still arrives here. So one Escape inside a
      // dialog closed the dialog AND ran the page's own escape chord behind
      // it: the peek rail under an open dialog shut at the same time, from a
      // press the reader aimed at the dialog.
      //
      // It fails silently in the shape that matters — the dialog does close,
      // so the gesture looks like it worked, and the second thing that closed
      // is behind the surface you were reading. `isModalLayerOpen` is
      // published for exactly this and is the only honest test: "was this
      // press already somebody's" is a fact about the stack, not about the
      // event.
      if (isModalLayerOpen()) return;

      const key = e.key.toLowerCase();
      const meta = e.metaKey || e.ctrlKey;
      const typing = isTyping();

      // A PREFIX IN FLIGHT CONSUMES THE NEXT KEY, matched or not. Half a
      // sequence that fell through to the plain chords would make `g` then
      // `k` scroll a grid, which is the one thing the reader who typed `g`
      // did not ask for.
      if (armed) {
        const prefix = armed;
        disarm();
        if (typing) return;
        for (const chord of held.current) {
          if (!("after" in chord) || chord.after !== prefix) continue;
          if (chord.when === false || chord.key !== key) continue;
          e.preventDefault();
          chord.run(e);
          return;
        }
        return;
      }

      for (const chord of held.current) {
        if (chord.when === false) continue;
        if (chord.key !== key) continue;
        if ("after" in chord) continue;
        if (Boolean(chord.meta) !== meta) continue;
        if (typing && !chord.whileTyping) continue;
        e.preventDefault();
        chord.run(e);
        return;
      }

      // ARM A PREFIX LAST, so a plain chord on the same letter wins. A
      // screen that binds both `g` and `g i` has said the first is what a
      // bare `g` means.
      if (typing || meta || e.altKey) return;
      if (held.current.some((c) => "after" in c && c.after === key && c.when !== false)) {
        armed = key;
        timer = window.setTimeout(disarm, PrefixWindow);
      }
    }

    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      disarm();
    };
  }, []);
}
