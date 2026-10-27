// @vitest-environment node
/** Comparing two versions: which files changed, and which of them compare line by line. */

import { describe, expect, test } from "vitest";
import type { ArtifactEntry, ArtifactManifest } from "~/protocol/index.ts";
import { compareFiles, isComparableText } from "./compare.ts";

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
