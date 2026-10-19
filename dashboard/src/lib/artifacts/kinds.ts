/**
 * The kind table, as screens ask it: one question per facet, answered from the
 * engine's own table (`ARTIFACT_KIND_SPECS`), so no screen switches on a
 * kind's name and a kind the engine grows is one every screen answers for.
 */

import { ARTIFACT_KIND_SPECS } from "~/contract/artifacts.ts";
import type { ArtifactDetail, ArtifactKind } from "~/protocol/index.ts";

/** Whether a kind runs author code in a sandboxed frame on the artifact origin. */
export function isFramed(kind: ArtifactKind): boolean {
  return ARTIFACT_KIND_SPECS[kind].render === "framed";
}

/** Whether a kind's content is its datastore, so a version holds no files to draw or compare. */
export function contentIsData(kind: ArtifactKind): boolean {
  return ARTIFACT_KIND_SPECS[kind].store === "content";
}

/**
 * Whether an artifact's data is live beside its versions, never versioned with
 * them: a store a page declared, or a native kind's own (a dashboard's).
 */
export function dataBesideVersions(kind: ArtifactKind): boolean {
  const store = ARTIFACT_KIND_SPECS[kind].store;
  return store === "declared" || store === "implied";
}

/**
 * Whether an artifact keeps a datastore a person can open: one that is its
 * content, one a native kind keeps beside its files, or one its page declared.
 */
export function keepsData(detail: Pick<ArtifactDetail, "kind" | "capabilities">): boolean {
  switch (ARTIFACT_KIND_SPECS[detail.kind].store) {
    case "content":
    case "implied":
      return true;
    case "declared":
      return (detail.capabilities ?? []).includes("db");
    case "none":
      return false;
  }
}
