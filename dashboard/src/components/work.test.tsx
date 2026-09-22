/**
 * How the tracker's shared pieces draw a PERSON.
 *
 * The rule is `docs/reference/dashboard-design.md`'s "A person in a grid cell
 * is a resolved name", and its avatar clause: an identity badge is initials on
 * a neutral disc, and the ONE variant it has is structural — the dashed ring a
 * HUMAN seat wears, which says the engine does not run it.
 *
 * These are the pieces every tracker surface draws rather than one screen's
 * columns, which is why they are asserted here: the board card, the compact
 * row and the item page all mount [Assignee], and before [RowChrome] carried
 * the kind not one of them could draw the ring at all. `routes/work/shapes/
 * Grid.test.tsx` holds the same rule over the grid's two column sets.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { Assignee, BoardCard, type RowChrome } from "./work.tsx";
import type { WorkSummary } from "~/protocol/index.ts";

afterEach(cleanup);

const NOW = Date.parse("2031-04-16T12:00:00Z");

/** The chart: `iris` is the human seat, `ada` the agent, `departed` neither. */
const chrome: RowChrome = {
  seatName: (handle) =>
    handle === "ada" ? "Ada Okonkwo" : handle === "iris" ? "Iris Chen" : handle,
  seatKind: (handle) => (handle === "ada" ? "agent" : handle === "iris" ? "human" : undefined),
};

const row = (over: Partial<WorkSummary> = {}): WorkSummary => ({
  id: "t-1",
  key: "ENG-9",
  project: "ENG",
  title: "the wrong subtree",
  type: "task",
  status: "todo",
  // A CARD DRAWS ITS FOOT ONLY WHERE IT HAS A MARK FOR IT, and the assignee
  // lives there — so a row with nothing else on it still needs the handle.
  updated: "2031-04-16T09:00:00Z",
  version: 1,
  ...over,
});

/** The one identity badge a render drew. */
function badge(container: HTMLElement): HTMLElement {
  const marks = container.querySelectorAll(".crewlet-avatar");
  if (marks.length !== 1) throw new Error(`expected one identity badge, drew ${marks.length}`);
  return marks[0] as HTMLElement;
}

// THE RING IS THE BADGE'S ONLY VARIANT, and it is a fact about the SEAT rather
// than about the surface: a person on a board card is the same person as on
// the row under it.
test("a human assignee wears the dashed ring", () => {
  const { container } = render(
    <Assignee handle="iris" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  expect(badge(container).className).toContain("dashed");
});

// AND AN AGENT DOES NOT. `agent` is what nearly every seat in an agent company
// is, so a ring on every one of them would separate nothing.
test("an agent assignee wears no ring", () => {
  const { container } = render(
    <Assignee handle="ada" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  expect(badge(container).className).not.toContain("dashed");
});

// A HANDLE THE CHART DOES NOT HOLD IS NOT AN AGENT — it is a seat that was
// renamed or removed, and the tracker still holds its work. The neutral disc
// is the honest badge; a ring would claim the chart answered when it did not.
test("a handle the chart does not hold falls back to the neutral disc", () => {
  const { container } = render(
    <Assignee handle="departed" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  const mark = badge(container);
  expect(mark.className).not.toContain("dashed");
  // AND THE HANDLE IS STILL THE LABEL, never a blank: "somebody the chart has
  // lost" and "nobody holds this" are different facts.
  expect(mark.textContent).toBe("D");
});

// A CELL HANDED NO RESOLVER AT ALL draws the neutral disc rather than throwing
// or guessing: every prop on this component is optional, and a surface that
// has no chart yet is an ordinary state.
test("no kind resolver draws the neutral disc", () => {
  const { container } = render(<Assignee handle="iris" seatName={chrome.seatName} />);
  expect(badge(container).className).not.toContain("dashed");
});

// THE BOARD CARD READS THE SAME CHROME. It was the surface with the least
// excuse for disagreeing — a column of cards is scanned for who holds what —
// and it drew the badge through the same [Assignee] with the kind dropped.
test("the board card draws a human seat's ring and an agent's plain disc", () => {
  const human = render(
    <BoardCard row={row({ assignee: "iris" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(badge(human.container).className).toContain("dashed");
  cleanup();
  const agent = render(
    <BoardCard row={row({ assignee: "ada" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(badge(agent.container).className).not.toContain("dashed");
});
