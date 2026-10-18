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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Tag } from "@crewlethq/ui";

import { QueryState, SeatCard } from "./common.tsx";
import { drawnClasses } from "~/testing.tsx";
import type { Seat } from "~/lib/seats.ts";
import { setStepUpConfirmer, type AgentRow, type LiveCall } from "~/protocol/index.ts";
import { page } from "~/lib/session.ts";

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
  expect(screen.getByTitle("First round in progress")).toBeTruthy();
  expect(screen.getByText("Ada Lovelace").closest("a")?.textContent).not.toContain("—");
});

// AND THE ORDINARY CASE KEEPS THE ONE-BASED READING, so the first case cannot be
// satisfied by deleting the field: `round_num` is zero-based and the number a
// person reads is `round_num + 1`.
test("a seat two rounds in reads as its third round", () => {
  render(<SeatCard seat={seat()} agent={running("execute", 2)} nameOf={(k) => k} />);
  expect(screen.getByText("round 3")).toBeTruthy();
});

test("a seat card says its state with the badge's glyph and its pill, and no coloured edge", () => {
  // A card in a state's colour beside a plate in the agent's own put two hues
  // on one seat, so the edge stripe is gone and the badge carries the glyph.
  const { container } = render(
    <SeatCard seat={seat()} agent={running("execute")} nameOf={(k) => k} />,
  );
  const card = container.querySelector("a.seat-card")!;
  expect(card.getAttribute("data-tone")).toBeNull();
  expect(card.querySelector(".crewlet-avatar__status--working")).not.toBeNull();
  expect(card.innerHTML).not.toContain("ring-");
});

test("a seat card links through seatPath, so an empty handle still reaches a page", () => {
  const { container } = render(
    <SeatCard seat={seat({ handle: "" })} agent={undefined} nameOf={(k) => k} />,
  );
  // `#/company/org/seats/` opens nothing. The seat screen resolves a NAME as well
  // as a handle, which is why `seatPath` exists and why the peek this card sits
  // under has always fallen back; the href had not.
  expect(container.querySelector("a.seat-card")!.getAttribute("href")).toBe(
    "#/company/org/seats/Ada%20Lovelace",
  );
});

// A REFUSAL WITH THE ENGINE'S SENTENCE SAYS THE SENTENCE. The generic
// `bad_params` banner says only that the engine refused; the engine's words
// name the parameter, so they replace it.
test("a bad_params refusal with the engine's sentence shows the sentence, not the guess", () => {
  render(<QueryState error="bad_params" detail="days=91 is past the ninety" loading={false} />);
  expect(
    screen.getByText(/The engine refused this request: days=91 is past the ninety/),
  ).toBeTruthy();
  expect(screen.queryByText("The engine refused this request.")).toBeNull();
  cleanup();
  render(<QueryState error="bad_params" loading={false} />);
  expect(screen.getByText("The engine refused this request.")).toBeTruthy();
});

// A REFUSAL NO ACCESS WOULD CLEAR SAYS WHICH RULE DECIDED, in that rule's own
// sentence (`contract/authority.ts`): a credential bound to no seat is told the
// binding, a token the person it acts for, somebody else's record that it is
// somebody else's, and a reader below an artifact's role that its owner can
// share it higher. One sentence for every empty list told each of them that
// what they lacked was leading somebody. Mutation: draw one sentence for every
// empty list and the rows go red; drop the fallback and the last does.
test.each([
  ["unseated", /bind it to a human seat/],
  ["token_refused", /Access tokens can’t do this/],
  ["not_lead", /Only its lead in the org chart/],
  ["seat_only", /Only an agent can do this/],
  ["role", /Its owner can share it with you at a higher one/],
  ["ceiling", /working for can’t do this, so the agent can’t either/],
  ["a_reason_from_a_newer_engine", /^You can’t see this, and no access would change that\./],
])("a refusal by %s that names no access says so in its own sentence", (reason, said) => {
  render(<QueryState error="unauthorized" refusal={{ reason, grants: [] }} loading={false} />);
  const callout = screen.getByText(new RegExp(`Refused as`)).closest(".crewlet-callout")!;
  expect(callout.textContent).toMatch(said);
  expect(callout.textContent).toContain(reason);
});

// A REFUSAL OFFERS WHAT CLEARS ITS OWN RULE, and nothing where no reader can.
//
// The banner offered Sign in for every rule. A reader whose proof had gone
// stale was sent to sign in again rather than to confirm who they are; one
// whose account is not active signed in to the same refusal; and a stale
// proof's grants — what ADMITTED the reader — were drawn as "needs Admin
// access" to an admin. Mutation: offer Sign in for every reason and the
// step_up, stage, unknown_action, seat_only, role and ceiling rows fail; draw
// the grants whatever the reason and the step_up row says "needs Admin access"
// and the role row names Admin where its owner's share is the remedy.
test.each([
  ["no_grant", ["worklet"], "Sign in", /needs Worklet access/],
  ["not_lead", ["admin"], "Sign in", /needs Admin access/],
  ["step_up", ["admin"], "Confirm it’s you", /Confirm it’s you, then try again/],
  ["stage", [], "Try again", /Your account isn’t active/],
  ["unseated", [], "Sign in as yourself", /bind it to a human seat/],
  ["token_refused", [], "Sign in as yourself", /Access tokens can’t do this/],
  ["unknown_action", [], "Reload", /no rule for that request/],
  ["seat_only", ["admin"], null, /Only an agent can do this/],
  ["role", ["admin"], "Try again", /Its owner can share it with you at a higher one/],
  ["ceiling", [], null, /working for can’t do this/],
  ["unnamed", [], null, /didn’t say what it’s about/],
  ["a_reason_from_a_newer_engine", [], null, /no access would change that/],
  ["a_reason_from_a_newer_engine", ["chatlet"], "Sign in", /needs Chatlet access/],
])("a refusal by %s naming %j offers %s", (reason, grants, control, said) => {
  render(<QueryState error="unauthorized" refusal={{ reason, grants }} loading={false} />);
  const callout = screen.getByText(new RegExp(`Refused as`)).closest(".crewlet-callout")!;
  expect(callout.textContent).toMatch(said);
  const buttons = [...callout.querySelectorAll("button")].map((b) => b.textContent);
  expect(buttons).toEqual(control === null ? [] : [control]);
  if (reason === "step_up" || reason === "role") {
    expect(callout.textContent).not.toMatch(/needs Admin access/);
  }
});

// EACH CONTROL DOES WHAT IT SAYS: Confirm asks the one step-up ceremony, and
// asking again reloads this screen, after an admin has acted. Mutation: point
// Confirm at the sign-in and the first assertion fails.
test("a stale proof's control asks for the step-up, and an inactive account's reloads", () => {
  const asked = vi.fn(() => Promise.resolve(false));
  const uninstall = setStepUpConfirmer(asked);
  const reload = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
  try {
    render(
      <QueryState
        error="unauthorized"
        refusal={{ reason: "step_up", grants: ["admin"] }}
        loading={false}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Confirm it’s you/ }));
    expect(asked).toHaveBeenCalledTimes(1);
    cleanup();
    render(
      <QueryState error="unauthorized" refusal={{ reason: "stage", grants: [] }} loading={false} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Try again/ }));
    expect(reload).toHaveBeenCalledWith(location.hash);
  } finally {
    uninstall();
    reload.mockRestore();
  }
});
