// @vitest-environment node
/** Comparing two versions: which files changed, and which lines of a text file. */

import { describe, expect, test } from "vitest";
import type { ArtifactEntry, ArtifactManifest } from "~/protocol/index.ts";
import {
  MAX_COMPARE_LINES,
  compareFiles,
  compareLines,
  isComparableText,
  lines,
} from "./compare.ts";

function file(path: string, hash: string, size = 10): ArtifactEntry {
  return { path, content_type: "text/html", object: { hash, size } };
}

function manifest(...files: ArtifactEntry[]): ArtifactManifest {
  return { entry: "index.html", files };
}

describe("the files of two versions", () => {
  test("are compared by digest, path by path", () => {
    const got = compareFiles(
      manifest(file("index.html", "a"), file("app.js", "b"), file("old.css", "c")),
      manifest(file("index.html", "a"), file("app.js", "B"), file("new.css", "d")),
    );
    expect(got.unchanged).toBe(1);
    expect(got.changes.map((c) => `${c.kind}:${c.path}`)).toEqual([
      "changed:app.js",
      "added:new.css",
      "removed:old.css",
    ]);
  });

  test("of one version are identical", () => {
    const m = manifest(file("index.html", "a"));
    expect(compareFiles(m, m)).toEqual({ changes: [], unchanged: 1 });
  });
});

describe("a text type", () => {
  test("is the text and structured text a page serves", () => {
    for (const t of ["text/html; charset=utf-8", "text/css", "application/json", "image/svg+xml"]) {
      expect(isComparableText(t)).toBe(true);
    }
    for (const t of ["image/png", "font/woff2", "application/wasm", "application/pdf"]) {
      expect(isComparableText(t)).toBe(false);
    }
  });
});

describe("two texts line by line", () => {
  test("split on every line ending, a final newline ending the last line", () => {
    expect(lines("a\nb\r\nc\n")).toEqual(["a", "b", "c"]);
    expect(lines("")).toEqual([]);
    expect(lines("one")).toEqual(["one"]);
  });

  test("identical have no hunks", () => {
    expect(compareLines("a\nb\n", "a\nb\n")).toEqual({ kind: "hunks", hunks: [] });
  });

  test("name a changed line where both versions hold it", () => {
    expect(compareLines("a\nb\nc\n", "a\nB\nc\n")).toEqual({
      kind: "hunks",
      hunks: [{ fromStart: 2, from: ["b"], toStart: 2, to: ["B"] }],
    });
  });

  test("name an addition and a removal apart", () => {
    const got = compareLines("a\nb\nc\nd\n", "a\nc\nd\ne\n");
    expect(got).toEqual({
      kind: "hunks",
      hunks: [
        { fromStart: 2, from: ["b"], toStart: 2, to: [] },
        { fromStart: 5, from: [], toStart: 4, to: ["e"] },
      ],
    });
  });

  test("are the shortest edit between them, whatever the texts", () => {
    let seed = 7;
    const rand = () => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed / 2147483648;
    };
    for (let round = 0; round < 200; round++) {
      const a = Array.from(
        { length: Math.floor(rand() * 30) },
        () => "abcde"[Math.floor(rand() * 5)] ?? "a",
      );
      const b = Array.from(
        { length: Math.floor(rand() * 30) },
        () => "abcde"[Math.floor(rand() * 5)] ?? "a",
      );
      const got = compareLines(a.join("\n"), b.join("\n"));
      if (got.kind !== "hunks") throw new Error(`round ${round}: ${got.kind}`);
      // REPLAYING THE HUNKS on the older text gives the newer one.
      const replayed: string[] = [];
      let at = 0;
      for (const h of got.hunks) {
        const start = h.fromStart - 1;
        replayed.push(...a.slice(at, start));
        replayed.push(...h.to);
        at = start + h.from.length;
      }
      replayed.push(...a.slice(at));
      expect(replayed).toEqual(b);
      // AND IT IS THE SHORTEST: never more edits than the longest common
      // subsequence allows.
      const edits = got.hunks.reduce((n, h) => n + h.from.length + h.to.length, 0);
      expect(edits).toBe(a.length + b.length - 2 * lcs(a, b));
    }
  });

  test("past the line bound are not compared", () => {
    const big = Array.from({ length: MAX_COMPARE_LINES + 1 }, (_, i) => String(i)).join("\n");
    expect(compareLines(big, "x")).toEqual({ kind: "too-large", lines: MAX_COMPARE_LINES + 1 });
  });

  test("with nothing in common past the edit bound are called too different", () => {
    const a = Array.from({ length: 3000 }, (_, i) => `a${i}`).join("\n");
    const b = Array.from({ length: 3000 }, (_, i) => `b${i}`).join("\n");
    expect(compareLines(a, b)).toEqual({ kind: "too-different" });
  });
});

/** The length of the longest common subsequence, by the textbook table. */
function lcs(a: string[], b: string[]): number {
  const t = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = 1; i <= a.length; i++) {
    for (let j = 1; j <= b.length; j++) {
      const row = t[i] ?? [];
      row[j] =
        a[i - 1] === b[j - 1]
          ? (t[i - 1]?.[j - 1] ?? 0) + 1
          : Math.max(t[i - 1]?.[j] ?? 0, row[j - 1] ?? 0);
    }
  }
  return t[a.length]?.[b.length] ?? 0;
}
