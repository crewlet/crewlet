/**
 * How the tracker's shared pieces draw a PERSON.
 *
 * The rule is `docs/reference/dashboard-design.md`'s "A person in a grid cell
 * is a resolved name", and its avatar clause: an identity badge is initials,
 * and the ONE variant it has is structural — its OUTLINE, a circle for a
 * person and a squircle for an agent.
 *
 * These are the pieces every tracker surface draws rather than one screen's
 * columns, which is why they are asserted here: the board card, the compact
 * row and the item page all mount [Assignee], and before [RowChrome] carried
 * the kind not one of them could draw a person at all. `routes/work/shapes/
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

/** Which outline the kit drew the badge in: a person's circle or an agent's squircle. */
function outline(container: HTMLElement): "human" | "agent" {
  const mark = badge(container);
  const human = mark.classList.contains("crewlet-avatar--human");
  const agent = mark.classList.contains("crewlet-avatar--agent");
  if (human === agent)
    throw new Error(`the badge is drawn as both kinds or neither: ${mark.className}`);
  return human ? "human" : "agent";
}

// THE OUTLINE IS THE BADGE'S ONLY VARIANT, and it is a fact about the SEAT
// rather than about the surface: a person on a board card is the same person
// as on the row under it.
test("a human assignee is drawn as a person", () => {
  const { container } = render(
    <Assignee handle="iris" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  expect(outline(container)).toBe("human");
});

// AND AN AGENT IS DRAWN AS AN AGENT.
test("an agent assignee is drawn as an agent", () => {
  const { container } = render(
    <Assignee handle="ada" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  expect(outline(container)).toBe("agent");
});

// A HANDLE THE CHART DOES NOT HOLD is a seat that was renamed or removed, and
// the tracker still holds its work. It takes the kit's default outline, an
// agent's, because a person is always DECLARED: a circle there would claim
// the chart answered when it did not.
test("a handle the chart does not hold is never drawn as a person", () => {
  const { container } = render(
    <Assignee handle="departed" seatName={chrome.seatName} seatKind={chrome.seatKind} />,
  );
  expect(outline(container)).toBe("agent");
  // AND THE HANDLE IS STILL THE LABEL, never a blank: "somebody the chart has
  // lost" and "nobody holds this" are different facts.
  expect(badge(container).textContent).toBe("DE");
});

// A CELL HANDED NO RESOLVER AT ALL draws the default rather than throwing or
// guessing: every prop on this component is optional, and a surface that has
// no chart yet is an ordinary state.
test("no kind resolver never draws a person", () => {
  const { container } = render(<Assignee handle="iris" seatName={chrome.seatName} />);
  expect(outline(container)).toBe("agent");
});

// THE BOARD CARD READS THE SAME CHROME. It was the surface with the least
// excuse for disagreeing — a column of cards is scanned for who holds what —
// and it drew the badge through the same [Assignee] with the kind dropped.
test("the board card draws a human seat as a person and an agent as an agent", () => {
  const human = render(
    <BoardCard row={row({ assignee: "iris" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(outline(human.container)).toBe("human");
  cleanup();
  const agent = render(
    <BoardCard row={row({ assignee: "ada" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(outline(agent.container)).toBe("agent");
});
