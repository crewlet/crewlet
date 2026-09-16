/**
 * The per-seat budget table reads a node with no live meter for a seat as
 * absent, never as zero.
 *
 * The engine answers `live_used: null` for a seat this process has not run
 * (`internal/api/queries/budgets.go`), because a zero is a measurement.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type BudgetsAnswer } from "~/protocol/index.ts";
import { Spend } from "./Spend.tsx";

const budgets: BudgetsAnswer = {
  org: { max_tokens: 0, durable_used: 0, durable_updated_at: "", live_used: null },
  seats: [
    {
      role: "Unmetered",
      handle: "unmetered",
      agent_id: "a1",
      max_tokens: 0,
      durable_used: 10,
      durable_updated_at: "",
      live_used: null,
    },
    {
      role: "Busy",
      handle: "busy",
      agent_id: "a2",
      max_tokens: 0,
      durable_used: 5,
      durable_updated_at: "",
      live_used: 900,
    },
    {
      role: "Idle",
      handle: "idle",
      agent_id: "a3",
      max_tokens: 0,
      durable_used: 1,
      durable_updated_at: "",
      live_used: 0,
    },
  ],
  durable: true,
};

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("a seat with no live meter sorts below every measurement, zero included", async () => {
  location.hash = "#/spend";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "budgets" ? budgets : null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Spend />
      </Router>
    </ClientContext.Provider>,
  );

  await screen.findByText("Unmetered");
  const table = screen.getByText("Unmetered").closest("table")!;
  // The first press on a column sorts it descending.
  fireEvent.click(within(table).getByRole("button", { name: /This process/ }));
  const order = within(table)
    .getAllByRole("row")
    .slice(1)
    .map((row) => within(row).queryByText(/^(Unmetered|Busy|Idle)$/)?.textContent ?? "");
  expect(order).toEqual(["Busy", "Idle", "Unmetered"]);
  // And the absent meter is not drawn as a zero.
  const unmetered = within(table).getByText("Unmetered").closest("tr")!;
  expect(within(unmetered).queryByText("0")).toBeNull();
});

// A METER SAYS WHAT IT MEASURES. The bar this replaces was a `role="meter"`
// with its legend drawn as a sibling and nothing linking the two, so a screen
// reader announced "meter, 62 percent" with no idea of what, and read the raw
// number against the maximum where the value has a unit.
test("every budget meter names what it measures and says its value in words", async () => {
  location.hash = "#/spend";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "budgets"
        ? {
            ...budgets,
            seats: [{ ...budgets.seats[0]!, max_tokens: 1000, durable_used: 250 }],
          }
        : null,
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Spend />
      </Router>
    </ClientContext.Provider>,
  );

  const meter = await screen.findByRole("meter", { name: "Unmetered budget" });
  expect(meter.getAttribute("aria-valuetext")).toBe("250 / 1,000");
});

// AN ABSENCE SAYS WHAT IT IS. A seat with no budget drew a bare em dash in
// the headroom column, which a screen reader reads as "dash" or skips, so the
// one row in the table that cannot run out of budget was the one row that said
// nothing about itself.
test("a seat with no budget says so where the meter would be", async () => {
  location.hash = "#/spend";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "budgets" ? budgets : null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Spend />
      </Router>
    </ClientContext.Provider>,
  );

  await screen.findByText("Unmetered");
  const row = screen.getByText("Unmetered").closest("tr")!;
  expect(within(row).getByText("No budget set")).toBeDefined();
  expect(within(row).queryByRole("meter")).toBeNull();
});
