// @vitest-environment node

/**
 * A code search's answer as the Gitlet workspace draws it: its lines grouped
 * by repository and file in the engine's order, each line's address at the
 * commit its repository was searched at, and what was not searched in words.
 */

import { describe, expect, test } from "vitest";
import {
  coverageNotes,
  facetsOf,
  fileBlocks,
  groupHits,
  hitAt,
  markedSegments,
  matchingLines,
  matchRanges,
  searchLine,
} from "./codeSearch.ts";

const TIP_WEB = "a".repeat(40);
const TIP_API = "b".repeat(40);
const set = (names: string[], count = names.length) => ({ count, names });
const quiet = () => ({
  searched: 2,
  not_indexed: set([]),
  building: set([]),
  not_answering: null,
  not_permitted: set([]),
  grep_not_named: set([]),
  stale: set([]),
  no_ref: set([]),
  past_limit: set([]),
  skipped_files: 0,
  truncated: false,
});
const hit = (repo: string, path: string, line: number, extra: Record<string, unknown> = {}) => ({
  repo,
  path,
  line,
  text: `line ${line}`,
  blob: `${repo}-${path}`.padEnd(40, "0"),
  ...extra,
});

describe("a search's lines", () => {
  // IN THE ENGINE'S ORDER: it merged them by repository, path and line, and a
  // group is drawn where its first line falls. Mutation: sort the groups by
  // name, or fold a file by its text when its bytes differ, and this fails.
  test("are grouped by repository, then file, in the order the engine answered", () => {
    const groups = groupHits({
      hits: [
        hit("web", "src/a.ts", 3),
        hit("web", "src/a.ts", 9),
        hit("web", "src/b.ts", 1),
        hit("api", "main.go", 4),
        hit("api", "x?", 2, { path_b64: "eP8=" }),
        hit("api", "x?", 5, { path_b64: "eP4=" }),
      ],
      tips: { web: TIP_WEB, api: TIP_API },
    });
    expect(groups.map((g) => [g.repo, g.tip, g.lines])).toEqual([
      ["web", TIP_WEB, 3],
      ["api", TIP_API, 3],
    ]);
    expect(groups[0]!.files.map((f) => [f.path, f.lines.map((l) => l.line)])).toEqual([
      ["src/a.ts", [3, 9]],
      ["src/b.ts", [1]],
    ]);
    // TWO PATHS THAT READ ALIKE AS TEXT ARE TWO FILES, by their bytes.
    expect(groups[1]!.files.map((f) => f.pathB64 ?? f.path)).toEqual(["main.go", "eP8=", "eP4="]);
  });

  test("are no groups at all where nothing matched", () => {
    expect(groupHits({ hits: [] })).toEqual([]);
  });
});

describe("a line's address", () => {
  // AT THE COMMIT ITS REPOSITORY WAS SEARCHED AT, never the branch, with the
  // line picked. Mutation: drop the ref, or the lines, and this fails.
  test("is the file on the Code tab at the searched commit, with its line picked", () => {
    const [group] = groupHits({ hits: [hit("web", "src/a.ts", 12)], tips: { web: TIP_WEB } });
    expect(hitAt(group!.tip, group!.files[0]!, 12)).toEqual({
      ref: TIP_WEB,
      path: "src/a.ts",
      lines: "12",
    });
  });

  test("is no address for a path that is not UTF-8", () => {
    const [group] = groupHits({
      hits: [hit("web", "x?", 1, { path_b64: "eP8=" })],
      tips: { web: TIP_WEB },
    });
    expect(hitAt(group!.tip, group!.files[0]!, 1)).toBeNull();
  });
});

