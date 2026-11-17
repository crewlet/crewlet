import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { codeRest } from "~/lib/useCodeQuery.ts";
import type { CodeGitAnswer, CodeTreeEntry } from "~/protocol/index.ts";
import {
  TREE_READS_AT_ONCE,
  ancestorsOf,
  forgetTrees,
  treeForest,
  useRepoTree,
  type Level,
} from "./tree.ts";

const A = "0123456789abcdef0123456789abcdef01234567";
const B = "89abcdef0123456789abcdef0123456789abcdef";

afterEach(() => {
  forgetTrees();
  vi.restoreAllMocks();
});

const entry = (path: string, type = "blob", extra: Partial<CodeTreeEntry> = {}): CodeTreeEntry =>
  ({ path, type, mode: type === "tree" ? "040000" : "100644", oid: A, ...extra }) as CodeTreeEntry;

const read = (entries: CodeTreeEntry[], extra: Partial<Level> = {}): Level => ({
  entries,
  total: entries.length,
  next: "",
  state: "read",
  refusal: null,
  paging: false,
  pageFailed: false,
  ...extra,
});

const labels = (forest: ReturnType<typeof treeForest>) => {
  const out: string[] = [];
  const walk = (nodes: typeof forest.roots, depth: number) => {
    for (const n of nodes) {
      out.push(`${"  ".repeat(depth)}${n.label}`);
      walk(n.children ?? [], depth + 1);
    }
  };
  walk(forest.roots, 0);
  return out;
};

// A WHOLE DIRECTORY IS A CODE HOST'S ORDER, a paged one git's with Load more, an
// unread one a placeholder the kit can open. Mutation: sort a paged level, or
// drop the placeholder, and this fails.
test("the forest orders whole levels, keeps git's order while paged and holds a place for unread ones", () => {
  const levels = new Map<string, Level>([
    ["", read([entry("Makefile"), entry("cmd", "tree"), entry("a.go"), entry("lib", "tree")])],
    ["cmd", read([entry("cmd/z.go"), entry("cmd/a", "tree")], { total: 9, next: "p2" })],
  ]);
  const forest = treeForest(levels);
  expect(labels(forest)).toEqual([
    "cmd",
    "  z.go",
    "  a",
    "    Loading a",
    "  Load more in cmd",
    "lib",
    "  Loading lib",
    "a.go",
    "Makefile",
  ]);
  expect(forest.nodes.get("cmd//more")?.kind).toBe("more");
  expect(forest.nodes.get("lib//loading")?.kind).toBe("loading");
});

// A SUBMODULE AND A PATH GIT CANNOT SPELL ARE LISTED, never links. Mutation: make
// either a file, and this fails.
test("a submodule and a path git cannot spell are listed as inert", () => {
  const forest = treeForest(
    new Map([
      [
        "",
        read([
          entry("vendor", "commit", { mode: "160000" }),
          entry("bad", "blob", { path_b64: "YmFk" }),
        ]),
      ],
    ]),
  );
  expect([...forest.nodes.values()].map((n) => n.kind)).toEqual(["inert", "inert"]);
  expect(forest.nodes.has("//b64/YmFk")).toBe(true);
});

test("a path's ancestors are its directories from the root", () => {
  expect(ancestorsOf("cmd/web/main.go")).toEqual(["cmd", "cmd/web"]);
  expect(ancestorsOf("go.mod")).toEqual([]);
});

/** Every code_tree read, each answered when its own `answer` is called. */
function held() {
  const asks: { path: string; ref: string; signal?: AbortSignal; answer: () => void }[] = [];
  vi.spyOn(codeRest, "query").mockImplementation(
    ((_what: string, params: Record<string, string>, signal?: AbortSignal) =>
      new Promise<CodeGitAnswer>((resolve) => {
        asks.push({
          path: params.path ?? "",
          ref: params.ref ?? "",
          signal,
          answer: () => resolve({ tree: { entries: [], total: 0 } } as unknown as CodeGitAnswer),
        });
      })) as typeof codeRest.query,
  );
  return asks;
}

// ONE READ A DIRECTORY A COMMIT, at most four at once, kept for the visit and
// aborted when its last reader leaves. Mutation: drop the in-flight bound, or the
// kept map, and this fails.
test("each directory is read once a commit, four at a time, and kept across readers", async () => {
  const asks = held();
  const dirs = ["", "a", "b", "c", "d", "e"];
  const first = renderHook(({ commit }) => useRepoTree("web", commit, dirs), {
    initialProps: { commit: A },
  });
  expect(asks.map((a) => a.path)).toEqual(dirs.slice(0, TREE_READS_AT_ONCE));
  expect(asks.every((a) => a.ref === A)).toBe(true);
  await act(async () => asks[0]!.answer());
  expect(asks.map((a) => a.path)).toEqual(dirs.slice(0, TREE_READS_AT_ONCE + 1));
  for (const a of asks.slice(1)) await act(async () => a.answer());
  await act(async () => asks[asks.length - 1]!.answer());
  expect(asks).toHaveLength(dirs.length);

  const again = renderHook(() => useRepoTree("web", A, dirs));
  expect(asks).toHaveLength(dirs.length);
  expect(again.result.current.levels.get("e")?.state).toBe("read");
  again.unmount();

  first.rerender({ commit: B });
  const atB = asks.slice(dirs.length);
  expect(atB.map((a) => a.ref)).toEqual(Array(TREE_READS_AT_ONCE).fill(B));
  first.unmount();
  expect(atB.every((a) => a.signal?.aborted)).toBe(true);
});
