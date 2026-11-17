// @vitest-environment node

import { expect, test } from "vitest";
import type { CodeRefListing, CodeTagRow } from "~/protocol/index.ts";
import { branchRows, foundTagRows, revSpelling, tagRows } from "./picker.ts";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 1 };
const branch = (name: string, at: string): CodeRefListing => ({
  ref: `refs/heads/${name}`,
  oid: "a".repeat(40),
  position: POSITION,
  moved_at: at,
});
const PAGE = [
  branch("feature", "2026-10-27T11:00:00Z"),
  branch("main", "2026-10-27T10:00:00Z"),
  branch("fix", "2026-10-27T09:00:00Z"),
];
const opts = { defaultBranch: "main", value: "", unset: false, finding: false };

// A BRANCH IS WRITTEN BY ITS NAME, the default as none, a tag by its whole ref.
// Mutation: write a tag's short name, and the engine reads a branch of that name.
test("a ref is spelled as the address carries it", () => {
  expect(revSpelling("refs/heads/feature", "main", false)).toBe("feature");
  expect(revSpelling("refs/heads/main", "main", false)).toBe("");
  expect(revSpelling("refs/heads/main", "main", true)).toBe("main");
  expect(revSpelling("refs/tags/v1", "main", false)).toBe("refs/tags/v1");
});

// THE DEFAULT IS PINNED FIRST and dropped from the page, never pinned under a
// find. Mutation: keep the page's order, or list the default twice.
test("the default branch is pinned first without a find, and in the page's place with one", () => {
  const pinned = PAGE[1]!;
  expect(branchRows(PAGE, pinned, opts).map((r) => r.name)).toEqual(["main", "feature", "fix"]);
  expect(branchRows(PAGE, pinned, { ...opts, finding: true }).map((r) => r.name)).toEqual([
    "feature",
    "main",
    "fix",
  ]);
  const later = branch("main", "2026-01-01T00:00:00Z");
  const page = [PAGE[0]!, PAGE[2]!];
  expect(branchRows(page, later, opts).map((r) => [r.name, r.at])).toEqual([
    ["main", "2026-01-01T00:00:00Z"],
    ["feature", "2026-10-27T11:00:00Z"],
    ["fix", "2026-10-27T09:00:00Z"],
  ]);
  expect(branchRows(page, null, opts).map((r) => r.name)).toEqual(["feature", "fix"]);
});

// THE CURRENT ROW IS THE ADDRESS'S: the default for none or its own name, a
// branch by its name, none for a whole commit.
test("the current row is the revision the address names", () => {
  const current = (value: string, unset = false) =>
    branchRows(PAGE, PAGE[1]!, { ...opts, value, unset })
      .filter((r) => r.current)
      .map((r) => r.name);
  expect(current("")).toEqual(["main"]);
  expect(current("main")).toEqual(["main"]);
  expect(current("fix")).toEqual(["fix"]);
  expect(current("b".repeat(40))).toEqual([]);
  expect(current("", true)).toEqual([]);
  expect(branchRows(PAGE, PAGE[1]!, { ...opts, unset: true })[0]!.rev).toBe("main");
});

const tag = (name: string, extra: Partial<CodeTagRow> = {}): CodeTagRow => ({
  ref: `refs/tags/${name}`,
  oid: "c".repeat(40),
  type: "commit",
  peeled: "c".repeat(40),
  peeled_type: "commit",
  ...extra,
});

// LATEST IS THE NEWEST PAGE'S FIRST ROW ALONE; a find's tags carry no date.
test("tags are written by their whole ref, Latest on the first only", () => {
  const rows = tagRows(
    [
      tag("v3", { date: "2026-10-28T07:00:00Z" }),
      tag("v2", { date_unix: 253402300801 }),
      tag("v1"),
    ],
    "refs/tags/v2",
  );
  expect(rows.map((r) => [r.name, r.rev, r.latest, r.current, r.late, r.undated])).toEqual([
    ["v3", "refs/tags/v3", true, false, false, false],
    ["v2", "refs/tags/v2", false, true, true, false],
    ["v1", "refs/tags/v1", false, false, false, true],
  ]);
  const found = foundTagRows([{ ref: "refs/tags/v3" }], "");
  expect(found).toEqual([
    {
      ref: "refs/tags/v3",
      name: "v3",
      rev: "refs/tags/v3",
      isDefault: false,
      latest: false,
      current: false,
    },
  ]);
});