describe("what a search covered", () => {
  // Mutation: return no lead where nothing is missing, and this fails.
  test("is said quietly where every repository answered", () => {
    const notes = coverageNotes(quiet());
    expect(notes.variant).toBe("info");
    expect(notes.notes).toEqual([]);
    expect(notes.lead).toEqual({ count: "", rest: "No line matched in 2 repositories", ref: "" });
  });

  // Mutation: drop a gap's note, or keep the tone at info, and this fails.
  test("is a warning naming every repository left unanswered", () => {
    const notes = coverageNotes({
      ...quiet(),
      building: set(["web", "api"]),
      not_answering: [{ node: "git-2", why: "timeout", repos: set(["api", "ops"], 5) }],
      not_permitted: set(["vault"]),
    });
    expect(notes.variant).toBe("warning");
    expect(notes.notes).toEqual([
      "Not searched: web and api, the Gitlet node asked was still building their index.",
      "Not searched: api, ops and 3 more, Gitlet node git-2 did not answer (timeout).",
      "Not searched: vault, a Gitlet node judged you may not read it now.",
    ]);
  });

  test("is a warning where the lines were cut", () => {
    const notes = coverageNotes({ ...quiet(), truncated: true });
    expect(notes.variant).toBe("warning");
    expect(notes.notes).toEqual([
      "Lines may be missing: the answer holds the most lines one search answers. Narrow it with repo: or path:.",
    ]);
  });

  // Lines of a repository past the limit may match and are not shown. Mutation: keep it quiet.
  test("names the repositories past the limit, as a warning", () => {
    const notes = coverageNotes({ ...quiet(), past_limit: set(["zeta"], 4) });
    expect(notes.variant).toBe("warning");
    expect(notes.notes).toEqual([
      "Past the limit: zeta and 3 more, named after the last line shown. Narrow the search with repo: or path: to reach them.",
    ]);
  });

  // A grep repository is searched only when named. Mutation: keep the old "Left out" sentence.
  test("says how to search a repository with no index, without a warning", () => {
    const words = (names: string[], count = names.length) =>
      coverageNotes({ ...quiet(), grep_not_named: set(names, count) }).notes;
    expect(words(["docs"])).toEqual(["docs is not indexed: name it with repo:docs to search it."]);
    expect(words(["docs", "wiki"])).toEqual([
      "docs and wiki are not indexed: name one with repo: to search it.",
    ]);
    expect(words(["docs", "wiki"], 5)).toEqual([
      "docs, wiki and 3 more are not indexed: name one with repo: to search it.",
    ]);
    expect(coverageNotes({ ...quiet(), grep_not_named: set(["docs"]) }).variant).toBe("info");
  });

  test("says what was read another way or passed over, quietly, in order", () => {
    const notes = coverageNotes(
      { ...quiet(), not_indexed: set(["web"]), no_ref: set(["api"]), skipped_files: 3 },
      [],
      "v2",
    );
    expect(notes.variant).toBe("info");
    expect(notes.notes).toEqual([
      "web was read with git grep: its index was not ready, so the same lines came more slowly.",
      "Nothing to search at v2 in api.",
      "3 files passed over for their size or as binaries.",
    ]);
  });
});

const total = (repo: string, path: string, lines: number[], all?: number) =>
  lines.map((line) => hit(repo, path, line, all === undefined ? {} : { file_matches: all }));

describe("the lead sentence", () => {
  const groups = groupHits({
    hits: [
      ...total("api", "x/a.go", [1, 2], 2),
      ...total("web", "x/b.go", [3], 5),
      ...total("web", "y/c.go", [4], 1),
    ],
    tips: {},
  });

  // Mutation: count the lines answered rather than each file's total.
  test("counts lines, files and repositories, on the default branches or at a ref", () => {
    const rest = " in 3 files across 2 repositories";
    expect(searchLine(groups, quiet(), "")).toEqual({ count: "8 matching lines", rest, ref: "" });
    expect(searchLine(groups, quiet(), "v2").ref).toBe("v2");
  });

  // Mutation: read an empty ref= as the default branches whatever the phrase names.
  test("names no revision where the phrase names one with ref:", () => {
    expect(searchLine(groups, quiet(), "", "Flush ref:v2").ref).toBeNull();
    expect(searchLine([], quiet(), "", "ref:main Flush").ref).toBeNull();
    expect(searchLine(groups, quiet(), "", "Flush pref:x").ref).toBe("");
    expect(searchLine(groups, quiet(), "v3", "Flush ref:v2").ref).toBe("v3");
  });

  // Mutation: say an exact count of a cut answer.
  test("says at least where the answer was cut or a total is missing", () => {
    expect(searchLine(groups, { ...quiet(), truncated: true }, "").count).toBe(
      "At least 8 matching lines",
    );
    const bare = groupHits({ hits: total("web", "a.go", [1, 2]), tips: {} });
    expect(searchLine(bare, quiet(), "").count).toBe("At least 2 matching lines");
  });
});

