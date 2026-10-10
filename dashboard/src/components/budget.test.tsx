/**
 * A scope's live meters: one bar per capped window, and what a refusing one
 * says — every value as the Budgets screen says it, never as the engine sent
 * it.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { BudgetWindow } from "~/protocol/types.ts";
import { WindowMeters } from "./budget.tsx";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(Date.parse("2026-01-01T12:00:00Z"));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

/** A week cut in Tokyo, refusing: it turns over at Monday's midnight there,
 *  which is Sunday 4 January in UTC. */
const week: BudgetWindow = {
  period: "week",
  window: "2026-W01",
  starts_at: "2025-12-28T15:00:00Z",
  resets_at: "2026-01-04T15:00:00Z",
  used: 1_250_000,
  limit: 1_000_000,
  state: "refusing",
  refused_at: "2026-01-01T11:55:00Z",
};

// THE CAPTION PRINTED THE ENGINE'S OWN VALUES — "Last refused a call at
// 2026-01-01T11:55:00Z. Room comes back when 2026-W01 turns over at
// 2026-01-04T15:00:00Z" — on the seat page, one click from a screen that
// says the same window as "this week" and "Jan 5".
test("a refusing window says when, and when room comes back, on the company's calendar", () => {
  render(<WindowMeters windows={[week]} whose="Lead's" zone="Asia/Tokyo" />);
  expect(
    screen.getByText(
      "Last refused a call 5m ago. Room comes back when the week turns over on Jan 5, or when the ceiling is raised.",
    ),
  ).toBeTruthy();
});

test("a bar is named by whose ceiling and which window, never by the engine's label", () => {
  render(<WindowMeters windows={[week]} whose="Lead's" zone="Asia/Tokyo" />);
  const meter = screen.getByRole("meter", { name: "Lead's weekly token budget, Week 1" });
  expect(meter.getAttribute("aria-valuetext")).toBe("1,250,000 of 1,000,000 tokens this week");
  expect(document.body.textContent).not.toMatch(/2026-W01|T\d{2}:\d{2}/);
});
