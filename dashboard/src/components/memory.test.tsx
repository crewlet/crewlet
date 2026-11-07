import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { DiaryCard } from "./memory.tsx";
import type { AgentMemory } from "~/contract/memory.ts";

afterEach(cleanup);

const memory = {
  diary: Array.from({ length: 8 }, (_, i) => ({
    id: `d${i}`,
    content: `note ${i}`,
    retention: "diary_long",
    source: "tool:reflect_and_persist",
    turn_id: "",
    created_at: "2026-09-21T07:00:00Z",
    ttl_until: "",
    retrievals: 0,
  })),
  diary_total: 8,
} as AgentMemory;

// A DIARY'S OWN PAGE IS WHERE IT IS READ THROUGH: only a screen that names a
// cap cuts the card, so Doclet's diary still draws every entry it was sent.
test("a diary with no cap draws every entry and offers nothing to open", () => {
  render(<DiaryCard memory={memory} />);
  expect(screen.getAllByText(/^note \d$/)).toHaveLength(8);
  expect(screen.queryByRole("button")).toBeNull();
});

test("a diary with a cap draws that many and offers the rest", () => {
  render(<DiaryCard memory={memory} cap={5} />);
  expect(screen.getAllByText(/^note \d$/)).toHaveLength(5);
  expect(screen.getByRole("button", { name: "Show all 8" })).toBeTruthy();
});
