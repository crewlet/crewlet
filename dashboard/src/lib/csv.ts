/**
 * A table as CSV, for an export a reader opens in a spreadsheet.
 *
 * ONE WRITER for every export in the product. The audit log had its own, and
 * it quoted correctly and guarded nothing else — so a cell a model or a
 * vendor wrote that began with `=` opened as a FORMULA: a turn's summary,
 * a task title or a model name is text somebody other than the reader chose,
 * and a spreadsheet evaluates `=HYPERLINK(…)` in it without asking.
 *
 * Two rules, both from what a spreadsheet does with a file rather than from
 * RFC 4180 alone:
 *
 *  - EVERY TEXT CELL IS QUOTED, with its quotes doubled, so a comma, a quote
 *    or a newline inside it can never shift the columns after it — the
 *    failure that makes an export worse than none, because the file opens
 *    without complaint.
 *  - A TEXT CELL THAT A SPREADSHEET WOULD READ AS A FORMULA — one starting
 *    with `=`, `+`, `-`, `@`, a tab or a carriage return — is prefixed with a
 *    single quote, the spreadsheet's own "this is text" mark. A NUMBER is
 *    written bare and never prefixed, so a count stays a number a reader can
 *    sum.
 */

/** One cell: text, a number, or nothing. */
export type CsvCell = string | number | null | undefined;

/** The first characters a spreadsheet takes as the start of a formula. */
const FORMULA_LEAD = /^[=+\-@\t\r]/;

/** One cell as CSV. See the file's two rules. */
export function csvCell(value: CsvCell): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "number") return Number.isFinite(value) ? String(value) : "";
  const text = FORMULA_LEAD.test(value) ? `'${value}` : value;
  return `"${text.replaceAll('"', '""')}"`;
}

/**
 * A header and its rows as one CSV document, lines joined by CRLF — the
 * separator RFC 4180 names, which every spreadsheet reads and a bare `\n`
 * inside a quoted cell stays distinguishable from.
 */
export function toCsv(header: readonly string[], rows: readonly (readonly CsvCell[])[]): string {
  return [header.map(csvCell), ...rows.map((row) => row.map(csvCell))]
    .map((cells) => cells.join(","))
    .join("\r\n");
}
