/**
 * A week's report, read as a report: its head (and, for a first week, that it
 * is one), the headline figures beside the week before, the week across its
 * days, what the week moved, what cost the most as rows opening each
 * finding's page, every other number with its change, the model's summary
 * where one was written, and the sources whose numbers are left out.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { AdvisorReport, AdvisorReportSummary } from "~/protocol/advisorFindings.ts";
import { ReportPage } from "./ReportPage.tsx";

afterEach(cleanup);

const REPORT: AdvisorReport = {
  week: "2026-W47",
  period: { from: "2026-11-16T00:00:00Z", to: "2026-11-23T00:00:00Z" },
  written_at: "2026-11-23T06:00:00Z",
  opened: 3,
  resolved: 1,
  applied: 1,
  dismissed: 0,
  headline: [],
  metrics: [
    { key: "turns", label: "Turns", value: 3_116, unit: "turns", was: 2_000 },
    {
      key: "turns_nothing",
      label: "Turns that committed nothing",
      value: 535,
      unit: "turns",
      was: 500,
    },
    { key: "tokens", label: "Tokens", value: 2_540_000_000, unit: "tokens", was: 2_000_000_000 },
    { key: "wait_h", label: "Hours work waited for a seat", value: 29, unit: "hours", was: 40 },
    { key: "held_h", label: "Hours work was held", value: 85, unit: "hours", was: 80 },
    {
      key: "round_caps",
      label: "Runs ended on their round cap",
      value: 3,
      unit: "count",
      was: null,
    },
  ],
  unread: [],
  top: [
    {
      id: "1f2e3d4c-5b6a-4978-8877-665544332211",
      class: "capacity.waiting",
      severity: "costs",
      title: "Work waits for Backend Engineer every day",
      subject: "Backend Engineer",
      item: "",
    },
  ],
  summary: null,
  days: [
    { day: "2026-11-16", turns: 400, turns_nothing: 60, tokens: 300_000_000 },
    { day: "2026-11-17", turns: 900, turns_nothing: 120, tokens: 800_000_000 },
  ],
};

function mount(answer: Partial<AdvisorReport> | Error, weeks: AdvisorReportSummary[] = []) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { name: string; params: Record<string, unknown> }[] = [];
  (
    socket as unknown as {
      query: (name: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (name, params) => {
    asked.push({ name, params: params ?? {} });
    if (name === "advisor_reports") return Promise.resolve({ reports: weeks, next_cursor: null });
    if (name !== "advisor_report") return Promise.resolve({});
    return answer instanceof Error
      ? Promise.reject(answer)
      : Promise.resolve({ ...REPORT, ...answer });
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <ReportPage week="2026-W47" />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

const tile = (scope: HTMLElement, key: string) =>
  scope.querySelector<HTMLElement>(`[data-stat="${key}"]`)!.textContent;

test("a report leads with its four figures beside the week before, and its top findings open their pages", async () => {
  const asked = mount({});
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  expect(asked.find((q) => q.name === "advisor_report")?.params).toEqual({ week: "2026-W47" });
  expect(within(article).getByRole("heading", { name: "Week 47 of 2026" })).toBeDefined();
  const figures = within(article).getByLabelText("The week's numbers");
  expect(tile(figures, "turns")).toMatch(/3,116.*turns.*\+56%/);
  expect(tile(figures, "tokens")).toMatch(/2\.54B.*tokens.*\+27%/);
  // THE SHARE THAT CHANGED NOTHING, its count beside it; fewer is the change wanted.
  expect(tile(figures, "changed_nothing")).toMatch(/17%.*−8 pts.*535 of 3,116 turns/);
  expect(tile(figures, "wait_h")).toMatch(/29.*h.*−28%/);
  const top = within(article).getByRole("link", {
    name: "Work waits for Backend Engineer every day",
  });
  expect(top.getAttribute("href")).toBe("#/advisor/findings/1f2e3d4c-5b6a-4978-8877-665544332211");
  // NOT A FIRST WEEK, no model summary, no source unread: none is drawn.
  expect(within(article).queryByText("First week measured")).toBeNull();
  expect(within(article).queryByText("The Advisor's summary")).toBeNull();
  expect(within(article).queryByText("Some numbers are left out")).toBeNull();
});

test("a report draws its week across its days, and what the week moved as a strip", async () => {
  mount({});
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  const charts = within(article).getByLabelText("The week by day");
  expect(within(charts).getByText("Turns each day")).toBeDefined();
  expect(within(charts).getByText("Tokens each day")).toBeDefined();
  expect(
    within(charts).getByText("The busiest day was Tue 17: 900 turns, 120 of them changed nothing."),
  ).toBeDefined();
  const moved = within(article).getByRole("region", { name: "What the week moved" });
  expect(
    within(moved)
      .getAllByRole("listitem")
      .map((li) => li.textContent),
  ).toEqual(["3 Findings opened", "1 Resolved", "1 Applied", "0 Dismissed"]);
});

// EVERY OTHER NUMBER is listed once, with its change, and never repeats a
// headline figure.
test("a report lists the rest of its numbers with their change", async () => {
  mount({});
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  const rest = within(article).getByRole("region", { name: "The week's other numbers" });
  expect(rest.textContent).toContain("Hours work was held");
  expect(rest.textContent).toContain("85 h+6%");
  expect(rest.textContent).toContain("Runs ended on their round cap3");
  expect(rest.textContent).not.toContain("Tokens");
});

// A FIRST WEEK IS SAID ONCE, never as a column of "not measured".
test("a first week says so once, and its figures carry no change", async () => {
  mount({ metrics: REPORT.metrics.map((m) => ({ ...m, was: null })) });
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  expect(within(article).getByText("First week measured")).toBeDefined();
  expect(within(article).queryByText(/Not measured/)).toBeNull();
  expect(within(article).queryByText("vs the week before")).toBeNull();
});

test("a report says which sources its numbers leave out, and draws the model's summary", async () => {
  mount({
    unread: ["ledger", "fires"],
    summary: "Waiting fell by a fifth after the second seat was added.",
  });
  expect(await screen.findByText("Some numbers are left out")).toBeDefined();
  expect(
    screen.getByText(/couldn't read the agents' turns and the week's schedule fires for this week/),
  ).toBeDefined();
  expect(screen.getByText("The Advisor's summary")).toBeDefined();
  expect(
    screen.getByText("Waiting fell by a fifth after the second seat was added."),
  ).toBeDefined();
});

// A REPORT WITH NOTHING TO DRAW still reads as one: what the week moved, and
// that nothing open was costing the company.
test("a report with no numbers, no days and no findings says what the week moved and that nothing cost", async () => {
  mount({ metrics: [], top: [], days: [] });
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  expect(within(article).queryByLabelText("The week's numbers")).toBeNull();
  expect(within(article).queryByLabelText("The week by day")).toBeNull();
  expect(within(article).getByText("What the week moved")).toBeDefined();
  expect(
    within(article).getByText("Nothing open was costing the company when this report was written."),
  ).toBeDefined();
});

// A FINDING ABOUT THE WHOLE COMPANY is named by the engine with its kind; the
// report says it in words, as the Inbox does.
test("a finding about the company as a whole reads as the company's", async () => {
  mount({
    top: [
      {
        id: "745ef3cd-7a51-5227-a907-3aa24c982011",
        class: "durability.no_verified_backup",
        severity: "blocks",
        title: "Your company has no recent verified backup.",
        subject: "company",
        item: "",
      },
    ],
  });
  expect(await screen.findByText("The company")).toBeDefined();
  expect(screen.queryByText("company")).toBeNull();
});

// THE WEEKS EITHER SIDE are a press away, from the list of weeks.
test("a report links the weeks either side of it", async () => {
  const week = (n: number): AdvisorReportSummary => ({
    week: `2026-W${n}`,
    period: { from: "2026-11-16T00:00:00Z", to: "2026-11-23T00:00:00Z" },
    written_at: "2026-11-23T06:00:00Z",
    opened: 0,
    resolved: 0,
    applied: 0,
    dismissed: 0,
    headline: [],
  });
  mount({}, [week(48), week(47), week(46)]);
  const weeks = await screen.findByRole("navigation", { name: "Other weeks" });
  expect(
    within(weeks)
      .getAllByRole("link")
      .map((a) => [a.textContent, a.getAttribute("href")]),
  ).toEqual([
    ["Week 46 of 2026", "#/advisor/reports/2026-W46"],
    ["Week 48 of 2026", "#/advisor/reports/2026-W48"],
  ]);
});
