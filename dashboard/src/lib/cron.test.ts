import { describe as group, expect, it } from "vitest";
import { describe, nextFires, parseCron } from "./cron.ts";

function fires(expression: string, from: string, count = 3, zone = "UTC"): string[] {
  return nextFires(expression, new Date(from), zone, count).map((d) => d.toISOString());
}

group("reading an expression as a sentence", () => {
  it("says the shapes a company's schedules are actually written in", () => {
    expect(describe("0 9 * * 1-5")).toBe("at 09:00 on weekdays");
    expect(describe("30 8 * * *")).toBe("at 08:30 every day");
    expect(describe("*/15 * * * *")).toBe("every 15 minutes every day");
    expect(describe("0 */4 * * *")).toBe("every 4 hours at :00 past every day");
    expect(describe("0 0 1 * *")).toBe("at 00:00 on the 1st");
    expect(describe("0 0 * * 0,6")).toBe("at 00:00 on weekends");
    expect(describe("0 12 * 1 *")).toBe("at 12:00 every day in January");
  });

  it("names both restrictions when both are set, because cron ORs them", () => {
    // THE RULE PEOPLE GET WRONG: `0 0 1 * 1` fires on the first of the month
    // AND on every Monday, not on a Monday that is the first.
    expect(describe("0 0 1 * 1")).toBe("at 00:00 on the 1st and on Monday");
  });

  it("reads the names cron accepts for days and months", () => {
    expect(describe("0 9 * * MON-FRI")).toBe("at 09:00 on weekdays");
    expect(describe("0 9 * JAN *")).toBe("at 09:00 every day in January");
  });

  it("reads 7 as Sunday rather than matching nothing", () => {
    // A schedule written `0 9 * * 7` that silently matched nothing would be a
    // schedule that never fires with nothing on the screen to say why.
    expect(describe("0 9 * * 7")).toBe("at 09:00 on Sunday");
    expect(fires("0 9 * * 7", "2026-06-15T00:00:00Z", 1)).toEqual(["2026-06-21T09:00:00.000Z"]);
  });

  it("says it cannot read an expression rather than guessing one", () => {
    // The ENGINE runs the schedule. A reading aid that invented a sentence
    // would put words on the screen the engine does not act on.
    // ONE PER FIELD, so a check dropped from any of the five is caught:
    // wrong arity, an unreadable word, and a value out of range in the
    // minute, the day-of-month, the month and the weekday in turn.
    for (const bad of [
      "",
      "0 9 * *",
      "0 9 * * * *",
      "nonsense",
      "99 9 * * *",
      "0 99 * * *",
      "0 9 0 * *",
      "0 9 * 13 *",
      "0 9 * * 9",
      "5-1 9 * * *",
      "*/0 * * * *",
    ]) {
      expect(describe(bad), bad).toBeNull();
      expect(parseCron(bad), bad).toBeNull();
    }
  });
});

group("the instants it fires at", () => {
  it("starts at the next whole minute, never the current one", () => {
    // A fire at the current minute has either happened or is happening, and
    // neither is "next".
    expect(fires("* * * * *", "2026-06-15T09:30:20Z", 2)).toEqual([
      "2026-06-15T09:31:00.000Z",
      "2026-06-15T09:32:00.000Z",
    ]);
  });

  it("walks a weekday schedule across a weekend", () => {
    // Friday 10:00 → the next three are Monday, Tuesday, Wednesday.
    expect(fires("0 9 * * 1-5", "2026-06-19T10:00:00Z")).toEqual([
      "2026-06-22T09:00:00.000Z",
      "2026-06-23T09:00:00.000Z",
      "2026-06-24T09:00:00.000Z",
    ]);
  });

  it("ORs the day and weekday fields when both are restricted", () => {
    // June 2026: the 1st is a Monday. `1 * 1` fires on the 1st and on every
    // Monday — so the 1st, the 8th, the 15th, not only whichever is both.
    expect(fires("0 0 1 * 1", "2026-06-01T12:00:00Z")).toEqual([
      "2026-06-08T00:00:00.000Z",
      "2026-06-15T00:00:00.000Z",
      "2026-06-22T00:00:00.000Z",
    ]);
    // And with only one restricted it is an AND — a plain monthly schedule.
    expect(fires("0 0 1 * *", "2026-06-02T00:00:00Z", 2)).toEqual([
      "2026-07-01T00:00:00.000Z",
      "2026-08-01T00:00:00.000Z",
    ]);
  });

  it("crosses a month and a year boundary", () => {
    expect(fires("0 0 31 12 *", "2026-01-01T00:00:00Z", 2)).toEqual([
      "2026-12-31T00:00:00.000Z",
      "2027-12-31T00:00:00.000Z",
    ]);
  });

  it("returns what it found rather than throwing on an expression that never fires", () => {
    // 31 February. A caller that got fewer than it asked for says so; the
    // alternative is a screen that throws on a schedule an operator typed.
    expect(fires("0 0 31 2 *", "2026-01-01T00:00:00Z", 3)).toEqual([]);
  });

  it("answers nothing at all for an expression it cannot read", () => {
    expect(fires("nonsense", "2026-06-15T00:00:00Z")).toEqual([]);
    expect(nextFires("0 9 * * *", new Date("2026-06-15T00:00:00Z"), "UTC", 0)).toEqual([]);
  });

  it("reads a list and a step in one field", () => {
    expect(fires("0,30 9 * * *", "2026-06-15T00:00:00Z", 3)).toEqual([
      "2026-06-15T09:00:00.000Z",
      "2026-06-15T09:30:00.000Z",
      "2026-06-16T09:00:00.000Z",
    ]);
    // A bare value WITH a step is a range to the end of the field.
    expect(fires("5/20 0 * * *", "2026-06-15T00:00:00Z", 3)).toEqual([
      "2026-06-15T00:05:00.000Z",
      "2026-06-15T00:25:00.000Z",
      "2026-06-15T00:45:00.000Z",
    ]);
  });
});

