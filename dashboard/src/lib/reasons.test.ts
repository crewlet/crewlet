// @vitest-environment node

/**
 * The wake reasons, as English.
 *
 * The COMPLETENESS half of this is a Go test — `TestEveryWakeReasonReadsAs
 * EnglishOnTheClient` reads this same table from source and walks the engine's
 * twenty — because the authority on which reasons exist is the engine, and a
 * list restated here would only ever agree with itself. What is left is the
 * behaviour a table alone cannot state: what happens to a reason this build
 * has never heard of.
 */

import { describe, expect, test } from "vitest";
import { reasonPhrase, reasonWhy } from "./reasons.ts";

describe("reason phrasing", () => {
  test("a known reason reads as a sentence about a person", () => {
    expect(reasonPhrase("lead_fallback")).toBe("routed to you as the lead");
    expect(reasonWhy("unblocked")).toBe("what this was waiting on is done");
  });

  // A ROLLING UPGRADE PUTS A NEWER NODE'S VALUES IN FRONT OF AN OLDER
  // SCREEN, which is the same rule the event registry states for unknown
  // types: an unknown value renders as ITSELF rather than vanishing. A row
  // that dropped its reason would claim the engine recorded none — and
  // recording one is the thing this surface exists to show.
  test("a reason this build does not know renders as itself", () => {
    expect(reasonPhrase("summoned_by_owl")).toBe("summoned by owl");
    expect(reasonWhy("summoned_by_owl")).toContain("summoned_by_owl");
  });

  test("an empty reason does not become an empty chip", () => {
    // The engine records one on every notice, so this is the case where
    // something upstream lost it — and a blank chip is indistinguishable
    // from a chip that was never drawn, which hides the one fact this
    // surface exists to show.
    expect(reasonPhrase("")).toBe("no reason recorded");
    expect(reasonWhy("")).toContain("carries no reason");
  });
});
