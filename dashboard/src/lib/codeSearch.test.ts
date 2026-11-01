// @vitest-environment node

/**
 * A code search's answer as the Code workspace draws it: its lines grouped
 * by repository and file in the engine's order, each line's address at the
 * commit its repository was searched at, and what was not searched in words.
 */

import { describe, expect, test } from "vitest";
import { coverageNotes, groupHits, hitAt } from "./codeSearch.ts";

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
  skipped_files: 0,
  truncated: false,
});
const hit = (repo: string, path: string, line: number, extra: Record<string, string> = {}) => ({
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
  test("is the file on Files at the searched commit, with its line picked", () => {
    const [group] = groupHits({ hits: [hit("web", "src/a.ts", 12)], tips: { web: TIP_WEB } });
    expect(hitAt(group!.tip, group!.files[0]!, 12)).toEqual({
      tab: "files",
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
      "Not searched: 1 repository (web), the git node asked was still building its index.",
      "Not searched: 5 repositories (api, ops and 3 more), git node git-2 did not answer (timeout).",
    ]);
  });

  test("is a warning where the lines were cut", () => {
    expect(coverageNotes({ ...quiet(), truncated: true }).variant).toBe("warning");
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