group("the instants, in the schedule's own zone", () => {
  // THE STANDING OFFSET, not a twice-a-year drift. The engine evaluates the
  // expression in the row's timezone, so a UTC reading of `0 9 * * 1-5` in
  // Tokyo listed 09:00Z against the engine's 00:00Z — nine hours late on
  // every row of every company that does not run in UTC, for ever.
  it("matches the LOCAL projection, so a zone's standing offset is not lost", () => {
    expect(fires("0 9 * * 1-5", "2026-06-14T00:00:00Z", 3, "Asia/Tokyo")).toEqual([
      "2026-06-15T00:00:00.000Z",
      "2026-06-16T00:00:00.000Z",
      "2026-06-17T00:00:00.000Z",
    ]);
    // The same expression in UTC is the answer this used to give for both.
    expect(fires("0 9 * * 1-5", "2026-06-14T00:00:00Z", 3)).toEqual([
      "2026-06-15T09:00:00.000Z",
      "2026-06-16T09:00:00.000Z",
      "2026-06-17T09:00:00.000Z",
    ]);
  });

  // NOT EVERY ZONE IS A WHOLE HOUR AHEAD, which is why nothing here skips by
  // one: Kathmandu is +05:45 and an hour-granular walk lands on :00.
  it("reads a zone whose offset is not a whole number of hours", () => {
    expect(fires("0 9 * * *", "2026-06-14T12:00:00Z", 2, "Asia/Kathmandu")).toEqual([
      "2026-06-15T03:15:00.000Z",
      "2026-06-16T03:15:00.000Z",
    ]);
  });

  // A VANISHED LOCAL HOUR YIELDS NO FIRE. Berlin's clocks go 02:00 → 03:00 on
  // 29 March 2026, so a daily 02:00 schedule simply does not run that day —
  // which is what walking UTC and projecting gives for free, and what
  // constructing local times would have had to invent an answer for.
  it("skips a daily fire that the spring change deletes", () => {
    expect(fires("0 2 * * *", "2026-03-27T12:00:00Z", 3, "Europe/Berlin")).toEqual([
      "2026-03-28T01:00:00.000Z",
      "2026-03-30T00:00:00.000Z",
      "2026-03-31T00:00:00.000Z",
    ]);
  });

  // AND A REPEATED ONE YIELDS TWO, for the same reason and with the same
  // authority: `Expr.Next` says every UTC instant has exactly one local
  // reading, so the hour Berlin lives through twice on 25 October 2026 is two
  // instants that both match.
  it("fires twice in the hour the autumn change repeats", () => {
    expect(fires("0 2 * * *", "2026-10-24T12:00:00Z", 3, "Europe/Berlin")).toEqual([
      "2026-10-25T00:00:00.000Z",
      "2026-10-25T01:00:00.000Z",
      "2026-10-26T01:00:00.000Z",
    ]);
  });

  // A ZONE THIS RUNTIME CANNOT READ IS NOT UTC — the same refusal the file
  // makes for an expression it cannot parse, rather than a list of instants
  // the engine does not fire at.
  it("answers nothing at all for a zone it cannot read", () => {
    expect(fires("0 9 * * *", "2026-06-15T00:00:00Z", 3, "Mars/Olympus_Mons")).toEqual([]);
  });
});
