/**
 * A week's report: its numbers beside the week before's, what the week moved,
 * the findings that cost the most as links to their pages, the model's summary
 * where one was written, and the sources whose numbers are left out.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { AdvisorReport } from "~/protocol/advisorFindings.ts";
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
  metrics: [
    { key: "wait_hours", label: "Hours ready work waited", value: 41.5, unit: "hours", was: 52 },
    { key: "failed_turns", label: "Turns that failed", value: 7, unit: "turns", was: null },
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
};

function mount(answer: Partial<AdvisorReport> | Error) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { name: string; params: Record<string, unknown> }[] = [];
  (
    socket as unknown as {
      query: (name: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (name, params) => {
    asked.push({ name, params: params ?? {} });
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

test("a report sets each number beside the week before's, and its top findings open their pages", async () => {
  const asked = mount({});
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  expect(asked.find((q) => q.name === "advisor_report")?.params).toEqual({ week: "2026-W47" });
  const table = within(article).getByRole("table");
  const rows = within(table)
    .getAllByRole("row")
    .slice(1)
    .map((r) =>
      within(r)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    );
  // A NUMBER THE WEEK BEFORE DID NOT MEASURE is said so, never drawn as a zero.
  expect(rows).toEqual([
    ["41.5 h", "52 h"],
    ["7 turns", "Not measured"],
  ]);
  const top = within(article).getByRole("link", {
    name: "Work waits for Backend Engineer every day",
  });
  expect(top.getAttribute("href")).toBe("#/advisor/findings/1f2e3d4c-5b6a-4978-8877-665544332211");
  expect(within(article).getByText("Findings opened")).toBeDefined();
  // NO MODEL WROTE A SUMMARY, and no source was left unread: neither is drawn.
  expect(within(article).queryByText("Some numbers are left out")).toBeNull();
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
  expect(
    screen.getByText("Waiting fell by a fifth after the second seat was added."),
  ).toBeDefined();
});

test("a report with no measures and no findings draws only what the week moved", async () => {
  mount({ metrics: [], top: [] });
  const article = await screen.findByRole("article", { name: "Week 47 of 2026" });
  expect(within(article).queryByRole("table")).toBeNull();
  expect(within(article).queryByText("What cost the most")).toBeNull();
  expect(within(article).getByText("What the week moved")).toBeDefined();
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
