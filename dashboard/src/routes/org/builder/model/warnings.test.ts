// @vitest-environment node
/**
 * The warnings the banner over the chart and a save's review both draw, merged
 * by kind: every warning of the check is in the list, in the engine's order;
 * one kind is one notice saying who it is about; seats are named from the
 * answer's own derivation, never the draft.
 */

import { describe, expect, test } from "vitest";
import type { ConfigWarning, Derived } from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./document.ts";
import { placeProblems } from "./problems.ts";
import { fixtureCompany } from "./testkit.ts";
import { groupNotices } from "@crewlethq/ui";
import { configWarningEntries, subjectCount } from "~/components/ConfigWarnings.tsx";
import { warningsOf } from "./warnings.ts";

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
  const draft = warningsOf(placeProblems(sent, { warnings, derived: answer }).all, answer);
  const notices = groupNotices(
    configWarningEntries(draft.warnings, draft.seat, (w) => draft.linkOf(w)),
    { count: subjectCount(draft.warnings, draft.seat) },
  );
  return notices.map((n) => ({
    who: n.subject === undefined ? null : String(n.subject),
    words: text(n.title),
    link: n.action ?? null,
  }));
}

const text = (node: unknown): string =>
  Array.isArray(node) ? node.map(text).join("") : typeof node === "string" ? node : "";

describe("the builder's warnings", () => {
  /*
   * SEVEN AGENTS LEARNING ON THEIR OWN MODEL ARE ONE THING TO KNOW: the
   * engine raises it once per seat in the same words for a person, and the
   * list says it once, with how many agents and where it is changed. The one
   * about no node stays a line of its own, first, as the engine said it.
   * Mutation: one line per warning, and the count goes from two to four.
   */
  test("one kind raised for several agents is one notice naming how many", () => {
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
  test("warnings with different words are different notices, in the engine's order", () => {
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

  /*
   * A WARNING RAISED ONCE PER SCHEDULE is one notice: the class's title, the
   * seat as its subject, each schedule an item, and the explanation once.
   */
  test("a seat's frequent schedules are one notice listing each schedule", () => {
    const schedule = (i: number, item: string): ConfigWarning => ({
      ...advisory(["roles", 0, "schedules", i, "cron"], "ceo", "Each run starts a turn."),
      class: "schedule.frequent",
      title: "Schedules run 12 or more times a day",
      item,
    });
    const sent = toDocument(fromDocument(fixtureCompany()));
    const answer = derived([{ handle: "ceo", name: "CEO", kind: "agent" }]);
    const draft = warningsOf(
      placeProblems(sent, {
        warnings: [schedule(0, "health-check (24 a day)"), schedule(1, "release (12 a day)")],
        derived: answer,
      }).all,
      answer,
    );
    const notices = groupNotices(configWarningEntries(draft.warnings, draft.seat), {
      count: subjectCount(draft.warnings, draft.seat),
    });
    expect(notices).toHaveLength(1);
    expect(notices[0]!.subject).toBe("CEO");
    expect(text(notices[0]!.title)).toBe("Schedules run 12 or more times a day");
    expect(text(notices[0]!.detail)).toBe("Each run starts a turn.");
    expect(notices[0]!.lines).toEqual([
      { items: ["health-check (24 a day)", "release (12 a day)"] },
    ]);
  });
});
