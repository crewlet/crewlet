/**
 * An incomplete answer is said in ONE set of words wherever it is said.
 *
 * Three surfaces say it — the page's state bar, a panel's own banner
 * ([Coverage]) and the audit, which names the source a shortfall belongs to —
 * and the scope it names and the remedy it gives were written out twice
 * beside the shared lead, while the audit, which took only the lead, said
 * neither. A reader who met "Affected: project:ENG" and "not a refresh" on
 * one screen met an audit that said writes were missing and not where, or
 * what would bring them back. The audit's own case holds its sentence; these
 * hold the other two to the same words.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { StateBar } from "~/app/frame/StateBar.tsx";
import { Coverage, type CoverageFacts } from "./work.tsx";

afterEach(cleanup);

const SHORT: CoverageFacts = {
  complete: false,
  incomplete: {
    records: 2,
    version: 9,
    from: { stream: "CREWLET_WORK_LOG", generation: 1, seq: 40 },
    scope: ["project:ENG", "task:t-1"],
  },
};

const SCOPE = "Affected: project:ENG, task:t-1.";
const REMEDY =
  "Record version 9, from sequence 40 — a build that can read it is what resolves this, not a refresh.";

test("the state bar names what an incomplete answer affects and what resolves it", () => {
  const { container } = render(<StateBar degraded={null} coverage={SHORT} />);
  const said = container.textContent ?? "";
  expect(said).toContain("This answer is incomplete — 2 record(s) this build cannot read.");
  expect(said).toContain(SCOPE);
  expect(said).toContain(REMEDY);
});

test("a panel's coverage banner says the same", () => {
  const { container } = render(<Coverage answer={SHORT} />);
  const said = container.textContent ?? "";
  expect(said).toContain("This answer is incomplete — 2 record(s) this build cannot read");
  expect(said).toContain(SCOPE);
  expect(said).toContain(REMEDY);
});

// AND NOTHING WHERE THE ANSWER DID NOT SAY: no scope is no "Affected", and an
// answer that counted no records names no version to upgrade past.
test("an incomplete answer that counted nothing names no scope and no remedy", () => {
  const { container } = render(<StateBar degraded={null} coverage={{ complete: false }} />);
  const said = container.textContent ?? "";
  expect(said).toContain("This answer is incomplete.");
  expect(said).not.toContain("Affected");
  expect(said).not.toContain("Record version");
});
