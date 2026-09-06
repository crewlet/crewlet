// @vitest-environment node
/**
 * The screen's phase ordering is the engine's, recomputed from the Go source.
 *
 * `PHASE_ORDER` in Integrations.tsx exists because a TypeScript screen cannot
 * import a Go slice, and it decides which of a tool's surfaces is reported:
 * Atlassian shows the least ready of Jira, Confluence and its Forge relay. If
 * the two orders drift, the screen names the wrong surface and reports the
 * wrong state for the tool, silently and on every render.
 *
 * The comment above the constant asks a reader to keep the two in step. That
 * is a request, not a guarantee, so the file that owns the order is read and
 * compared. Same idiom as palette.test.ts recomputing the stylesheet's own
 * claims: the duplication stays, and drifting fails a build.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { PHASE_ORDER } from "./Integrations.tsx";

/** The `Phases` slice, in the order internal/integration/report.go writes it. */
function enginePhases(): string[] {
  const src = readFileSync(join(process.cwd(), "../internal/integration/report.go"), "utf8");
  const slice = /var Phases = \[\]Phase\{([\s\S]*?)\}/.exec(src);
  if (!slice) throw new Error("internal/integration/report.go has no `var Phases` slice to read");

  // The slice names constants (PhaseUnconfigured, ...); their wire values are
  // declared separately, so both halves are read rather than guessed.
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

test("the screen orders phases exactly as the engine does", () => {
  expect(PHASE_ORDER).toEqual(enginePhases());
});

test("the reader finds the phases it is meant to be comparing", () => {
  // Without this the test above passes on two empty lists, which is how a
  // scanner that stops matching reports everything as fine.
  const phases = enginePhases();
  expect(phases.length).toBeGreaterThanOrEqual(6);
  expect(phases[0]).toBe("unconfigured");
  expect(phases.at(-1)).toBe("ready");
});
