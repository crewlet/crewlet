/**
 * A dashboard table's columns are HELD at what they hold. The design system's
 * table fits every column it was given only a default width for to its box,
 * which on a narrow widget cut headings to their first letters and broke
 * values mid-word; a held width it leaves alone, and the table scrolls. jsdom
 * runs no fit, so the columns the table is handed are what is asserted.
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

test("every column is held at the width its content needs, never fitted narrower", () => {
  const columns = [
    { key: "outcome", type: "text" as const },
    { key: "status", type: "text" as const, words: "tracker_status" as const },
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
          columns: ["outcome", "status"],
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
          artifact: "a",
          version: 1,
          dataset: "ds",
          status: "ready",
          table: { columns, rows: [{ outcome: "succeeded", status: "in_progress" }] },
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
  expect(cols.outcome?.width).toBe(112);
  expect(cols.status?.width).toBe(111);
  expect(cols.outcome?.defaultWidth).toBeUndefined();
});
