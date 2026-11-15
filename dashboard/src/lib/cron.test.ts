import { describe as group, expect, it } from "vitest";
import { cronProblem, describe, nextFires, parseCron } from "./cron.ts";

function fires(expression: string, from: string, count = 3, zone = "UTC"): string[] {
  return nextFires(expression, new Date(from), zone, count).map((d) => d.toISOString());
}

group("reading an expression as a sentence", () => {
  it("says the shapes a company's schedules are actually written in", () => {
    expect(describe("0 9 * * 1-5")).toBe("at 09:00 on weekdays");
    expect(describe("30 8 * * *")).toBe("at 08:30 every day");
    expect(describe("*/15 * * * *")).toBe("every 15 minutes");
    expect(describe("0 */4 * * *")).toBe("every 4 hours at :00 past");
    expect(describe("0 0 1 * *")).toBe("at 00:00 on the 1st");
    expect(describe("0 0 * * 0,6")).toBe("at 00:00 on weekends");
    expect(describe("0 12 * 1 *")).toBe("at 12:00 every day in January");
  });

  // A TIME THAT RECURS THROUGH THE DAY SAYS "EVERY DAY" ALREADY. Appended, it
  // read "every 4 hours at :00 past every day"; a day it does skip is still
  // said, and so is a time that is not a recurrence.
  it("says a day only where the time does not already cover every one", () => {
    expect(describe("* * * * *")).toBe("every minute");
    expect(describe("5 * * * *")).toBe(":05 past every hour");
    expect(describe("*/20 * * * 1-5")).toBe("every 20 minutes on weekdays");
    expect(describe("0 */6 * 1 *")).toBe("every 6 hours at :00 past in January");
    expect(describe("* 9 * * *")).toBe("every minute of 09:00 every day");
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

// A REFUSAL NAMES THE FIELD THAT BROKE IT. Every mistake used to read as "this
// is not a cron expression: five fields", including an expression with five
// fields and one minute out of range, which left the person to find which of
// the five was wrong.
group("saying what is wrong with an expression", () => {
  it("names the field out of range, with the values it takes", () => {
    expect(cronProblem("61 25 * * *")).toBe("The minute must be 0 to 59.");
    expect(cronProblem("0 25 * * *")).toBe("The hour must be 0 to 23.");
    expect(cronProblem("0 9 0 * *")).toBe("The day of the month must be 1 to 31.");
    expect(cronProblem("0 9 * 13 *")).toBe("The month must be 1 to 12, or JAN to DEC.");
    expect(cronProblem("0 9 * * 8")).toBe("The weekday must be 0 to 7, or SUN to SAT.");
    expect(cronProblem("0 9 * * MON-FRY")).toBe("The weekday must be 0 to 7, or SUN to SAT.");
  });

  it("says how many fields there are when that is what is wrong", () => {
    expect(cronProblem("0 9 * *")).toBe(
      "A cron expression has five fields: minute, hour, day of the month, month and weekday. This one has 4.",
    );
  });

  it("names a step, an empty entry and a range that runs backwards", () => {
    expect(cronProblem("*/0 * * * *")).toBe(
      "The minute’s step, after the “/”, must be a whole number above 0.",
    );
    expect(cronProblem("0 9,,10 * * *")).toBe("The hour has an empty entry between its commas.");
    expect(cronProblem("0 22-2 * * *")).toBe(
      "The hour range 22-2 runs backwards. A range does not wrap, so write it as two ranges.",
    );
  });

  it("answers nothing for an expression the engine reads", () => {
    expect(cronProblem("30 9 * * 1-5")).toBeNull();
  });
});

// THE ENGINE'S GRAMMAR, NOT A SECOND ONE. The copy beside the form had drifted
// from `internal/cron`: it refused the weekend the engine documents and read a
// month's name in the weekday field.
group("reading the grammar the engine reads", () => {
  it("reads 6-7 as the weekend, 7 being Sunday", () => {
    expect(cronProblem("0 9 * * 6-7")).toBeNull();
    expect(describe("0 9 * * 6-7")).toBe("at 09:00 on weekends");
  });

  it("reads a name only in the field it names", () => {
    expect(cronProblem("0 9 * * JAN")).toBe("The weekday must be 0 to 7, or SUN to SAT.");
    expect(cronProblem("0 9 * MON *")).toBe("The month must be 1 to 12, or JAN to DEC.");
    expect(cronProblem("MON 9 * * *")).toBe("The minute must be 0 to 59.");
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
