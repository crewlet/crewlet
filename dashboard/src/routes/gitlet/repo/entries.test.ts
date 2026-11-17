// @vitest-environment node
/** A directory's order: directories first, then by name case-folded, ties in git's order. */
import { expect, test } from "vitest";
import type { CodeTreeEntry } from "~/protocol/index.ts";
import { sortEntries } from "./entries.ts";

const entry = (path: string, type = "blob", oid = "a"): CodeTreeEntry => ({
  mode: type === "tree" ? "040000" : "100644",
  type,
  oid,
  path,
});

// Mutation: compare names as git does (byte order), or files before directories, and this fails.
test("directories come first, then names case-folded, and equal names keep git's order", () => {
  const sorted = sortEntries([
    entry("README.md"),
    entry("go.mod"),
    entry("Makefile"),
    entry("web", "tree"),
    entry(".github", "tree"),
    entry("docs/a", "blob", "first"),
    entry("x/A", "blob", "second"),
  ]);
  expect(sorted.map((e) => `${e.path}:${e.oid}`)).toEqual([
    ".github:a",
    "web:a",
    "docs/a:first",
    "x/A:second",
    "go.mod:a",
    "Makefile:a",
    "README.md:a",
  ]);
});
