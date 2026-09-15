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
import { reasonAbout, reasonPhrase, reasonWhy } from "./reasons.ts";

/** The engine's twenty, as this table spells them. The Go gate
 *  `TestEveryWakeReasonReadsAsEnglishOnTheClient` is what keeps this in step
 *  with the engine; here it is the set the two-voice rule is walked over. */
const KNOWN = [
  "mention",
  "prioritised",
  "blocking",
  "asked",
  "answered",
  "assignee",
  "unassigned",
  "reporter",
  "thread",
  "unblocked",
  "routed_to",
  "parent_assignee",
  "checklist",
  "collaborator",
  "goal_owner",
  "sprint",
  "watcher",
  "unwatched",
  "purged",
  "lead_fallback",
];

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

  // TWO VOICES, and neither is the other's fallback. A change's routing lists
  // COLLEAGUES, so "assigned to you" beside somebody else's name is a sentence
  // about the wrong person — which is what the Woke tab rendered until this
  // existed. Every reason has to carry both, or the surface that lacks one
  // silently borrows the other's pronoun.
  test("a reason about somebody else is not in the second person", () => {
    expect(reasonAbout("assignee")).toBe("assignee");
    expect(reasonAbout("lead_fallback")).toBe("the unit's lead");
    expect(reasonAbout("watcher")).toBe("watching");
  });

  // AND NO THIRD-PERSON FORM SAYS "YOU". This is the rule rather than a
  // spot-check on three of them: the failure is one entry keeping the inbox's
  // wording, which reads perfectly on its own line and is a sentence about the
  // wrong person on every routing list in the product.
  test("no third-person form is in the second person", () => {
    for (const reason of KNOWN) {
      expect(reasonAbout(reason).toLowerCase(), `${reason} reads as second person`).not.toMatch(
        /\byou\b|\byour\b|\byours\b/,
      );
      // And the second-person form is still second-person, so the two
      // have not quietly become one table with one voice.
      expect(reasonPhrase(reason), `${reason} has no phrase`).toBeTruthy();
    }
  });

  test("an unknown reason has a third-person form too", () => {
    expect(reasonAbout("summoned_by_owl")).toBe("summoned by owl");
    expect(reasonAbout("")).toBe("no reason recorded");
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
