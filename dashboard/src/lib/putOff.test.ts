/**
 * A notice put off stays off for the time chosen and no longer, and comes back
 * the moment its condition worsens: a new run of it, or more nodes.
 */
import { expect, test } from "vitest";
import { PUT_OFF_CHOICES, putOffHolds, splitPutOff, type PutOffMark } from "./putOff.ts";

const NOW = Date.parse("2026-11-14T12:00:00Z");
const DAY = 24 * 3_600_000;
const notice = { id: "alarm-census_drift", at: "2026-11-13T06:15:15Z", spread: 1 };
const mark = (over: Partial<PutOffMark> = {}): PutOffMark => ({
  until: new Date(NOW + DAY).toISOString(),
  since: notice.at,
  spread: 1,
  ...over,
});

test("a mark holds until its time, for the same condition on as many nodes", () => {
  expect(putOffHolds(mark(), notice, NOW)).toBe(true);
  expect(putOffHolds(mark(), notice, NOW + DAY)).toBe(false);
});

test("a condition that cleared and came back is a new one, and is not put off", () => {
  expect(putOffHolds(mark(), { ...notice, at: "2026-11-14T11:00:00Z" }, NOW)).toBe(false);
});

test("a condition standing on more nodes than when it was put off comes back", () => {
  expect(putOffHolds(mark(), { ...notice, spread: 2 }, NOW)).toBe(false);
  expect(putOffHolds(mark({ spread: 2 }), { ...notice, spread: 1 }, NOW)).toBe(true);
});

test("never for good: a mark past the longest choice, written by hand, does not hold", () => {
  const longest = Math.max(...PUT_OFF_CHOICES.map((c) => c.hours)) * 3_600_000;
  expect(
    putOffHolds(mark({ until: new Date(NOW + longest + DAY).toISOString() }), notice, NOW),
  ).toBe(false);
});

test("only a platform notice can be put off: a row that names no spread is always shown", () => {
  const seat = { id: "seat-state-1", at: notice.at };
  expect(putOffHolds(mark(), seat, NOW)).toBe(false);
  expect(splitPutOff([seat, notice], { [seat.id]: mark(), [notice.id]: mark() }, NOW)).toEqual({
    shown: [seat],
    putOff: [notice],
  });
});
