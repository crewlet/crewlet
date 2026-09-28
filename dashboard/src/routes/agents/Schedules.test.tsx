/**
 * Which zone the fires after the next one are worked out in.
 *
 * The engine resolves a schedule's own timezone and evaluates the expression
 * there (`internal/schedule/entries.go`, `Expr.FireTimes`), so a reading aid
 * that worked the same expression out in UTC did not merely drift across a
 * daylight-saving change — it was wrong by the zone's STANDING offset on every
 * row of every company that does not run in UTC. This panel is the only place
 * in the dashboard that predicts a fire rather than reporting the one the
 * engine already computed, so it is the only place that can be wrong this way.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { NextFires, OutcomeTag, Wakes } from "./Schedules.tsx";
import { Router } from "~/app/router.tsx";
import { setZone } from "~/lib/prefs.ts";
import type { ScheduleRow } from "~/protocol/index.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  setZone("UTC");
});

afterEach(() => {
  cleanup();
  setZone("");
});

const NOW = Date.parse("2026-06-15T00:30:00Z");

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    scope_id: "ceo",
    name: "standup",
    cron: "0 9 * * *",
    timezone: "Asia/Tokyo",
    task: "run the standup",
    target: "",
    enabled: true,
    timeout_seconds: 600,
    catchup: false,
    runners: ["ceo"],
    next_run: "2026-06-16T00:00:00Z",
    ...over,
  };
}

// 09:00 IN TOKYO IS 00:00Z, and the nine-hour gap is the whole finding: the
// list used to be worked out in UTC and put every fire nine hours late, for
// ever, on a screen whose header carries the engine's own answer right above
// it.
test("the fires are worked out in the schedule's zone, not in UTC", () => {
  render(<NextFires row={row()} now={NOW} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  expect(screen.queryByText(/09:00:00/)).toBeNull();
  expect(screen.getByText(/evaluated in Asia\/Tokyo/)).toBeTruthy();
});

// AND A SCHEDULE THAT NAMES NO ZONE ARRIVES ON THE COMPANY'S CLOCK (ADR-0018):
// the engine resolves it, so the row names the company's zone and the fires are
// worked out there. The screen defaults nothing — a zone-less row read as UTC
// was nine hours out for a company in Tokyo — so a row that somehow named no
// zone draws no fires rather than a list on a clock the engine does not fire
// on.
test("a row is worked out in the zone the engine names, and never in a default", () => {
  render(<NextFires row={row({ timezone: "Asia/Tokyo" })} now={NOW} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  cleanup();
  const { container } = render(<NextFires row={row({ timezone: "" })} now={NOW} count={3} />);
  expect(container.textContent).toBe("");
});

// A LEDGER OUTCOME IS A WORD, AND NEITHER SKIP ASKS FOR ANYBODY. The raw enum
// sat in an amber pill on every row of a company whose node had been down
// overnight: `skipped_catchup` is the catchup cap doing its job, and amber is
// the one state that asks a person for something.
test("an outcome is named in words, and a skip is never the needs-you amber", () => {
  const { container } = render(
    <>
      <OutcomeTag outcome="fired" />
      <OutcomeTag outcome="skipped_catchup" />
      <OutcomeTag outcome="skipped_paused" />
    </>,
  );
  expect(container.textContent).not.toContain("_");
  expect(
    screen.getByText("skipped · missed").closest(".crewlet-tag")?.getAttribute("title"),
  ).toMatch(/catchup window/);
  expect(container.querySelector(".crewlet-tag--warning")).toBeNull();
});

// THE FACES ARE THE CHART'S BADGES. Handed the whole name the kit made "AS",
// "AI" and "AA" of three seats the chart draws as SW, FS and AS.
test("a stack of the seats a schedule wakes draws each seat's own badge", () => {
  const names: Record<string, string> = {
    swe: "Agent SWE",
    fs: "Agent Frontend SWE",
    as: "Agent AI Systems Engineer",
  };
  const who = (h: string) => ({ name: names[h]!, kind: "agent" as const });
  const { container } = render(
    <Router>
      <Wakes runners={["swe", "fs", "as"]} who={who} />
    </Router>,
  );
  const faces = [...container.querySelectorAll(".crewlet-avatar-stack__member")].map((el) =>
    el.textContent?.trim(),
  );
  expect(faces).toEqual(["SW", "FS", "AS"]);
});

// SEVERAL SEATS ARE A COUNT AND FACES. A chip per seat in a column that never
// wraps cut three names to "A" and "Age…"; the names are still said.
test("a schedule waking several seats says how many, and names them for a screen reader", () => {
  const who = (h: string) => ({ name: `Agent ${h.toUpperCase()}`, kind: "agent" as const });
  const { container } = render(
    <Router>
      <Wakes runners={["swe", "fs", "as"]} who={who} />
    </Router>,
  );
  expect(container.textContent).toContain("3 seats");
  expect(container.querySelector(".sr-only")?.textContent).toContain(
    "Agent SWE, Agent FS, Agent AS",
  );
  cleanup();
  render(
    <Router>
      <Wakes runners={["swe"]} who={who} />
    </Router>,
  );
  expect(screen.getByRole("link", { name: /Agent SWE/ }).getAttribute("href")).toBe(
    "#/agents/seats/swe",
  );
});
