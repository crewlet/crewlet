/**
 * A ledger reads the same on every object: a time on the right, a sentence that
 * wraps and leads on a phone, plain heads over the engine's walk.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { DataGrid } from "@crewlethq/ui";
import { ledgerColumns } from "./ledgerGrid.tsx";

afterEach(cleanup);

const row = { at: "2026-10-18T10:00:00Z", what: "Saved it", who: "Ada", turn: "t-1" };

function draw(columns: Parameters<typeof ledgerColumns<typeof row>>[0]) {
  render(
    <DataGrid
      rows={[row]}
      rowKey={(r) => r.what}
      columns={ledgerColumns(columns, Date.parse(row.at))}
    />,
  );
  return screen.getByRole("table");
}

const facts = {
  at: (r: typeof row) => r.at,
  what: (r: typeof row) => r.what,
  who: (r: typeof row) => r.who,
};

test("its heads name the facts and none of them sorts, since the walk is the engine's", () => {
  const table = draw(facts);
  expect(
    within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["When", "What changed", "Who"]);
  expect(within(table).queryByRole("button")).toBeNull();
});

// PROSE THAT CUTS LOSES WHERE THE VALUE WENT, so the sentence wraps; a time
// stands on the right as every grid's does. Mutation: drop either column prop.
test("the time stands on the right and the sentence wraps", () => {
  const table = draw(facts);
  const [when, what] = within(table).getAllByRole("cell");
  expect(when?.classList.contains("crewlet-data-grid__cell--right")).toBe(true);
  expect(what?.classList.contains("crewlet-data-grid__cell--wrap")).toBe(true);
});

test("a turn and an action are columns only where the ledger has them", () => {
  const table = draw({ ...facts, turn: (r) => r.turn, action: () => <button>Undo</button> });
  expect(within(table).getByRole("link", { name: "Open" }).getAttribute("href")).toBe(
    "#/live/turns/t-1",
  );
  expect(within(table).getByRole("button", { name: "Undo" })).toBeTruthy();
});
