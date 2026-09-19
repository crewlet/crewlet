/**
 * How a turn's own rows are WORDED and SHAPED, as distinct from which band
 * they fall in.
 *
 * Each case is a way a correct list still told the reader something untrue.
 */

import { describe, expect, test } from "vitest";
import { lead, withoutActor } from "~/routes/activity/Turn.tsx";

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

describe("a heading over a turn", () => {
  test("takes the lead sentence of a summary that is a paragraph", () => {
    // The turn this was measured on. A reviewer asked to account for a turn's
    // calls writes one sentence per call, and all four landed in an `<h1>`.
    const summary =
      "Founder posted a test message in Mattermost. Since the task was " +
      "delivered directly, I need to reply in the thread rather than staying " +
      "silent. Activating mattermost_post_message to reply to founder's test " +
      "message Replying to founder's test message to confirm Mattermost " +
      "integration works";
    expect(lead(summary)).toBe("Founder posted a test message in Mattermost.");
  });

  test("leaves a summary that is already one sentence, stop and all", () => {
    expect(lead("Replied to the founder in the thread.")).toBe(
      "Replied to the founder in the thread.",
    );
  });

  test("leaves a summary with no sentence boundary at all", () => {
    // Cutting mid-clause reads as a broken string; the two-line clamp on
    // `.object-title` is what bounds this one.
    expect(lead("replied to the founder in the thread")).toBe(
      "replied to the founder in the thread",
    );
  });

  test("keeps a version, a duration and a tool name whole", () => {
    // Why the boundary is a stop FOLLOWED BY A SPACE. A bare `.` cut every
    // one of these, and the third is the commonest thing a turn summary says.
    expect(lead("Bumped the client to v1.2 and re-ran the suite")).toBe(
      "Bumped the client to v1.2 and re-ran the suite",
    );
    expect(lead("Took 1m 52.4s to answer")).toBe("Took 1m 52.4s to answer");
    expect(lead("Called mattermost_post_message once")).toBe("Called mattermost_post_message once");
  });

  test("ends at a question or an exclamation too", () => {
    expect(lead("Did the founder mean staging? I asked in the thread.")).toBe(
      "Did the founder mean staging?",
    );
  });

  test("is a no-op on nothing, so the caller's own fallback still runs", () => {
    expect(lead("")).toBe("");
  });
});
