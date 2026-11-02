/**
 * What a failed charter save says.
 *
 * The invariant: "the engine did not answer" only where the engine's answer
 * never arrived. A 5xx the engine wrote is an engine that failed, said with
 * its own words, whether the save is reported as it came back or after the
 * read that settled it.
 */

import { expect, test } from "vitest";
import { classifyConfigRefusal, type ConfigUnsettled } from "~/protocol/configAnswer.ts";
import { failureWords } from "./words.ts";

/** A proxy's refusal in the engine's own shape, without its mark, as the one classifier reads it. */
function proxied(status: number): ConfigUnsettled {
  const read = classifyConfigRefusal({
    status,
    body: { error: "forbidden", detail: "blocked by policy" },
    fromEngine: false,
  });
  if (read.kind !== "unsettled") throw new Error(`a proxy's ${status} read as ${read.kind}`);
  return read;
}

const CAUSES: readonly [string, ConfigUnsettled, string][] = [
  ["a proxy answered 403", proxied(403), "The engine did not answer."],
  ["a proxy answered 413", proxied(413), "The engine did not answer."],
  [
    "nothing answered",
    { kind: "unsettled", answered: false, status: 0, detail: "Failed to fetch" },
    "The engine did not answer.",
  ],
  [
    "a gateway answered",
    {
      kind: "unsettled",
      answered: false,
      status: 504,
      detail: "a 504 came back without the engine's JSON",
    },
    "The engine did not answer.",
  ],
  [
    "the engine failed",
    { kind: "unsettled", answered: true, status: 500, detail: "store: disk full" },
    "The engine failed: store: disk full.",
  ],
];

test.each(CAUSES)("a save left unsettled where %s says so", (_, refusal, said) => {
  expect(failureWords({ kind: "refused", refusal }, "mission")).toBe(
    `${said} Whether the mission changed is not known. Save again: the page checks what the company holds before it writes.`,
  );
});

test.each(CAUSES)("a save settled as lost where %s says so", (_, refusal, said) => {
  expect(failureWords({ kind: "lost", refusal }, "mission")).toBe(
    `${said} Read back, the mission still reads as it did, so nothing was saved. Save again.`,
  );
});