describe("the facets", () => {
  // Mutation: count the lines answered, or drop a repository's sum.
  test("count each repository and directory from the answer, the largest directory first", () => {
    const facets = facetsOf(
      groupHits({
        hits: [
          ...total("api", "x/a.go", [1], 2),
          ...total("web", "x/b.go", [3], 5),
          ...total("web", "y/z/c.go", [4], 1),
          ...total("web", "Makefile", [9], 4),
        ],
        tips: {},
      }),
      quiet(),
    );
    expect(facets).toEqual({
      all: 12,
      repos: [
        { key: "api", count: 2 },
        { key: "web", count: 10 },
      ],
      paths: [
        { key: "x", count: 7 },
        { key: "y/z", count: 1 },
      ],
      morePaths: 0,
      partial: null,
    });
  });

  // Mutation: list every directory.
  test("list the eight largest directories and count the rest", () => {
    const hits = Array.from({ length: 10 }, (_, i) =>
      total("web", `d${i}/f.go`, [1], 10 - i),
    ).flat();
    const facets = facetsOf(groupHits({ hits, tips: {} }), quiet());
    expect(facets.paths.map((p) => p.key)).toEqual([
      "d0",
      "d1",
      "d2",
      "d3",
      "d4",
      "d5",
      "d6",
      "d7",
    ]);
    expect(facets.morePaths).toBe(2);
  });

  // Mutation: mark them partial only on past_limit, or name every floor a cut.
  test("are partial where the answer was cut or a file's total was not answered, saying which", () => {
    const answered = groupHits({ hits: total("web", "x/a.go", [1], 1), tips: {} });
    expect(facetsOf(answered, quiet()).partial).toBeNull();
    expect(facetsOf(answered, { ...quiet(), truncated: true }).partial).toBe("cut");
    const bare = groupHits({ hits: total("web", "x/a.go", [1]), tips: {} });
    expect(facetsOf(bare, quiet()).partial).toBe("no total");
    expect(facetsOf(bare, { ...quiet(), truncated: true }).partial).toBe("cut");
  });
});

describe("a file's blocks", () => {
  const numbers = (blocks: ReturnType<typeof fileBlocks>) =>
    blocks.map((b) => b.rows.map((r) => (r.hit ? `${r.line}!` : r.line)));

  // Mutation: draw each hit alone, or number its before lines from the hit down.
  test("join a hit's lines around it and near hits into one run, with gaps between", () => {
    const blocks = fileBlocks([
      hit("web", "a.go", 5, { before: ["three", "four"], after: ["six"] }),
      hit("web", "a.go", 7, { after: ["eight"] }),
      hit("web", "a.go", 20, { before: ["nineteen"] }),
    ]);
    expect(numbers(blocks)).toEqual([
      [3, 4, "5!", 6, "7!", 8],
      [19, "20!"],
    ]);
    expect(blocks[0]!.rows[0]!.text).toBe("three");
    expect(blocks.map((b) => [b.first, b.last])).toEqual([
      [3, 8],
      [19, 20],
    ]);
  });

  // Mutation: let a later answer replace a row, or a context row replace a hit.
  test("keep the first answer of a line, except that a hit outranks a line around one", () => {
    const blocks = fileBlocks([
      hit("web", "a.go", 5, { after: ["six", "seven"] }),
      hit("web", "a.go", 9, { before: ["other seven", "eight"] }),
      hit("web", "a.go", 6, { text: "six, a hit" }),
    ]);
    expect(numbers(blocks)).toEqual([["5!", "6!", 7, 8, "9!"]]);
    expect(blocks[0]!.rows.map((r) => r.text)).toEqual([
      "line 5",
      "six, a hit",
      "seven",
      "eight",
      "line 9",
    ]);
  });

  test("take the lines a reader revealed, closing the gap they fill", () => {
    const hits = [hit("web", "a.go", 2), hit("web", "a.go", 6)];
    expect(numbers(fileBlocks(hits))).toEqual([["2!"], ["6!"]]);
    const revealed = new Map([
      [3, "three"],
      [4, "four"],
      [5, "five"],
    ]);
    expect(numbers(fileBlocks(hits, revealed))).toEqual([["2!", 3, 4, 5, "6!"]]);
  });
});

