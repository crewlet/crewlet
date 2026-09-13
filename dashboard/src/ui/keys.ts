/**
 * Keyboard facts every primitive has to read the same way.
 *
 * WHILE AN INPUT METHOD IS COMPOSING, THE KEYS BELONG TO IT. Somebody typing
 * Japanese, Chinese or Korean builds each word in a composition: the arrows
 * walk the candidates, Enter accepts one and Escape abandons it. Those presses
 * still reach the page as `keydown` events, so a list that read that Enter as
 * "take the highlighted option", a list field that read it as "add this item",
 * or a modal that read that Escape as "close" acted on a key the reader had
 * pressed for the input method, and search closed or navigated in the middle
 * of a word.
 *
 * Browsers flag such a press with `isComposing`. Safari delivers the Enter
 * that ends a composition after it has already cleared that flag, marked only
 * by the legacy key code 229 ("processed by an input method"), so both are
 * read. One definition, because a primitive that checked only the flag would
 * still act on that Enter in Safari while the others did not.
 */

import type { KeyboardEvent as ReactKeyboardEvent } from "react";

/** The key code a browser reports for a press its input method consumed. */
const IME_PROCESS_KEY = 229;

/** Whether this press belongs to an input method's composition rather than to the page. */
export function isComposing(e: KeyboardEvent | ReactKeyboardEvent): boolean {
  const native = "nativeEvent" in e ? e.nativeEvent : e;
  return native.isComposing || native.keyCode === IME_PROCESS_KEY;
}
