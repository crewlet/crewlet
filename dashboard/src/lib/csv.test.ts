import { describe, expect, test } from "vitest";
import { csvCell, toCsv } from "./csv.ts";

describe("a CSV cell", () => {
  // A COMMA, A QUOTE OR A NEWLINE NEVER MOVES THE COLUMNS AFTER IT.
  test("quotes every text cell and doubles its quotes", () => {
    expect(csvCell('say "done", then\nstop')).toBe('"say ""done"", then\nstop"');
  });

  // A CELL SOMEBODY ELSE WROTE IS TEXT, NEVER A FORMULA — a spreadsheet
  // evaluates a leading `=`, `+`, `-` or `@` the moment the file opens.
  test.each(["=1+1", "+SUM(A1)", "-2+3", "@cmd", "\tx", "\rx"])(
    "marks %j as text rather than a formula",
    (value) => {
      expect(csvCell(value)).toBe(`"'${value.replaceAll('"', '""')}"`);
    },
  );

  // A COUNT STAYS A NUMBER a reader can sum — a negative one included.
  test("writes a number bare, and nothing for an absent or non-finite one", () => {
    expect(csvCell(48_600_000)).toBe("48600000");
    expect(csvCell(-4)).toBe("-4");
    expect(csvCell(Number.NaN)).toBe("");
    expect(csvCell(undefined)).toBe("");
    expect(csvCell(null)).toBe("");
  });
});

test("a table is its header and rows joined by CRLF", () => {
  expect(toCsv(["a", "b"], [["x", 1]])).toBe('"a","b"\r\n"x",1');
});
