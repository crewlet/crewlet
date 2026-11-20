/**
 * The weekly reports as a timeline of weeks: a card each, the newest first,
 * each its days, when it was written and four figures beside the week
 * before, opening its week's report; older weeks a press away by the
 * engine's cursor; and no report yet said as such.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { AdvisorMetric, AdvisorReportSummary } from "~/protocol/advisorFindings.ts";
import { Reports } from "./Reports.tsx";

afterEach(cleanup);

const metric = (key: string, value: number, unit: string, was: number | null): AdvisorMetric => ({
  key,
  label: key,
  value,
  unit,
  was,
});

/** Week n of 2026, from its Monday, its figures beside the week before's unless `first`. */
function week(n: number, patch: Partial<AdvisorReportSummary> = {}, first = false) {
  const monday = Date.UTC(2025, 11, 29) + (n - 1) * 7 * 86_400_000;
  const was = (v: number) => (first ? null : v);
  return {
    week: `2026-W${n}`,
    period: {
      from: new Date(monday).toISOString().replace(".000", ""),
      to: new Date(monday + 7 * 86_400_000).toISOString().replace(".000", ""),
    },
    written_at: new Date(Date.now() - 86_400_000).toISOString(),
    opened: 2,
    resolved: 1,
    applied: 0,
    dismissed: 0,
    headline: [
      metric("turns", 3_116, "turns", was(2_000)),
      metric("turns_nothing", 535, "turns", was(500)),
      metric("tokens", 2_540_000_000, "tokens", was(2_000_000_000)),
    ],
    ...patch,
  } satisfies AdvisorReportSummary;
}

function mount(
  pages: (cursor: unknown) => { reports: AdvisorReportSummary[]; next_cursor: string | null },
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (
    socket as unknown as {
      query: (name: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (name, params) => {
    if (name !== "advisor_reports") return Promise.resolve({});
    asked.push(params ?? {});
    return Promise.resolve(pages(params?.cursor));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <Reports />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

test("each week is a card, the newest first, its figures beside the week before", async () => {
  mount(() => ({ reports: [week(47, { opened: 3 }), week(46)], next_cursor: null }));
  const list = await screen.findByRole("list", { name: "Weekly reports" });
  const cards = within(list).getAllByRole("link");
  expect(cards.map((a) => [a.getAttribute("aria-label"), a.getAttribute("href")])).toEqual([
    ["Week 47 of 2026, Nov 16 to 22, 2026", "#/advisor/reports/2026-W47"],
    ["Week 46 of 2026, Nov 9 to 15, 2026", "#/advisor/reports/2026-W46"],
  ]);
  const newest = cards[0]!;
  expect(within(newest).getByText("Week 47")).toBeDefined();
  expect(within(newest).getByText("Written yesterday")).toBeDefined();
  const figure = (key: string) => newest.querySelector(`[data-stat="${key}"]`)!.textContent;
  expect(figure("turns")).toMatch(/3,116.*turns.*\+56%/);
  expect(figure("tokens")).toMatch(/2\.54B.*tokens/);
  expect(figure("changed_nothing")).toMatch(/17%.*535 of 3,116 turns/);
  // THE FINDINGS IT OPENED beside the week before's, which the list holds.
  expect(figure("opened")).toMatch(/3.*\+50%.*1 resolved · 0 applied · 0 dismissed/);
});

// A FIRST WEEK SAYS SO ONCE, in its head, and carries no change.
test("the first week measured says so", async () => {
  mount(() => ({ reports: [week(46, {}, true)], next_cursor: null }));
  const list = await screen.findByRole("list", { name: "Weekly reports" });
  expect(within(list).getByText("First week measured")).toBeDefined();
  expect(within(list).queryByText("vs the week before")).toBeNull();
});

// OLDER WEEKS ARE ASKED FOR by the cursor the engine answered, and the weeks
// already shown stay, the older after them.
test("older weeks are asked for by the engine's cursor", async () => {
  const asked = mount((cursor) =>
    cursor === "c2"
      ? { reports: [week(45)], next_cursor: null }
      : { reports: [week(47), week(46)], next_cursor: "c2" },
  );
  const list = await screen.findByRole("list", { name: "Weekly reports" });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Show older reports" }));
  });
  await within(list).findByText("Week 45");
  expect(
    within(list)
      .getAllByRole("heading")
      .map((h) => h.textContent),
  ).toEqual(["Week 47", "Week 46", "Week 45"]);
  expect(asked.map((q) => q.cursor)).toEqual([undefined, "c2"]);
  expect(screen.queryByRole("button", { name: "Show older reports" })).toBeNull();
});

test("no report yet is said, with when one is written", async () => {
  mount(() => ({ reports: [], next_cursor: null }));
  expect(await screen.findByText("No report yet")).toBeDefined();
  expect(screen.getByText(/writes a report every Monday morning/)).toBeDefined();
});
