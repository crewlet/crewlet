/**
 * Settings › Limits draws the engine's answer and nothing of its own: each
 * group a table named by its card, each cap its name, its key, its value and
 * where the value comes from.
 *
 * Its own file because it mocks the screen's query hook.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { LimitsAnswer } from "~/contract/limits.ts";
import { Limits, limitSource } from "./Limits.tsx";

const answer = vi.hoisted(() => ({ data: null as unknown }));

vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({
    data: answer.data,
    loading: false,
    error: null,
    refusal: null,
    detail: null,
    refetch: () => {},
  }),
}));

afterEach(() => {
  cleanup();
  answer.data = null;
});

type Row = LimitsAnswer["limits"][number];

function row(over: Partial<Row> & Pick<Row, "key" | "group">): Row {
  return { unit: "count", value: 3, default: 3, off: false, default_off: false, ...over };
}

const LIMITS: LimitsAnswer = {
  limits: [
    row({ key: "turn_engine.max_iterations", group: "turn" }),
    row({ key: "turn_engine.onboarding_max_tool_rounds", group: "turn", value: 7, default: 10 }),
    row({
      key: "notification_rate_limit",
      group: "notifications",
      unit: "per_second",
      value: 0,
      default: 0,
      off: true,
      default_off: true,
    }),
    row({
      key: "schedules.timeout_seconds",
      group: "schedules",
      unit: "seconds",
      value: 180,
      default: 180,
    }),
  ],
};

test("draws each group as a table named by its card, in the engine's order", () => {
  answer.data = LIMITS;
  render(<Limits />);
  expect(screen.getAllByRole("table").map((t) => t.getAttribute("aria-labelledby"))).toEqual([
    "limits-turn",
    "limits-notifications",
    "limits-schedules",
  ]);
  expect(screen.getByRole("table", { name: "Guardrails on every turn" })).toBeDefined();
  expect(screen.getByRole("table", { name: "Schedules" })).toBeDefined();
});

test("each cap carries its name, its key, its value and its source", () => {
  answer.data = LIMITS;
  render(<Limits />);
  const turn = screen.getByRole("table", { name: "Guardrails on every turn" });
  const changed = within(turn).getByRole("row", { name: /Tool rounds on a first turn/ });
  expect(within(changed).getByText("turn_engine.onboarding_max_tool_rounds")).toBeDefined();
  expect(
    within(changed)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["7", "Changed from 10"]);
  // ONLY THE CHANGED ROW IS MARKED; a default says so in a word, never a tag.
  const resting = within(turn).getByRole("row", { name: /Review rounds per turn/ });
  expect(resting.hasAttribute("data-changed")).toBe(false);
  expect(changed.hasAttribute("data-changed")).toBe(true);
  expect(
    within(resting)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["3", "Default"]);
});

test("a cap whose value switches it off says Off, as a word", () => {
  answer.data = LIMITS;
  render(<Limits />);
  const wakeUps = screen.getByRole("row", { name: /Wake-ups per seat/ });
  expect(
    within(wakeUps)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["Off", "Default"]);
});

test("a set value names the default it replaced, Off where the default is no limit", () => {
  expect(
    limitSource(
      row({
        key: "notification_rate_limit",
        group: "notifications",
        unit: "per_second",
        value: 2,
        default: 0,
        default_off: true,
      }),
    ),
  ).toEqual({ changed: true, words: "Changed from Off" });
  expect(
    limitSource(
      row({
        key: "turn_engine.delegation.task_timeout_seconds",
        group: "delegation",
        unit: "seconds",
        value: 600,
        default: 300,
      }),
    ),
  ).toEqual({ changed: true, words: "Changed from 5 minutes" });
  expect(limitSource(row({ key: "x", group: "turn" }))).toEqual({
    changed: false,
    words: "Default",
  });
});

test("a node running no company says why there is nothing to show", () => {
  answer.data = { limits: [] };
  render(<Limits />);
  expect(screen.getByText("No limits to show")).toBeDefined();
  expect(screen.queryAllByRole("table")).toHaveLength(0);
});
