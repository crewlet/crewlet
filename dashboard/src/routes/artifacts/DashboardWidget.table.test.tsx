/**
 * A dashboard table on the kit's grid. The prose column takes the room the others
 * leave, every other column its content, so a narrow widget never cuts a key or a
 * date at its edge: a long value ends in an ellipsis with its whole words on hover,
 * and columns give way in order, with the foot saying which. jsdom runs no fit, so
 * the columns the grid is handed are what is asserted.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { DataGridColumn } from "@crewlethq/ui";

const handed: DataGridColumn<unknown>[][] = [];

vi.mock("@crewlethq/ui", async () => {
  const actual = await vi.importActual<typeof import("@crewlethq/ui")>("@crewlethq/ui");
  return {
    ...actual,
    DataGrid: (props: { columns: DataGridColumn<unknown>[] }) => {
      handed.push(props.columns);
      return null;
    },
  };
});
vi.mock("~/lib/artifacts/people.ts", () => ({ useSeatNames: () => (handle: string) => handle }));

const { DashboardWidget } = await import("./DashboardWidget.tsx");
const { ToastProvider } = await import("@crewlethq/ui");

afterEach(cleanup);

test("the prose takes the room and gives up its tail, and the rest shrink to their content and give way last to first", () => {
  const columns = [
    { key: "outcome", type: "text" as const },
    { key: "status", type: "text" as const, words: "tracker_status" as const },
    { key: "title", type: "text" as const },
    { key: "n", type: "number" as const },
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
          columns: ["outcome", "status", "title", "n"],
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
                n: 4,
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
  const cols = Object.fromEntries(handed.at(-1)!.map((c) => [c.key, c]));
  expect(cols.title?.floor).toBeDefined();
  expect(cols.title?.shrink).toBeUndefined();
  expect(cols.title?.drop).toBeUndefined();
  // THE FIRST COLUMN NAMES THE ROW: it never goes.
  expect(cols.outcome?.shrink).toBe(true);
  expect(cols.outcome?.drop).toBeUndefined();
  // The last column gives way before the one before it.
  expect(cols.n?.drop).toBeLessThan(cols.status?.drop ?? 0);
  expect(cols.n?.align).toBe("right");
  for (const c of handed.at(-1)!) {
    expect(c.overflow).toBe("truncate");
    expect(c.sortValue).toBeUndefined();
    expect(c.sortable).toBeUndefined();
  }
});
