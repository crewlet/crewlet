/**
 * The sources the Advisor names as unread, each said in words. A word the
 * engine sends that this build cannot name reads as its raw key ("flow"), so
 * the engine's words are read off its source and every one must have its
 * sentence and its name here.
 */

import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { UNREAD_KNOWN, unreadNames, unreadSentences, unreadTitle } from "./advisorUnread.ts";

/** The repository root: the tests run from the dashboard's own folder. */
const REPO = join(process.cwd(), "..");

/** The unread words the engine's Go source declares (`UnreadX = "word"`). */
function engineWords(): string[] {
  const files = [
    ...readdirSync(join(REPO, "internal/advisor"))
      .filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"))
      .map((f) => join(REPO, "internal/advisor", f)),
    join(REPO, "internal/api/queries/advisorrun.go"),
  ];
  const words = new Set<string>();
  for (const file of files) {
    for (const [, word] of readFileSync(file, "utf8").matchAll(
      /\bUnread[A-Z]\w*\s*=\s*"([a-z_]+)"/g,
    )) {
      words.add(word!);
    }
  }
  return [...words].sort();
}

test("every unread word the engine sends is said in words, never as its key", () => {
  const words = engineWords();
  // THE SCAN FINDS THE ENGINE'S WORDS, so an empty pass is no pass.
  expect(words).toContain("alarm_episodes");
  expect(words.filter((w) => !UNREAD_KNOWN.includes(w))).toEqual([]);
  for (const word of words) {
    // A NAME OF ITS OWN, never the key, and a sentence of its own, never the
    // fallback that says the key.
    expect(unreadNames([word])).not.toBe(word);
    expect(unreadSentences([word])[0]).not.toBe(
      `The advisor couldn't read ${word}, so what it would raise is missing.`,
    );
  }
});

test("the sources added with the alarm history, the flow record and the skipped runs are named", () => {
  expect(unreadNames(["alarm_episodes", "flow", "schedule_skips"])).toBe(
    "the engine's alarm history, how long work items stood in each state and the schedules' skipped runs",
  );
  expect(unreadNames(["fires"])).toBe("the week's schedule fires");
  expect(unreadTitle(["schedule_skips"])).toBe(
    "The advisor couldn't read the schedules' skipped runs",
  );
});
