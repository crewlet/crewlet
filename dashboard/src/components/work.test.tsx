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

import {
  Assignee,
  CARD_FACTS,
  WorkCard,
  cardHidden,
  cardHideParam,
  fitTags,
  type RowChrome,
} from "./work.tsx";
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
    <WorkCard row={row({ assignee: "iris" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(outline(human.container)).toBe("human");
  cleanup();
  const agent = render(
    <WorkCard row={row({ assignee: "ada" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(outline(agent.container)).toBe("agent");
});

// A CARD MARKS THE EXCEPTIONS. A board is mostly tasks, so a type mark on every
// card is noise — and the one `task` wore was the check a delivered task is
// drawn with. A bug still carries its mark, which is what a reader finds it by.
// THE DASHED SQUARE IS A FIXED 20px BOX, and a word printed inside it runs
// past its edge. The table's Assignee column names its holder, and with
// "Unassigned" drawn IN the square the grid's fit read that overrun as a
// squeezed column and dropped Type, Priority, Due and Updated to make room
// for a word no width could fit. Beside it, the word is the cell's to size.
test("a named unassigned holder prints its word beside the square, never inside it", () => {
  const named = render(<Assignee name />).container;
  const square = named.querySelector(".work-nobody");
  expect(square).toBeTruthy();
  expect(square!.textContent).toBe("");
  expect(named.textContent).toBe("Unassigned");
  cleanup();
  // Unnamed, the square alone carries the name — for a screen reader.
  const bare = render(<Assignee />).container;
  expect(bare.querySelector(".work-nobody .sr-only")?.textContent).toBe("Unassigned");
});

// A READER MAY PUT AWAY WHAT DESCRIBES A TASK, NEVER WHAT SAYS SOMETHING IS
// WRONG. The Display menu's "Card shows" hides size, priority, labels, due
// and tokens; blocked, "blocks N" and open asks stay, because a board that
// could hide them would report a quiet project that is not one — and a card
// whose every remaining mark was put away draws no empty foot.
test("a card draws only the facts not put away, and never hides a state", () => {
  const full = row({
    points: 3,
    priority: "high",
    tags: ["api"],
    due: "2031-04-20T00:00:00Z",
    spend: { tokens: 12_000 } as WorkSummary["spend"],
    blocked: true,
    dependents_count: 2,
  });
  const all = new Set(CARD_FACTS.map((f) => f.key));
  const { container } = render(<WorkCard row={full} href="#/work/ENG-9" now={NOW} omit={all} />);
  expect(container.querySelector(".work-card-points")).toBeNull();
  expect(container.querySelector(".work-due")).toBeNull();
  expect(container.querySelector(".work-card-tokens")).toBeNull();
  expect(container.querySelector("[data-tag]")).toBeNull();
  expect(container.textContent).not.toContain("High");
  expect(container.textContent).toContain("Blocked");
  expect(container.textContent).toContain("blocks 2");
  cleanup();
  const quiet = render(
    <WorkCard
      row={row({ points: 3, due: "2031-04-20T00:00:00Z" })}
      href="#"
      now={NOW}
      omit={all}
    />,
  ).container;
  expect(quiet.querySelector(".work-card-foot")).toBeNull();
  cleanup();
  const shown = render(<WorkCard row={full} href="#" now={NOW} />).container;
  expect(shown.querySelector(".work-card-points")?.textContent).toBe("3 pts");
  expect(shown.querySelector(".work-due")).toBeTruthy();
  expect(shown.querySelector(".work-card-tokens")).toBeTruthy();
  expect(cardHidden("labels,retired,due")).toEqual(new Set(["labels", "due"]));
  expect(cardHideParam(new Set(["tokens", "size"]))).toBe("size,tokens");
});

test("a card draws a type mark for a bug and none for a plain task", () => {
  const task = render(<WorkCard row={row()} href="#/work/ENG-9" now={NOW} chrome={chrome} />);
  expect(task.container.querySelector(".work-card-top .work-type")).toBeNull();
  cleanup();
  const bug = render(
    <WorkCard row={row({ type: "bug" })} href="#/work/ENG-9" now={NOW} chrome={chrome} />,
  );
  expect(bug.container.querySelector(".work-card-top .work-type")?.textContent).toBe("Bug");
});

// ---------------------------------------------------------------------------
// A card's mark row is one line
// ---------------------------------------------------------------------------

// THE LABELS GIVE WAY FIRST, to a "+N": the token count keeps its place at the
// row's end. The row wrapped instead, and the count stood alone on a line of
// its own under a full row of chips.
test("labels that do not fit give way to a count, and the room for it is kept", () => {
  // Everything fits: every label, and no "+N".
  expect(fitTags(200, [40, 40, 40], 24, 6)).toBe(3);
  // One short: the "+N" takes its own room before any label is kept.
  expect(fitTags(130, [40, 40, 40], 24, 6)).toBe(2);
  expect(fitTags(100, [40, 40, 40], 24, 6)).toBe(1);
  // Nothing but the count fits.
  expect(fitTags(40, [40, 40], 24, 6)).toBe(0);
});

test("a card too narrow for its labels names the rest in a +N and keeps the count on the row", () => {
  // THE WIDTHS A BROWSER WOULD LAY OUT: a 220px row, 50px labels, a 30px "+N",
  // a 60px due date and a 40px token count.
  const width = (el: Element): number => {
    const e = el as HTMLElement;
    if (e.classList.contains("work-card-foot")) return 220;
    if (e.dataset.more) return 30;
    if (e.dataset.tag) return 50;
    if (e.classList.contains("work-card-tokens")) return 40;
    if (e.classList.contains("spacer")) return 0;
    return 60;
  };
  const offset = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  const client = Object.getOwnPropertyDescriptor(Element.prototype, "clientWidth");
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return width(this);
    },
  });
  Object.defineProperty(Element.prototype, "clientWidth", {
    configurable: true,
    get(this: Element) {
      return width(this);
    },
  });
  try {
    const card = render(
      <WorkCard
        row={row({
          tags: ["gpu", "backend", "api"],
          due: "2031-04-20",
          spend: { tokens: 56_200, turns: 3, workers: 0, sent_back: 0, reopens: 0 },
        })}
        href="#/work/ENG-9"
        now={NOW}
        chrome={chrome}
      />,
    );
    const foot = card.container.querySelector(".work-card-foot") as HTMLElement;
    const hidden = [...foot.querySelectorAll("[data-tag]")].filter((el) =>
      el.classList.contains("work-card-tag-over"),
    );
    // 220 − 60 (due) − 40 (tokens) = 120 (no stylesheet, so no gap): the "+N"
    // and one label, and the count keeps its place.
    expect(hidden.map((el) => el.textContent)).toEqual(["backend", "api"]);
    const more = foot.querySelector("[data-more]") as HTMLElement;
    expect(more.classList.contains("work-card-tag-over")).toBe(false);
    expect(more.getAttribute("title")).toBe("backend, api");
    expect(more.textContent).toContain("2 more labels: backend, api");
    expect(foot.lastElementChild?.classList.contains("work-card-tokens")).toBe(true);
  } finally {
    if (offset) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offset);
    if (client) Object.defineProperty(Element.prototype, "clientWidth", client);
  }
});
