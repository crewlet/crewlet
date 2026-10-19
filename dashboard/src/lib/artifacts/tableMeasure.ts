/**
 * How wide a widget table's text is drawn, measured rather than estimated:
 * the fonts the compact table sets its cells and its headings in, resolved
 * from the design system's tokens on this document, measured on a canvas.
 *
 * WHY MEASURED: a column is held at what it holds, and an estimate by the
 * character reserved too much for lowercase words and too little for
 * capitals, so one table both ran past its widget and cut an identifier
 * (`DEP-1050` as `DEP-105…`). A canvas measures the run of text itself.
 *
 * Read once per document and kept: the tokens are the stylesheet's, and a
 * density change re-renders the board. Where there is no canvas (a suite's
 * DOM), the board falls back to the estimate.
 */

import { estimateTableText, type TableMeasure } from "./dashboard.ts";

let cached: TableMeasure | null = null;

/** Resolves a font's parts from the tokens, the way a cell or a heading of the compact table is set. */
function fontOf(size: string, weight: string): { font: string; tracking: number } {
  const probe = document.createElement("span");
  probe.style.cssText = `position:absolute;visibility:hidden;font-family:var(--font-family-sans);font-size:${size};font-weight:${weight};letter-spacing:var(--font-letter-spacing-wide)`;
  document.body.appendChild(probe);
  const style = getComputedStyle(probe);
  const font = `${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
  const tracking = Number.parseFloat(style.letterSpacing) || 0;
  probe.remove();
  return { font, tracking };
}

export function tableMeasure(): TableMeasure {
  if (cached) return cached;
  const context =
    typeof document === "undefined" || typeof OffscreenCanvas === "undefined"
      ? null
      : new OffscreenCanvas(1, 1).getContext("2d");
  if (!context) return estimateTableText;
  const cell = fontOf("var(--font-size-compact)", "var(--font-weight-regular)");
  const head = fontOf("var(--font-size-2xs)", "var(--font-weight-medium)");
  cached = (text, heading) => {
    const f = heading ? head : cell;
    context.font = f.font;
    // A heading is tracked: each character carries the spacing after it.
    return context.measureText(text).width + (heading ? f.tracking * text.length : 0);
  };
  return cached;
}
