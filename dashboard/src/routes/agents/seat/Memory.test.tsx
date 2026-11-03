/**
 * The per-seat panel whose wire shape nobody had ever seen.
 *
 * # Why this one, and why as a unit
 *
 * A seat's counterparty profiles were written by the engine from the day their
 * subsystem landed and read by nothing. The client carried a TYPE for them,
 * invented rather than observed, and it was wrong: `CounterpartyProfile` had a
 * `summary` string and a flat `subject` where the engine writes a traits bag
 * and a two-identity subject. A type nothing renders is a guess, and it stays
 * a guess.
 *
 * It is mounted as a UNIT rather than through the seat screen, for the reason
 * the engine's own pure-value suites give: a rule exercised only through the
 * whole machine is a rule nobody re-runs. The screen needs an org, a roster, a
 * projection and four queries before the panel draws at all.
 *
 * The fixtures below are the shapes the engine actually emitted, copied from a
 * running company rather than from the type.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { CounterpartyRow, counterpartyKey } from "./Memory.tsx";
import { Router } from "~/app/router.tsx";

afterEach(cleanup);

const seen = "2026-03-01T09:00:00Z";
/** The reader's clock: two days after `seen`. */
const NOW = Date.parse("2026-03-03T09:00:00Z");

test("a profile renders its traits, not a summary field that does not exist", () => {
  render(
    <Router>
      <CounterpartyRow
        now={NOW}
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
        now={NOW}
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
        now={NOW}
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
  expect(screen.getByText(/No longer learning about them/)).toBeTruthy();
});

// AND A FRESH ONE DOES NOT. The gap is what the sentence is about, so a
// profile corroborated on its last interaction must not carry it.
test("a freshly corroborated profile carries no staleness line", () => {
  render(
    <Router>
      <CounterpartyRow
        now={NOW}
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
  expect(screen.queryByText(/No longer learning about them/)).toBeNull();
});

// WHEN THEY LAST WORKED TOGETHER IS RELATIVE, like every other list on the
// profile (Episodes, Sessions), with the instant on hover: a full timestamp
// with seconds was the one row on the tab that read differently.
test("a profile's last interaction reads relative, the instant on its title", () => {
  render(
    <Router>
      <CounterpartyRow
        now={NOW}
        profile={{
          subject: { handle: "bo", name: "Bo Lang" },
          resolved: true,
          traits: {},
          interactions: 2,
          first_seen_at: seen,
          last_updated_at: seen,
          last_corroborated_at: seen,
        }}
      />
    </Router>,
  );
  const when = screen.getByText("2d ago");
  expect(when.getAttribute("title")).toMatch(/2026/);
  expect(document.body.textContent).not.toMatch(/\d{2}:\d{2}:\d{2}/);
});
