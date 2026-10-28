/**
 * ONE POLL FOR THE DURABLE CODING-RUN RECORD.
 *
 * The runs board polled `sandbox_runs` every 20 s and the attention queue
 * every 30 s, so a run that parked on a question reached the board and then,
 * up to ten seconds later, the Inbox — two screens disagreeing about whether
 * anybody was being waited on. Every reader now names `RUNS_POLL_MS`, and this
 * reads the tree to hold it: a call site with a literal of its own, or with no
 * poll at all, is the drift coming back.
 *
 * Mutation: put `pollMs: 30_000` back in `lib/useAttention.ts`, and this names
 * the line.
 */

import { expect, test } from "vitest";

import { lineOf, modules, parse, stringValue, walk, type Node } from "../test/source.ts";
import { RUNS_POLL_MS, answerFate, type GivenAnswer } from "./runs.ts";

test("every reader of sandbox_runs polls on RUNS_POLL_MS", () => {
  const off: string[] = [];
  let sites = 0;
  for (const mod of modules()) {
    if (mod.lang === "dts") continue;
    const line = lineOf(mod.text);
    walk(parse(mod.text, mod.lang), (n) => {
      if (n.type !== "CallExpression") return;
      const callee = n.callee as Node;
      if (callee.type !== "Identifier" || callee.name !== "useQuery") return;
      const [what, , options] = n.arguments as Node[];
      if (stringValue(what) !== "sandbox_runs") return;
      sites++;
      const poll =
        options?.type === "ObjectExpression"
          ? (options.properties as Node[]).find(
              (p) =>
                p.type === "Property" &&
                (p.key as Node).type === "Identifier" &&
                (p.key as Node).name === "pollMs",
            )
          : undefined;
      const value = poll?.value as Node | undefined;
      if (value?.type !== "Identifier" || value.name !== "RUNS_POLL_MS") {
        off.push(`${mod.path}:${line(n.start)}`);
      }
    });
  }
  expect(off, "these read sandbox_runs on a poll of their own").toEqual([]);
  // NOT VACUOUS: the board, a run's page and rail, and the attention queue.
  expect(sites).toBeGreaterThanOrEqual(4);
  expect(RUNS_POLL_MS).toBe(20_000);
});

/**
 * WHAT BECAME OF AN ANSWER IS READ OFF THE RUN'S RECORD, never assumed from
 * the press. `answer_run` answers `pending`, and the Inbox used to show that
 * as "Answered" and drop the question: a person whose answer never reached the
 * run was told it had, and the question kept coming back.
 *
 * Mutation: answer `taken` for a run still waiting, and the first case fails;
 * ignore `declined_answers`, and the person is never told to answer again.
 */
test("an answer's fate is the run record's, not the press's", () => {
  const given: GivenAnswer = { text: "use main", launchId: "q1", answerId: "a1" };
  const run = (over: Record<string, unknown>) => ({
    status: "awaiting_clarification" as const,
    launch_id: "q1",
    declined_answers: [] as string[],
    ...over,
  });
  expect(answerFate(given, run({}), true)).toBe("sent");
  expect(answerFate(given, run({ status: "reseed" }), true)).toBe("sent");
  expect(answerFate(given, run({ status: "answered", answer_ids: ["a1"] }), true)).toBe("recorded");
  expect(answerFate(given, run({ status: "answered" }), true)).toBe("recorded");
  // THE FIRST RECORDED ANSWER WINS: another one on the same question was used.
  expect(answerFate(given, run({ status: "answered", answer_ids: ["a9"] }), true)).toBe(
    "superseded",
  );
  expect(answerFate(given, run({ status: "resumed" }), true)).toBe("taken");
  expect(answerFate(given, run({ status: "running" }), true)).toBe("taken");
  expect(answerFate(given, undefined, true)).toBe("taken");
  expect(answerFate(given, undefined, false)).toBe("sent");
  expect(answerFate(given, run({ declined_answers: ["a1"] }), true)).toBe("declined");
  expect(answerFate(given, run({ launch_id: "q2" }), true)).toBe("overtaken");
  // ANOTHER PERSON'S ANSWER LET GO is not this one's.
  expect(answerFate(given, run({ declined_answers: ["a0"] }), true)).toBe("sent");
});
