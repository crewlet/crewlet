// @vitest-environment node
/**
 * The warnings a person reads, as lines: what the banner over the chart and a
 * save's review both draw.
 *
 * What these protect: every warning of the check is in the list, wherever it
 * was placed, in the engine's order; warnings that say the same words with the
 * same link are one line, saying who they are about (the seat's name for one,
 * how many agents or seats for several, nobody where a warning names no seat);
 * and a line is drawn from the answer's own derivation, never the draft.
 */

import { describe, expect, test } from "vitest";
import type { ConfigWarning, Derived } from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./document.ts";
import { placeProblems } from "./problems.ts";
import { fixtureCompany } from "./testkit.ts";
import { warningLines } from "./warnings.ts";

const LEARNING = "Reflection and summaries run on the same model as the agent's own work.";

const advisory = (
  segments: (string | number)[],
  seat: string,
  person: string | undefined,
  message = `advisory at ${JSON.stringify(segments)}`,
): ConfigWarning => ({
  kind: "advisory",
  ref: "",
  path: "",
  segments,
  seat,
  unit: "",
  from: "",
  to: "",
  message,
  ...(person === undefined ? {} : { person }),
});

const derived = (seats: { handle: string; name: string; kind: string }[]): Derived =>
  ({ seats, units: [] }) as unknown as Derived;

function lines(warnings: ConfigWarning[], answer: Derived | null) {
  const sent = toDocument(fromDocument(fixtureCompany()));
  return warningLines(placeProblems(sent, { warnings, derived: answer }).all, answer);
}

describe("warningLines", () => {
  /*
   * SEVEN AGENTS LEARNING ON THEIR OWN MODEL ARE ONE THING TO KNOW: the
   * engine raises it once per seat in the same words for a person, and the
   * list says it once, with how many agents and where it is changed. The one
   * about no node stays a line of its own, first, as the engine said it.
   * Mutation: one line per warning, and the count goes from two to four.
   */
  test("one class raised for several agents is one line naming how many", () => {
    const got = lines(
      [
        advisory(["providers", "sandbox", "default_run_in"], "", "Agents run on the server."),
        advisory(["roles", 0, "llm_auxiliary"], "ceo", LEARNING, "ceo's reflection"),
        advisory(["units", 0, "roles", 0, "llm_auxiliary"], "vp-engineering", LEARNING, "vp's"),
        advisory(["units", 0, "roles", 1, "llm_auxiliary"], "dev", LEARNING, "dev's"),
      ],
      derived([
        { handle: "ceo", name: "CEO", kind: "agent" },
        { handle: "vp-engineering", name: "VP Engineering", kind: "agent" },
        { handle: "dev", name: "Dev", kind: "agent" },
      ]),
    );
    expect(got).toEqual([
      { who: null, words: "Agents run on the server.", link: "models" },
      { who: "3 agents", words: LEARNING, link: "models" },
    ]);
  });

  test("one seat is named, several of mixed kinds are counted as seats", () => {
    const answer = derived([
      { handle: "ceo", name: "CEO", kind: "agent" },
      { handle: "designer", name: "Designer", kind: "human" },
    ]);
    expect(lines([advisory(["roles", 0, "llm_auxiliary"], "ceo", LEARNING)], answer)).toEqual([
      { who: "CEO", words: LEARNING, link: "models" },
    ]);
    expect(
      lines(
        [
          advisory(["roles", 0, "contact"], "ceo", "No way to reach them."),
          advisory(["roles", 1, "contact"], "designer", "No way to reach them."),
        ],
        answer,
      ).map((l) => l.who),
    ).toEqual(["2 seats"]);
  });

  /*
   * A WARNING WITH NO WORDS FOR A PERSON IS LISTED IN THE ENGINE'S OWN, and
   * two that differ are two lines: nothing is merged on a guess.
   */
  test("warnings with different words are different lines, in the engine's order", () => {
    const got = lines(
      [
        advisory(["roles", 0, "goal"], "ceo", undefined, "first"),
        advisory(["roles", 1, "goal"], "designer", undefined, "second"),
      ],
      null,
    );
    expect(got.map((l) => [l.who, l.words])).toEqual([
      ["ceo", "first"],
      ["designer", "second"],
    ]);
  });
});
