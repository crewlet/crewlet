/**
 * A dashboard table's columns are HELD at what they hold. The design system's
 * table fits every column it was given only a default width for to its box,
 * which on a narrow widget cut headings to their first letters and broke
 * values mid-word; a held width it leaves alone. Only the prose column is
 * given a default width, so it alone takes or gives the room left. jsdom runs
 * no fit, so the columns the table is handed are what is asserted.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { DataTableColumn } from "@crewlethq/ui";

const handed: Record<string, DataTableColumn>[] = [];

vi.mock("@crewlethq/ui", async () => {
  const actual = await vi.importActual<typeof import("@crewlethq/ui")>("@crewlethq/ui");
  return {
    ...actual,
    DataTable: (props: { columns: Record<string, DataTableColumn> }) => {
      handed.push(props.columns);
      return null;
    },
  };
});

const { DashboardWidget } = await import("./DashboardWidget.tsx");
const { ToastProvider } = await import("@crewlethq/ui");

afterEach(cleanup);

test("every column is held at the width its content needs but the prose, which is fitted", () => {
  const columns = [
    { key: "outcome", type: "text" as const },
    { key: "status", type: "text" as const, words: "tracker_status" as const },
    { key: "title", type: "text" as const },
  ];
  render(
    <ToastProvider>
      <DashboardWidget
        boardTitle="Pulse"
        widget={{
          id: "t",
          type: "table",
          title: "Latest",
          dataset: "ds",
          columns: ["outcome", "status", "title"],
          w: 8,
        }}
        dataset={{
          id: "ds",
          title: "Deploys",
          source: "store",
          shape: { columns },
          freshness: { mode: "live", collection: "ds" },
        }}
        answer={{
          status: "ready",
          table: {
            columns,
            rows: [
              {
                outcome: "succeeded",
                status: "in_progress",
                title: "README and MVP acceptance run-through",
              },
            ],
          },
        }}
        status="ready"
        detail={undefined}
        asOf={undefined}
        onCite={undefined}
        onFocus={() => {}}
      />
    </ToastProvider>,
  );
  const cols = handed.at(-1)!;
  // Measured by the estimate (jsdom draws nothing): "succeeded" at 7.5px a
  // character, the cell's 28px and, as the first column, the frame's 4px more.
  expect(cols.outcome?.width).toBe(100);
  expect(cols.status?.width).toBe(111);
  expect(cols.outcome?.defaultWidth).toBeUndefined();
  expect(cols.status?.defaultWidth).toBeUndefined();
  // 37 characters at 7.5 px, the cell's 28 px and the last column's 4 px,
  // fitted no narrower than about twenty characters.
  expect(cols.title?.defaultWidth).toBe(310);
  expect(cols.title?.minWidth).toBe(150);
  expect(Object.values(cols).every((c) => c.sortable === false)).toBe(true);
  expect(cols.title?.width).toBeUndefined();
});
