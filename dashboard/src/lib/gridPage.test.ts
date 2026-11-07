import { expect, test } from "vitest";
import type { DataGridColumn } from "@crewlethq/ui";
import { sortGridRows } from "./gridPage.ts";

type Row = { name: string; n?: number };
const columns: DataGridColumn<Row>[] = [
  { key: "name", header: "Name", cell: (r) => r.name, sortValue: (r) => r.name },
  { key: "n", header: "N", cell: (r) => r.n, sortValue: (r) => r.n },
  { key: "plain", header: "Plain", cell: () => null },
];
const rows: Row[] = [{ name: "b10", n: 2 }, { name: "a" }, { name: "b9", n: 1 }];

test("a grid's order: numeric words, absent values last both ways, input untouched", () => {
  expect(sortGridRows(rows, columns, "name").map((r) => r.name)).toEqual(["a", "b9", "b10"]);
  expect(sortGridRows(rows, columns, "n").map((r) => r.name)).toEqual(["b9", "b10", "a"]);
  expect(sortGridRows(rows, columns, "-n").map((r) => r.name)).toEqual(["b10", "b9", "a"]);
  // A column with no sort value, or no sort, keeps the rows' own order.
  expect(sortGridRows(rows, columns, "plain")).toEqual(rows);
  expect(sortGridRows(rows, columns, "")).toEqual(rows);
  expect(rows.map((r) => r.name)).toEqual(["b10", "a", "b9"]);
});
