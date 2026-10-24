/**
 * An incomplete answer is said in ONE set of words wherever it is said.
 *
 * Two surfaces say it, the page's state bar and a panel's own banner
 * ([Coverage]), and they used to write the sentence out twice, each with its
 * own count, scope and remedy, so one could change without the other. Both
 * say `INCOMPLETE_NOTE` now, whose remedy is the only one a reader can act on
 * (a newer engine that can read the records, not a refresh); these hold the
 * two to it, and to saying nothing else about the shortfall.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { StateBar } from "~/app/frame/StateBar.tsx";
import { GENERATION_STRIDE } from "~/contract/positions.ts";
import {
  Coverage,
  INCOMPLETE_NOTE,
  appliedThrough,
  positionWords,
  type CoverageFacts,
} from "./work.tsx";

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

test("the state bar and a panel's banner say an incomplete answer in the same words", () => {
  for (const surface of [
    () => render(<StateBar degraded={null} coverage={SHORT} />),
    () => render(<Coverage answer={SHORT} />),
  ]) {
    expect(surface().container.textContent).toContain(INCOMPLETE_NOTE);
    cleanup();
  }
  expect(INCOMPLETE_NOTE).toMatch(/newer engine version/);
});

// AND THE SAME WHATEVER THE ANSWER COUNTED: an answer that named no records
// is no less incomplete, and the sentence does not change with it.
test("an incomplete answer that counted nothing is said the same way", () => {
  const { container } = render(<StateBar degraded={null} coverage={{ complete: false }} />);
  expect(container.textContent).toContain(INCOMPLETE_NOTE);
});

// A POSITION IS PACKED ON THE WIRE AND READ UNPACKED. `applied_through` and
// `log_seq` are (generation × 2^40) + seq, so after a log's first re-anchor
// the raw numbers were thirteen digits a reader could not place. The first
// generation, where nearly every log lives, still reads as the bare sequence.
test("a re-anchored log's position reads as its generation and sequence", () => {
  const at = (generation: number, seq: number) => generation * GENERATION_STRIDE + seq;
  expect(positionWords(at(0, 41))).toBe("41");
  expect(positionWords(at(1, 41))).toBe("1:41");
  expect(appliedThrough({ applied_through: at(0, 41), log_seq: at(0, 88) })).toBe(
    "applied through 41 of 88",
  );
  expect(appliedThrough({ applied_through: at(1, 41), log_seq: at(2, 3) })).toBe(
    "applied through 1:41 of 2:3",
  );
  // THE COMPARISON STAYS PACKED: a node a generation behind is behind even
  // where its sequence is higher, and one caught up across a re-anchor is not.
  expect(appliedThrough({ applied_through: at(1, 900), log_seq: at(2, 3) })).not.toBeNull();
  expect(appliedThrough({ applied_through: at(2, 3), log_seq: at(2, 3) })).toBeNull();
});
