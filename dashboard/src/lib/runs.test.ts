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
import { RUNS_POLL_MS } from "./runs.ts";

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
