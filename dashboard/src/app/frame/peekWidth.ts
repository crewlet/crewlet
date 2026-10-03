/**
 * How wide the peek RESTS on the screen under it, asked for by the screen.
 *
 * # Why a screen may ask
 *
 * The peek is one rail for every screen (`PeekHost`), and 420 is right beside
 * a list: the list keeps its floor and the rail has room for a task's body.
 * Beside a CHART it is wrong in a way no floor protects against: a chart does
 * not reflow into a narrower column, it is SHRUNK into it, so every pixel the
 * rail takes is a pixel the whole company is drawn smaller in. The approved
 * org chart rests its peek at 330 for exactly that reason. So a screen that
 * draws a canvas asks for the canvas width ([CANVAS_PEEK_WIDTH]), the way it
 * asks for the window's height (`app/fill.tsx`): the screen that knows says
 * so, the frame that draws the rail reads it, and nothing in between agrees
 * about a class name.
 *
 * # The reader's width still wins
 *
 * A width the reader DRAGGED is their preference and is kept on every screen
 * (`DetailRail`); a screen's request is the width the rail rests at until they
 * do. Withdrawn when the screen unmounts, so the next screen's rail rests at
 * the frame's own width rather than at the last screen's.
 */

import { createContext, useContext, useEffect } from "react";
import { PEEK_WIDTH } from "../layout.ts";

/** Set by the shell; a no-op wherever a screen renders without one. */
export const PeekWidthRequest = createContext<(width: number | null) => void>(() => {});

/** The width the rail rests at on the screen now: the frame's, or the screen's own. */
export const PeekRestingWidth = createContext<number>(PEEK_WIDTH);

/**
 * Rest the peek at `width` while this screen is mounted — or, given null, ask
 * nothing, for a screen that draws its canvas only at some widths.
 */
export function usePeekWidth(width: number | null): void {
  const request = useContext(PeekWidthRequest);
  useEffect(() => {
    request(width);
    return () => request(null);
  }, [request, width]);
}
