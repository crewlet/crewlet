/**
 * The seat card: the roster's row on People.
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

import { cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, expect, test } from "vitest";
import { Tag } from "@crewlethq/ui";

import { QueryState, SeatCard } from "./common.tsx";
import { drawnClasses } from "~/testing.tsx";
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

const PHASES = ["execute", "review"] as const;

// A PHASE IS A CATEGORY, AND A CATEGORY HAS NO COLOUR. The design system took
// its phase hues away because beside a state badge the state must be the one
// coloured thing on the row; a seat card that tinted its phase pill again would
// be a second hue saying "where", competing with the one saying "how".
test("a running seat's phase is a word on the neutral pill, never a hue", () => {
  const neutral = drawnClasses(Tag, { variant: "neutral", children: "x" });
  const drawn = PHASES.map((phase) => {
    const { getByText, unmount } = render(
      <SeatCard seat={seat()} agent={running(phase)} nameOf={(k) => k} />,
    );
    const pill = getByText(phase).closest(`.${neutral[0]}`)!;
    const classes = [...pill.classList];
    expect(classes.sort(), `the ${phase} pill is not drawn as the neutral tag`).toEqual(
      [...neutral].sort(),
    );
    unmount();
    return classes;
  });
  // And the two phases are drawn IDENTICALLY — only the word differs — which
  // stays red however a per-phase fill is respelled.
  expect(drawn[0]).toEqual(drawn[1]);
});

test("the phase is said as its word", () => {
  // The word IS the phase now, so it has to be there. It is lowercased on the
  // way, because the value is a store column and nothing normalises its case
  // on the wire.
  render(<SeatCard seat={seat()} agent={running("Execute")} nameOf={(k) => k} />);
  expect(screen.getByText("execute")).not.toBeNull();
});

test("a working seat whose first round has not come back says so", () => {
  render(<SeatCard seat={seat()} agent={running("execute", -1)} nameOf={(k) => k} />);
  expect(screen.getByText("round 1")).toBeTruthy();
  expect(screen.getByTitle(/first model round is in flight and has not come back/)).toBeTruthy();
  expect(screen.getByText("Ada Lovelace").closest("a")?.textContent).not.toContain("—");
});

// AND THE ORDINARY CASE KEEPS THE ONE-BASED READING, so the first case cannot be
// satisfied by deleting the field: `round_num` is zero-based and the number a
// person reads is `round_num + 1`.
test("a seat two rounds in reads as its third round", () => {
  render(<SeatCard seat={seat()} agent={running("execute", 2)} nameOf={(k) => k} />);
  expect(screen.getByText("round 3")).toBeTruthy();
});

test("a seat the engine reported no handle for still links to its page", () => {
  const { container } = render(
    <SeatCard seat={seat({ handle: "" })} agent={undefined} nameOf={(k) => k} />,
  );
  // `#/agents/seats/` opens nothing. The seat screen resolves a NAME as well
  // as a handle, which is why `seatPath` exists and why the peek this card sits
  // under has always fallen back; the href had not.
  expect(container.querySelector("a.seat-card")!.getAttribute("href")).toBe(
    "#/agents/seats/Ada%20Lovelace",
  );
});

// A REFUSAL WITH THE ENGINE'S SENTENCE SAYS THE SENTENCE. The generic
// `bad_params` banner blames the screen, which is right when the screen asked
// wrong and wrong when a reader chose a window past the history — the engine's
// words name the parameter either way, so they replace the guess.
test("a bad_params refusal with the engine's sentence shows the sentence, not the guess", () => {
  render(<QueryState error="bad_params" detail="days=91 is past the ninety" loading={false} />);
  expect(
    screen.getByText(/The engine refused this request: days=91 is past the ninety/),
  ).toBeTruthy();
  expect(screen.queryByText(/screen’s bug/)).toBeNull();
  cleanup();
  render(<QueryState error="bad_params" loading={false} />);
  expect(screen.getByText(/screen’s bug to fix/)).toBeTruthy();
});

// A REFUSAL ON AUTHORITY SAYS WHAT WOULD CHANGE IT. The banner read "the
// credential you presented does not carry the grant this answer needs" and
// named no grant, because the socket delivered the code alone; the engine's
// frame now carries the rule and the grants, and the banner draws them — a
// grant to ask for, or the honest answer that no grant would help.
test("a refusal on authority names the grants that would admit the reader", () => {
  render(
    <QueryState
      error="unauthorized"
      refusal={{ reason: "no_grant", grants: ["audit:read"] }}
      loading={false}
    />,
  );
  expect(screen.getByText("audit:read")).toBeTruthy();
  expect(screen.getByText("no_grant")).toBeTruthy();
});

test("a refusal no grant would change says so rather than naming one", () => {
  render(
    <QueryState
      error="unauthorized"
      refusal={{ reason: "not_lead", grants: [] }}
      loading={false}
    />,
  );
  expect(screen.getByText(/no grant would change that/)).toBeTruthy();
  expect(screen.getByText("not_lead")).toBeTruthy();
});

// A REFUSAL BY THE STATE LOG THAT WAITING WILL NOT CLEAR IS NOT "CATCHING UP".
// The `unavailable` banner promises the screen asks again on its own, which for
// a full log or a record the node cannot decode is a promise of a loop: the
// read is refused the same until an operator acts or another node is asked. So
// the banner says that, with the refusal's own words and code. A refusal that
// clears is the control and keeps the catching-up banner.
test("a log refusal no wait clears says so and names it", () => {
  render(
    <QueryState
      error="unavailable"
      refusal={{ code: "log_full", detail: "raise the stream's byte ceiling", retryAfter: 0 }}
      loading={false}
    />,
  );
  expect(screen.getByText(/asking it again will not change that/)).toBeTruthy();
  expect(screen.getByText(/raise the stream's byte ceiling/)).toBeTruthy();
  expect(screen.getByText("log_full")).toBeTruthy();
  expect(screen.queryByText(/asks again on its own/)).toBeNull();
});

// A REST READ NO ANSWER CAME BACK TO IS NOT A SOCKET THAT WENT AWAY. It was
// drawn as `closed`, which says the connection went and the screen reads again
// once the socket is back: false whenever the socket stayed up, which is the
// ordinary case for one request past its deadline. Its own sentence says only
// what is true of every such read, and that it is asked again regardless.
test("a read no answer came back to says so, and never that the socket went", () => {
  render(<QueryState error="unanswered" refusal={null} loading={false} />);
  expect(screen.getByText(/No answer from the engine reached this page/)).toBeTruthy();
  expect(screen.getByText(/asks again on its own/)).toBeTruthy();
  expect(screen.queryByText(/connection went away/)).toBeNull();
  expect(screen.queryByText(/code this build does not know/)).toBeNull();
});

test("a log refusal that clears keeps the catching-up banner", () => {
  render(
    <QueryState
      error="unavailable"
      refusal={{ code: "behind", detail: "40 000 records behind", retryAfter: 12 }}
      loading={false}
    />,
  );
  expect(screen.getByText(/asks again on its own/)).toBeTruthy();
  expect(screen.queryByText(/asking it again will not change that/)).toBeNull();
});
