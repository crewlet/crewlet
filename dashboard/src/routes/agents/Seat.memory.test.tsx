/**
 * The two per-seat panels whose wire shape nobody had ever seen.
 *
 * # Why these two, and why as units
 *
 * A seat's counterparty profiles and its thread ledger were both written by
 * the engine from the day their subsystems landed and read by nothing. The
 * client carried a TYPE for each, invented rather than observed, and both were
 * wrong: `CounterpartyProfile` had a `summary` string and a flat `subject`
 * where the engine writes a traits bag and a two-identity subject, and
 * `ConversationRow` had `source` and `local` fields no answer has ever
 * carried. A type nothing renders is a guess, and it stays a guess.
 *
 * These are mounted as UNITS rather than through the seat screen, for the
 * reason the engine's own pure-value suites give: a rule exercised only
 * through the whole machine is a rule nobody re-runs. The screen needs an org,
 * a roster, a projection and four queries before either panel draws at all.
 *
 * The fixtures below are the shapes the engine actually emitted, copied from a
 * running company rather than from the type.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { CounterpartyRow, ThreadTurn, counterpartyKey } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { overflowing } from "~/testing.tsx";

afterEach(cleanup);

const seen = "2026-03-01T09:00:00Z";

test("a profile renders its traits, not a summary field that does not exist", () => {
  render(
    <Router>
      <CounterpartyRow
        profile={{
          subject: { handle: "bo", name: "Bo Lang" },
          resolved: true,
          traits: { prefers: "async", timezone: "CET" },
          interactions: 7,
          first_seen_at: seen,
          last_updated_at: seen,
          last_corroborated_at: seen,
        }}
      />
    </Router>,
  );
  expect(screen.getByText("Bo Lang")).toBeTruthy();
  expect(screen.getByText("7 interactions")).toBeTruthy();
  // THE KEYS ARE THE MODEL'S OWN, so they are listed as they come rather
  // than mapped onto a fixed set of fields this company happened to produce.
  expect(screen.getByText(/prefers: async/)).toBeTruthy();
  expect(screen.getByText(/timezone: CET/)).toBeTruthy();
});

// AN UNMAPPED PERSON IS STILL SOMEBODY. A counterparty on a chat surface this
// company has not mapped to a seat has no handle at all, and a row that
// rendered a blank name there would lose the profile rather than the link.
test("an unresolved subject is named and marked, not blank", () => {
  render(
    <Router>
      <CounterpartyRow
        profile={{
          subject: { external_id: "U0EXTERNAL", platform: "slack", name: "Sam Rivera" },
          resolved: false,
          traits: {},
          interactions: 1,
          first_seen_at: seen,
          last_updated_at: seen,
          last_corroborated_at: seen,
        }}
      />
    </Router>,
  );
  expect(screen.getByText("Sam Rivera")).toBeTruthy();
  expect(screen.getByText("slack")).toBeTruthy();
  // SEEN AND NOTHING BELIEVED is its own state, not an empty row.
  expect(screen.getByText(/nothing believed about them yet/i)).toBeTruthy();
});

// IDENTITY IS NOT THE NAME. A person renaming themselves on a chat surface
// must not read as a different colleague, which is why the engine keeps the
// name out of the identity — and why a key built from it would re-key every
// row the day somebody changed their display name.
test("a profile's key is its identity, never its display name", () => {
  expect(
    counterpartyKey({
      subject: { handle: "bo", name: "Bo Lang" },
      resolved: true,
      traits: {},
      interactions: 0,
      first_seen_at: seen,
      last_updated_at: seen,
      last_corroborated_at: seen,
    }),
  ).toBe("bo");
  expect(
    counterpartyKey({
      subject: { external_id: "U0EXTERNAL", platform: "slack", name: "Sam Rivera" },
      resolved: false,
      traits: {},
      interactions: 0,
      first_seen_at: seen,
      last_updated_at: seen,
      last_corroborated_at: seen,
    }),
  ).toBe("slack:U0EXTERNAL");
});

// A PROFILE NOBODY HAS CORROBORATED is the state the Plan phase's own prefetch
// demotes on: this seat is still working with them and has stopped learning
// about them. A panel carrying one instant could not show it.
test("a stale profile says it has stopped learning, not that it has stopped working", () => {
  render(
    <Router>
      <CounterpartyRow
        profile={{
          subject: { handle: "bo", name: "Bo Lang" },
          resolved: true,
          traits: { prefers: "async" },
          interactions: 40,
          first_seen_at: "2025-01-01T00:00:00Z",
          last_updated_at: "2026-03-01T09:00:00Z",
          last_corroborated_at: "2025-06-01T09:00:00Z",
        }}
      />
    </Router>,
  );
  expect(screen.getByText(/stopped learning about them/i)).toBeTruthy();
});

// AND A FRESH ONE DOES NOT. The gap is what the sentence is about, so a
// profile corroborated on its last interaction must not carry it.
test("a freshly corroborated profile carries no staleness line", () => {
  render(
    <Router>
      <CounterpartyRow
        profile={{
          subject: { handle: "bo", name: "Bo Lang" },
          resolved: true,
          traits: { prefers: "async" },
          interactions: 40,
          first_seen_at: "2025-01-01T00:00:00Z",
          last_updated_at: seen,
          last_corroborated_at: seen,
        }}
      />
    </Router>,
  );
  expect(screen.queryByText(/stopped learning about them/i)).toBeNull();
});

// `reply` AND `unsent` CARRY THE SAME ARTIFACT, and which one holds it is the
// whole record of whether anybody received it. A turn can end with real work
// done and no way to say so — the round budget ran out, the loop broke — and a
// panel that rendered the two alike would show work announced to nobody as
// announced. That is the exact confusion that had a seat answer a follow-up
// against a message it never sent.
test("an undelivered turn is marked, and a delivered one is not", () => {
  const { unmount } = render(
    <Router>
      <ThreadTurn entry={{ turn_id: "t-1", at: seen, reply: "Filed LEAD-1.", trigger: "chat" }} />
    </Router>,
  );
  expect(screen.getByText("Filed LEAD-1.")).toBeTruthy();
  expect(screen.queryByText(/Nothing was delivered/i)).toBeNull();
  unmount();

  render(
    <Router>
      <ThreadTurn entry={{ turn_id: "t-2", at: seen, unsent: "Filed LEAD-1.", trigger: "chat" }} />
    </Router>,
  );
  expect(screen.getByText(/Nothing was delivered/i)).toBeTruthy();
  expect(screen.getByText(/Filed LEAD-1\./)).toBeTruthy();
});

// A TURN'S TOOL LOG IS A BLOCK, NOT A BARE `pre`. It was hand-written markup
// carrying the block stylesheet's own class, which put it under a ceiling with
// `overflow: auto` — so a long log was a scroll container with no tab stop and
// no accessible name, and in Chrome and Safari a scroll container with no
// tabindex cannot be scrolled by keyboard at all. This is the one panel where
// the whole record of what a turn DID is in that box.
test("a long tool log is a named, focusable region rather than an unreachable box", () => {
  const container = overflowing(
    <Router>
      <ThreadTurn entry={{ turn_id: "t-3", at: seen, tool_calls: "read_file\n".repeat(400) }} />
    </Router>,
  );
  const region = container.querySelector('[role="region"][tabindex="0"]');
  expect(region).not.toBeNull();
  expect(region?.getAttribute("aria-label")).toBe("Tool calls in this turn");
});
