/**
 * How Structure's charts are drawn on the kit's tree canvas: whole.
 *
 * EVERY CHART OPENS FITTED, the whole company in the canvas and centred,
 * however wide it is, and fits again after every layout action (the kit's
 * `fitKey`, see `routes/org/builder/chartPlace.ts`). A reader zooms in to read
 * a card. This replaced a legibility floor (85%) held by pressing the
 * canvas's own controls, which opened a wide company cut at its edges, with
 * the reader left to find the rest.
 */

/**
 * The lowest zoom a chart's fit goes to: low enough that every company opens
 * whole. The kit's own floor (25%) cut a thirty-seat company at both edges at
 * 1440px; at 2% a company of hundreds of seats still fits.
 */
export const CHART_FIT_FLOOR = 0.02;

/** The label the canvas's own Fit control carries on Structure. */
export const CANVAS_LABELS = { fit: "Fit to view" } as const;
