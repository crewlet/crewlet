/**
 * The typed cells, and the one thing they must never do.
 *
 * # They may not restate a format
 *
 * `lib/format.ts` owns how a number, a count and a duration are SPELLED.
 * These cells wrote their own: `DurationCell` rendered "500ms" where
 * `fmtDuration` writes "500 ms", and `TokenCell` abbreviated five thousand as
 * "5.0k" where `fmtCount` writes "5,000". That is worse than a drift — it is
 * a trap, because adopting a cell would silently change every figure in the
 * column, so the module built to make the product consistent could not be
 * adopted without making it inconsistent.
 *
 * These cases hold each cell against the formatter it composes, which is the
 * only way that stays true: a second spelling is a test failure rather than a
 * number somebody notices on a screen six months later.
 *
 * # Absent is not zero
 *
 * The other half of why a cell exists. "Nothing is estimated" and "everything
 * is estimated at nothing" are different facts, and a grid rendering both as
 * `0` makes the first invisible — while `value || "—"`, which is what three
 * columns in this product did, makes the SECOND invisible instead.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { DurationCell, NumberCell, TokenCell } from "./cells.tsx";
import { fmtCount, fmtDuration } from "~/lib/format.ts";

afterEach(cleanup);

test("a count is spelled the way fmtCount spells it", () => {
  for (const n of [0, 7, 999, 5_000, 12_345, 1_250_000]) {
    cleanup();
    render(<NumberCell value={n} />);
    expect(screen.getByText(fmtCount(n)), String(n)).toBeTruthy();
  }
});

test("tokens are spelled the way every other count is", () => {
  for (const n of [0, 900, 5_000, 250_000, 3_400_000]) {
    cleanup();
    render(<TokenCell value={n} />);
    expect(screen.getByText(fmtCount(n)), String(n)).toBeTruthy();
  }
});

test("a duration is spelled the way fmtDuration spells it", () => {
  for (const ms of [0, 500, 5_000, 95_000, 7_200_000]) {
    cleanup();
    render(<DurationCell ms={ms} />);
    expect(screen.getByText(fmtDuration(ms)), String(ms)).toBeTruthy();
  }
});

test("a zero renders as a zero, in every one of them", () => {
  // NOT a dash. `t.rounds || "—"` said "not measured" about a turn that
  // measurably ran no rounds.
  render(
    <>
      <NumberCell value={0} />
      <TokenCell value={0} />
    </>,
  );
  expect(screen.getAllByText("0").length).toBe(2);
  expect(screen.queryByText("—")).toBeNull();
});

test("an absent value renders a dash that says which absence it is", () => {
  const { container } = render(
    <>
      <NumberCell value={null} />
      <TokenCell value={undefined} />
      <DurationCell ms={null} />
    </>,
  );
  const dashes = container.querySelectorAll(".cell-dash");
  expect(dashes.length).toBe(3);
  expect([...dashes].map((d) => d.getAttribute("title"))).toEqual([
    "nothing recorded",
    "nothing recorded",
    "not measured",
  ]);
});
