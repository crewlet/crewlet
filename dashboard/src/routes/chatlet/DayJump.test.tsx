/**
 * A day's pill as a Jump to menu: the ranges it offers are the ones the
 * room's history reaches.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { DayJump } from "./DayJump.tsx";

afterEach(cleanup);

const jumps = () => ({ latest: vi.fn(), beginning: vi.fn(), toDay: vi.fn() });

function open(first: string) {
  const to = jumps();
  render(
    <DayJump
      label="Today"
      day="2026-11-08"
      today="2026-11-08"
      newest="2026-11-08"
      first={first}
      jumps={to}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: /Today/ }));
  return to;
}

const offered = () => screen.getAllByRole("menuitem").map((item) => item.textContent);

// A DAY BACK IS OFFERED WHERE THE ROOM GOES BACK FURTHER THAN THAT, and lands
// on yesterday's first message. Mutation: drop the item and the menu goes
// from Today straight to Last week.
test("offers Yesterday once the room's history reaches past it, and jumps to that day", () => {
  const to = open("2026-10-01");
  expect(offered()).toEqual([
    "Today",
    "Yesterday",
    "Last week",
    "Last month",
    "The very beginning",
    "Jump to a specific date",
  ]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Yesterday" }));
  expect(to.toDay).toHaveBeenCalledWith("2026-11-07");
});

// A room that starts yesterday reaches no further back than its beginning,
// which the menu already offers.
test("offers no day back where the room starts that day", () => {
  open("2026-11-07");
  expect(offered()).toEqual(["Today", "The very beginning", "Jump to a specific date"]);
});
