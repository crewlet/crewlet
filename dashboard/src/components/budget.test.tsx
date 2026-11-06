/**
 * A seat's live token meters, worded as Company › Budgets words them.
 *
 * The meters printed the wire's own values: the window's key (`2026-W45`) in
 * each bar's name, and the refusal and the reset as ISO stamps in UTC
 * ("Last refused 2026-11-06T11:30:27.555936Z. Resets 2026-11-07T07:00:00Z."),
 * where Budgets said "Last refused a call 36m ago" of the same window.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { WindowMeters } from "./budget.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BudgetWindow } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";

afterEach(cleanup);

function mount(windows: BudgetWindow[]) {
  const store = new Store();
  store.applyOrg(withDerived({ timezone: "UTC", roles: [{ name: "Lead", handle: "lead" }] }));
  const socket = new LiveSocket(store);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <WindowMeters windows={windows} whose="Lead's" />
    </ClientContext.Provider>,
  );
}

// Mutation: print `w.refused_at` and `w.resets_at` as they came, or the
// window's key in the label, and these fail.
test("a full window says when it last refused and when it resets, in words", () => {
  const refused = new Date(Date.now() - 36 * 60_000).toISOString();
  const { container } = mount([
    {
      period: "day",
      window: "2026-11-06",
      starts_at: "2026-11-06T00:00:00Z",
      resets_at: "2026-11-07T00:00:00Z",
      used: 2_100_000,
      limit: 2_000_000,
      refused_at: refused,
      state: "refusing",
    },
    {
      period: "week",
      window: "2026-W45",
      starts_at: "2026-11-02T00:00:00Z",
      resets_at: "2026-11-09T00:00:00Z",
      used: 4_000_000,
      limit: 9_000_000,
      state: "ok",
    },
  ]);
  const text = container.textContent ?? "";
  expect(text).toContain("Last refused a call 36m ago. Resets today at midnight.");
  expect(text).not.toMatch(/\d{4}-\d\d-\d\dT/);
  expect(text).not.toContain("2026-W45");
  expect(text).toContain("Week 45");
  // The seat's name is heard, never drawn: the page is already the seat's.
  expect(screen.getByRole("meter", { name: /Today.*Lead's daily token budget/ })).toBeTruthy();
  expect(screen.getByRole("meter", { name: /This week.*Lead's weekly token budget/ })).toBeTruthy();
});
