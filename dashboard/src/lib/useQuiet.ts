/**
 * A value that moves only once it has stopped moving.
 *
 * FOR A QUESTION ASKED AS SOMEBODY TYPES, which is a question per keystroke
 * otherwise: the composer asks whether a title is taken and which pages
 * already cover it, and asked on every key, a ten-letter title is ten reads
 * of the socket's four slots for nine answers nobody looks at. The value
 * settles after `ms` without a change, and the first value is the settled one
 * at once, so a screen that opens with text in a field asks about it
 * immediately.
 */

import { useEffect, useState } from "react";

/**
 * How long a typed value has to rest before it is asked about: about the gap
 * between two keystrokes of somebody pausing to think, so a pause asks and a
 * word being typed does not.
 */
export const QUIET_MS = 350;

export function useQuiet<T>(value: T, ms: number = QUIET_MS): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    if (Object.is(value, settled)) return;
    const timer = window.setTimeout(() => setSettled(value), ms);
    return () => window.clearTimeout(timer);
  }, [value, settled, ms]);
  return settled;
}
