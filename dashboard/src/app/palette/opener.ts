/**
 * Opening the command palette from a screen.
 *
 * The palette is the frame's (`app/Shell.tsx` mounts it, one at a time, in a
 * boundary of its own), and a screen that wants it open — Home's "New task",
 * which files through the palette's "Create task" — asks the frame rather than
 * mounting a second one. A context rather than an event on `window`, so a
 * screen rendered outside the frame (a suite) gets a function that does
 * nothing rather than one that reaches a palette that is not there.
 */

import { createContext, useContext } from "react";
import type { ScopeId } from "./hits.ts";

export type OpenPalette = (scope?: ScopeId) => void;

export const PaletteOpener = createContext<OpenPalette>(() => {});

/** Open the palette, on a scope where the caller names one. */
export function useOpenPalette(): OpenPalette {
  return useContext(PaletteOpener);
}
