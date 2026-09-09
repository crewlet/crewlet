/**
 * How a turn's own rows are WORDED and SHAPED, as distinct from which band
 * they fall in.
 *
 * Each case is a way a correct list still told the reader something untrue.
 */

import { describe, expect, test } from "vitest";
import { withoutActor } from "~/routes/Turn.tsx";

describe("a row on a page about one seat", () => {
  test("drops the seat's own name from the front of its own line", () => {
    // The engine builds these as `lead(actor, …)`, so every row under one
    // seat's heading opened with that seat's name — four times, in the column
    // the sentence itself needed.
    expect(withoutActor("Agent CEO wrote episode (done)", "Agent CEO")).toBe(
      "wrote episode (done)",
    );
  });

  test("leaves a name that is not at the front", () => {
    // The counterparty profiler names its SUBJECT mid-sentence. That is not
    // the seat talking about itself, and cutting it would change the fact.
    expect(withoutActor("PM updated 1 trait(s) on Agent CEO", "Agent CEO")).toBe(
      "PM updated 1 trait(s) on Agent CEO",
    );
  });

  test("leaves a summary that is nothing but the name", () => {
    // Emptying the line would render a row with no text at all, which reads
    // as a broken row rather than as a terse one.
    expect(withoutActor("Agent CEO", "Agent CEO")).toBe("Agent CEO");
  });

  test("is a no-op with no actor to strip", () => {
    expect(withoutActor("something happened", "")).toBe("something happened");
  });
});
