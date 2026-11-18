// @vitest-environment node

/**
 * A code search's answer as the Gitlet workspace draws it: its lines grouped
 * by repository and file in the engine's order, each line's address at the
 * commit its repository was searched at, and what was not searched in words.
 */

import { describe, expect, test } from "vitest";
import { coverageNotes, fileBlocks, groupHits, hitAt, matchingLines } from "./codeSearch.ts";

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
  // NEVER HIDDEN: an answer that searched everything still says how much.
  // Mutation: return no lead where nothing is missing, and this fails.
  test("is said quietly where every repository answered", () => {
    expect(coverageNotes(quiet())).toEqual({
      variant: "info",
      lead: "Searched 2 repositories.",
      notes: [],
    });
  });

  // A WARNING WHERE LINES MAY BE MISSING, each gap named with its
  // repositories. Mutation: drop a gap's note, or keep the tone at info, and
  // this fails.
  test("is a warning naming every repository left unanswered", () => {
    const notes = coverageNotes({
      ...quiet(),
      building: set(["web"]),
      not_answering: [{ node: "git-2", why: "timeout", repos: set(["api", "ops"], 5) }],
    });
    expect(notes.variant).toBe("warning");
    expect(notes.notes).toEqual([
      "Not searched: 1 repository (web), the Gitlet node asked was still building its index.",
      "Not searched: 5 repositories (api, ops and 3 more), Gitlet node git-2 did not answer (timeout).",
    ]);
  });

  test("is a warning where the lines were cut", () => {
    expect(coverageNotes({ ...quiet(), truncated: true }).variant).toBe("warning");
  });

  // PAST THE LIMIT IS SAID, beside the cut it comes with: a repository whose
  // every line falls after the last one shown is named, with how to reach it.
  // Mutation: drop the note, and this fails.
  test("names the repositories past the limit", () => {
    const notes = coverageNotes({ ...quiet(), truncated: true, past_limit: set(["zeta"], 4) });
    expect(notes.variant).toBe("warning");
    expect(notes.notes).toContain(
      "Past the limit: 4 repositories (zeta and 3 more), named after the last line shown. Narrow the search with repo: or path: to reach them.",
    );
  });

  // WHAT THE SEARCH'S OWN TERMS LEFT OUT IS SAID, AND IS NO WARNING: a grep
  // repository is searched only when named. Mutation: count it as missing.
  test("says what its own terms left out without a warning", () => {
    const notes = coverageNotes({ ...quiet(), grep_not_named: set(["scratch"]), skipped_files: 3 });
    expect(notes.variant).toBe("info");
    expect(notes.notes).toEqual([
      "Left out: 1 repository (scratch), searched only when a repo: filter names them.",
      "3 files passed over for their size or as binaries.",
    ]);
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
