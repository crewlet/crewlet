// @vitest-environment node
/**
 * The screen's phase ordering is the engine's, recomputed from both sources.
 *
 * `PHASE_ORDER` in Integrations.tsx exists because a TypeScript screen cannot
 * import a Go slice, and it decides which of a tool's surfaces is reported:
 * Atlassian shows the least ready of Jira, Confluence and its Forge relay. If
 * the two orders drift, the screen names the wrong surface and reports the
 * wrong state for the tool, silently and on every render. The comment above
 * the constant asks a reader to keep them in step, which is a request rather
 * than a guarantee.
 *
 * BOTH SIDES ARE READ AS TEXT, and the TypeScript side deliberately is not
 * imported. This file runs in the node environment, and importing the screen
 * would pull a React module graph into it — mixing environments inside a
 * directory of jsdom tests, which is a cost with nothing to buy: what is
 * under test is whether two literal lists agree, and a literal list is
 * something you can read. It also keeps PHASE_ORDER unexported, so the
 * constant has no caller that exists only for a test.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";

/** The `Phases` slice, in the order internal/integration/report.go writes it. */
function enginePhases(): string[] {
  const src = readFileSync(join(process.cwd(), "../internal/integration/report.go"), "utf8");
  const slice = /var Phases = \[\]Phase\{([\s\S]*?)\}/.exec(src);
  if (!slice) throw new Error("internal/integration/report.go has no `var Phases` slice to read");

  // The slice names constants (PhaseUnconfigured, ...) and their wire values
  // are declared separately, so both halves are read rather than guessed.
  const values = new Map<string, string>();
  for (const match of src.matchAll(/(Phase[A-Za-z]+)\s+Phase\s*=\s*"([a-z_]+)"/g)) {
    values.set(match[1]!, match[2]!);
  }
  return [...slice[1]!.matchAll(/Phase[A-Za-z]+/g)].map(([name]) => {
    const value = values.get(name);
    if (!value) throw new Error(`${name} is in Phases with no declared wire value`);
    return value;
  });
}

/** PHASE_ORDER, as Integrations.tsx declares it. */
function screenPhases(): string[] {
  const src = readFileSync(join(process.cwd(), "src/routes/Integrations.tsx"), "utf8");
  const list = /const PHASE_ORDER = \[([\s\S]*?)\];/.exec(src);
  if (!list) throw new Error("Integrations.tsx has no `PHASE_ORDER` list to read");
  return [...list[1]!.matchAll(/"([a-z_]+)"/g)].map((match) => match[1]!);
}

test("the screen orders phases exactly as the engine does", () => {
  expect(screenPhases()).toEqual(enginePhases());
});

test("the reader finds the phases it is meant to be comparing", () => {
  // Without this both tests pass on two empty lists, which is how a scanner
  // that stops matching reports everything as fine.
  const phases = enginePhases();
  expect(phases.length).toBeGreaterThanOrEqual(6);
  expect(phases[0]).toBe("unconfigured");
  expect(phases.at(-1)).toBe("ready");
  expect(screenPhases()).toEqual(phases);
});
