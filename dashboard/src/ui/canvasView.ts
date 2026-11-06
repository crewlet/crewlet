/**
 * How Structure's charts are drawn on the kit's tree canvas: whole, and read.
 *
 * EVERY CHART OPENS FITTED, what it draws in the canvas and centred, and fits
 * again after every layout action (the kit's `fitKey`, see
 * `routes/org/builder/chartPlace.ts`). This replaced a legibility floor (85%)
 * held by pressing the canvas's own controls, which opened a wide company cut
 * at its edges, with the reader left to find the rest.
 *
 * A CHART TOO WIDE TO READ OPENS ON ITS TOP RANKS rather than smaller
 * ([CHART_LEGIBLE_ZOOM]): what is drawn is still whole and fitted, and the
 * rest is one branch or Expand all away, which do fit it at any size.
 */

/**
 * The lowest zoom a chart's fit goes to: low enough that every company opens
 * whole. The kit's own floor (25%) cut a thirty-seat company at both edges at
 * 1440px; at 2% a company of hundreds of seats still fits.
 */
export const CHART_FIT_FLOOR = 0.02;

/**
 * The smallest zoom a chart OPENS at on its own. At half size a card's 13px
 * name draws at about 6.5px, the least that still reads as words, and fourteen
 * seats in one rank open at a quarter of it at 1440px and an eighth on a phone.
 */
export const CHART_LEGIBLE_ZOOM = 0.5;

/** The label the canvas's own Fit control carries on Structure. */
export const CANVAS_LABELS = { fit: "Fit to view" } as const;
