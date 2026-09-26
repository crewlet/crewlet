/**
 * The seat card: the roster's row on People, and Live now's.
 *
 * It is the one surface that draws a turn phase and is not itself a turn view,
 * which is how it came to draw every phase in one `info` fill while every other
 * surface drew `PhaseTag` — `execute` and `review` identical side by side on the
 * roster, and each of them a different colour one screen over. jsdom computes no
 * layout and paints no pixel, so what is asserted here is the CLAIM: the variant
 * the card asks the design system for, read back as the classes that design
 * system draws for it.
 *
 * And the round beside it: `round_num` is the engine's ZERO-BASED counter with
 * `-1` for the opening frame a phase publishes before its first provider call.
 * Two of the five working cards read "round —" for exactly that, with nothing
 * saying why a working seat had no round — which is the one fact the sentinel
 * states precisely.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Tag, type TagVariant } from "@crewlethq/ui";

import { SeatCard } from "./common.tsx";
import { drawnClasses, isDrawnAs } from "~/testing.tsx";
import type { Seat } from "~/lib/seats.ts";
import type { AgentRow, LiveCall } from "~/protocol/index.ts";

afterEach(cleanup);

const seat = (over: Partial<Seat> = {}): Seat =>
  ({
    key: "ada",
    name: "Ada Lovelace",
    handle: "ada",
    kind: "agent",
    goal: "",
    unitChain: [],
    unit: null,
    reports: [],
    autoReports: [],
    managers: [],
    manager: null,
    ...over,
  }) as Seat;

/** A seat mid-phase: the only state that draws the pill at all. */
const running = (phase: string, roundNum = 0): AgentRow =>
  ({
    id: "ada",
    role: "Ada Lovelace",
    activity: "working",
    live_call: {
      phase,
      in_progress: true,
      round_num: roundNum,
      rounds_used: Math.max(roundNum + 1, 0),
      updated_at: new Date().toISOString(),
    } as LiveCall,
  }) as AgentRow;

const PHASES: [phase: string, variant: TagVariant][] = [
  ["execute", "phase-execute"],
  ["review", "phase-review"],
];

test("a running seat's phase is drawn in that phase's own hue", () => {
  const base = drawnClasses(Tag, { children: "x" })[0]!;
  const drawn = PHASES.map(([phase, variant]) => {
    const { getByText, unmount } = render(
      <SeatCard seat={seat()} agent={running(phase)} sandboxes={[]} />,
    );
    const pill = getByText(phase).closest(`.${base}`)!;
    expect(
      isDrawnAs(pill, Tag, { variant, children: phase }, { children: phase }),
      `the ${phase} pill is not drawn as ${variant}`,
    ).toBe(true);
    const classes = [...pill.classList];
    unmount();
    return classes;
  });
  // THE REPORTED DEFECT ITSELF, and the assertion that stays red however the
  // pill is respelled: two phases, two fills. One hardcoded variant for every
  // phase passes every per-phase check above only if each of them names that
  // variant, and fails this one always.
  expect(drawn[0]).not.toEqual(drawn[1]);
});

test("the phase word is beside the colour, never replaced by it", () => {
  // Colour is never the only carrier: protan and deutan vision are what the hues
  // are measured against, and a reader with neither still reads a word. It is
  // lowercased on the way, because the value is a store column and nothing
  // normalises its case on the wire.
  render(<SeatCard seat={seat()} agent={running("Execute")} sandboxes={[]} />);
  expect(screen.getByText("execute")).not.toBeNull();
});

test("a working seat whose first round has not come back says so", () => {
  render(<SeatCard seat={seat()} agent={running("execute", -1)} sandboxes={[]} />);
  expect(screen.getByText("starting")).toBeTruthy();
  expect(screen.getByTitle(/first model round has not come back/)).toBeTruthy();
  expect(screen.getByText("Ada Lovelace").closest("a")?.textContent).not.toContain("—");
});

// AND THE ORDINARY CASE KEEPS THE ONE-BASED READING, so the first case cannot be
// satisfied by deleting the field: `round_num` is zero-based and the number a
// person reads is `round_num + 1`.
test("a seat two rounds in reads as its third round", () => {
  render(<SeatCard seat={seat()} agent={running("execute", 2)} sandboxes={[]} />);
  expect(screen.getByText("round 3")).toBeTruthy();
});

test("a seat the engine reported no handle for still links to its page", () => {
  const { container } = render(
    <SeatCard seat={seat({ handle: "" })} agent={undefined} sandboxes={[]} />,
  );
  // `#/company/people/` opens nothing. The seat screen resolves a NAME as well
  // as a handle, which is why `seatPath` exists and why the peek this card sits
  // under has always fallen back; the href had not.
  expect(container.querySelector("a.seat-card")!.getAttribute("href")).toBe(
    "#/company/people/Ada%20Lovelace",
  );
});