describe("a count of matching lines", () => {
  // Mutation: count the lines answered where a total is there, or call a guess exact.
  test("sums each file's total, else its lines answered, and says when it is a floor", () => {
    const [group] = groupHits({
      hits: [
        hit("web", "a.go", 1, { file_matches: 7 }),
        hit("web", "b.go", 2, { file_matches: 1 }),
      ],
    });
    expect(matchingLines(group!.files)).toEqual({ count: 8, exact: true });
    const [older] = groupHits({
      hits: [hit("web", "a.go", 1, { file_matches: 7 }), hit("web", "c.go", 3)],
    });
    expect(matchingLines(older!.files)).toEqual({ count: 8, exact: false });
  });
});

describe("where a line matched", () => {
  // Mutation: read the byte offsets as string indices, and the wide characters shift every mark.
  test("is each byte pair as the characters it covers", () => {
    expect(matchRanges("a Flush(", [[2, 8]])).toEqual([[2, 8]]);
    // "é" is two bytes and one index; "😀" is four bytes and two indices.
    expect(matchRanges("é Flush(", [[3, 9]])).toEqual([[2, 8]]);
    expect(matchRanges("😀 Flush(", [[5, 11]])).toEqual([[3, 9]]);
    expect(matchRanges("x", undefined)).toEqual([]);
  });

  // Mutation: throw on, or clamp, a pair that names no character's edge.
  test("drops a pair past the text, inside a character, empty or overlapping", () => {
    expect(matchRanges("é Flush(", [[1, 3]])).toEqual([]);
    expect(matchRanges("Flush(", [[0, 99]])).toEqual([]);
    expect(matchRanges("Flush(", [[2, 2]])).toEqual([]);
    expect(
      matchRanges("Flush(Flush(", [
        [0, 6],
        [3, 9],
        [6, 12],
      ]),
    ).toEqual([
      [0, 6],
      [6, 12],
    ]);
  });

  // Mutation: mark whole tokens, or lose a token's kind where a mark cuts it.
  test("cuts the line's tokens at each match's edges", () => {
    const tokens = [
      { end: 4, kind: "keyword" as const },
      { end: 5, kind: null },
      { end: 10, kind: "function" as const },
      { end: 11, kind: "punctuation" as const },
    ];
    expect(markedSegments("func Flush(", tokens, [[5, 11]])).toEqual([
      { end: 4, kind: "keyword", mark: -1 },
      { end: 5, kind: null, mark: -1 },
      { end: 10, kind: "function", mark: 0 },
      { end: 11, kind: "punctuation", mark: 0 },
    ]);
    expect(markedSegments("func Flush(", tokens, [[7, 9]])).toEqual([
      { end: 4, kind: "keyword", mark: -1 },
      { end: 5, kind: null, mark: -1 },
      { end: 7, kind: "function", mark: -1 },
      { end: 9, kind: "function", mark: 0 },
      { end: 10, kind: "function", mark: -1 },
      { end: 11, kind: "punctuation", mark: -1 },
    ]);
  });

  test("cuts plain text at the matches alone where there are no tokens", () => {
    expect(
      markedSegments("ab Flush( Flush(", null, [
        [3, 9],
        [10, 16],
      ]),
    ).toEqual([
      { end: 3, kind: null, mark: -1 },
      { end: 9, kind: null, mark: 0 },
      { end: 10, kind: null, mark: -1 },
      { end: 16, kind: null, mark: 1 },
    ]);
  });
});
