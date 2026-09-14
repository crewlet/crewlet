/**
 * What a goal panel CLAIMS, in the two places its raw value means something
 * else on screen.
 *
 * A goal is a quarter's question and the numbers on it are read as answers,
 * so the two failures here are the ones that would be believed: a percentage
 * where nobody has measured anything, and a person named by the slug their
 * row is keyed on rather than by what they are called.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { GoalPanel } from "./Goals.tsx";
import type { WorkGoal } from "~/protocol/index.ts";

afterEach(cleanup);

const NOW = Date.parse("2031-04-16T00:00:00Z");

const goal = (over: Partial<WorkGoal> = {}): WorkGoal => ({
  id: "g-1",
  name: "Ship the fleet console",
  owners: ["ada"],
  health: "on_track",
  created_at: "2031-01-06T00:00:00Z",
  updated_at: "2031-04-16T00:00:00Z",
  created_by: "founder",
  version: 1,
  ...over,
});

// A HANDLE IS THE DATABASE'S WORD FOR A PERSON. Every other surface resolves
// it through the chart, and this one printed the slug — so a goal owned by
// Ada Okonkwo was attributed to `ada-okonkwo` beside a board that calls her
// by name. The chip is also a FILTER, so the value it sends has to stay the
// handle while the word it shows is the name.
test("an owner chip shows a name and filters by the handle", () => {
  const onOwner = vi.fn();
  render(
    <GoalPanel
      goal={goal()}
      now={NOW}
      onOwner={onOwner}
      seatName={(h) => (h === "ada" ? "Ada Okonkwo" : h)}
    />,
  );
  const chip = screen.getByText("Ada Okonkwo");
  expect(screen.queryByText("ada")).toBeNull();
  chip.click();
  expect(onOwner).toHaveBeenCalledWith("ada");
});

// A GOAL WITH NO TARGETS GETS NO BAR, because "nothing has happened" and
// "there is nothing to measure" are different facts — and a 0% bar is the
// first of those stated as a measurement.
test("a goal with nothing to measure draws no bar and says why", () => {
  const { container } = render(
    <GoalPanel goal={goal()} now={NOW} onOwner={() => {}} seatName={(h) => h} />,
  );
  expect(container.querySelector(".meter-fill")).toBeNull();
  expect(screen.getByText(/nothing to measure/)).toBeTruthy();
});

// AN UNJUDGED GOAL WEARS NO HEALTH BADGE. A fresh goal nobody has assessed is
// not "on track", and rendering it as anything is the screen making the
// judgement on somebody's behalf.
test("a goal nobody has judged wears no health badge", () => {
  const { container } = render(
    <GoalPanel
      goal={goal({ health: undefined })}
      now={NOW}
      onOwner={() => {}}
      seatName={(h) => h}
    />,
  );
  expect(container.textContent).not.toContain("On track");
  cleanup();
  render(<GoalPanel goal={goal()} now={NOW} onOwner={() => {}} seatName={(h) => h} />);
  expect(screen.getByText("On track")).toBeTruthy();
});
