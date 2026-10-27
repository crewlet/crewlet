/**
 * Comparing two versions of an artifact: which files changed between them,
 * and which of them a reader can compare line by line.
 *
 * PURE: the History panel's compare pane fetches the manifests and hands them
 * here, so what a comparison says is held by tests rather than by a screen.
 *
 * FILES ARE COMPARED BY DIGEST. A version names every file by its SHA-256, and
 * a file the engine carried from one version to the next keeps its digest, so
 * two files with one digest are the same bytes whatever else changed.
 *
 * A text file's LINES are compared by the dashboard's one line diff,
 * `lib/lineDiff.ts`, which every diff the dashboard draws compares with; what
 * stays here is how much of a file is read for it ([MAX_COMPARE_BYTES]).
 */

import type { ArtifactEntry, ArtifactManifest } from "~/protocol/index.ts";

/** What happened to one file between the two versions. */
export type FileChange =
  | { kind: "added"; path: string; to: ArtifactEntry }
  | { kind: "removed"; path: string; from: ArtifactEntry }
  | { kind: "changed"; path: string; from: ArtifactEntry; to: ArtifactEntry };

/** The files of two versions, compared. */
export interface FileComparison {
  /** Every file added, removed or changed, by path. */
  changes: FileChange[];
  /** How many files the two versions hold identically. */
  unchanged: number;
}

/** Compares the files of `from` (the older side) with those of `to`. */
export function compareFiles(from: ArtifactManifest, to: ArtifactManifest): FileComparison {
  const before = new Map(from.files.map((f) => [f.path, f]));
  const after = new Map(to.files.map((f) => [f.path, f]));
  const paths = [...new Set([...before.keys(), ...after.keys()])].sort();
  const changes: FileChange[] = [];
  let unchanged = 0;
  for (const path of paths) {
    const a = before.get(path);
    const b = after.get(path);
    if (a && b) {
      if (a.object.hash === b.object.hash) unchanged++;
      else changes.push({ kind: "changed", path, from: a, to: b });
    } else if (b) {
      changes.push({ kind: "added", path, to: b });
    } else if (a) {
      changes.push({ kind: "removed", path, from: a });
    }
  }
  return { changes, unchanged };
}

/**
 * Whether a file's served type is text a reader can compare line by line:
 * the text types and the structured text the web serves under application or
 * image types.
 */
export function isComparableText(contentType: string): boolean {
  const essence = (contentType.split(";")[0] ?? "").trim().toLowerCase();
  return (
    essence.startsWith("text/") ||
    [
      "application/json",
      "application/javascript",
      "application/xml",
      "application/manifest+json",
      "image/svg+xml",
    ].includes(essence)
  );
}

/** The most bytes a side of a line comparison is read for: a mebibyte. */
export const MAX_COMPARE_BYTES = 1 << 20;
