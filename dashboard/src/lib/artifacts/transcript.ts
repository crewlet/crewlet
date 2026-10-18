/**
 * The artifact a turn's tool call wrote, as its transcript links it.
 *
 * A call to `create_artifact` or `publish_artifact` that landed answers the
 * artifact's id and the version's number (`ARTIFACT_PUBLISH_TOOLS` and its two
 * fields, held by running the tools), so the transcript can put a link to the
 * artifact beside the call rather than leave a reader to copy an id out of the
 * result. Anything else answers null: a failed call, another tool, an answer
 * that is not the engine's JSON, or an id that is not the canonical uuid the
 * engine mints, which is never made into a link.
 */

import {
  ARTIFACT_ANSWER_ID,
  ARTIFACT_ANSWER_VERSION,
  ARTIFACT_PUBLISH_TOOLS,
} from "~/contract/artifacts.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { isArtifactKind } from "./words.ts";

/** An artifact a tool call wrote, with what its arguments said about it. */
export interface WrittenArtifact {
  id: string;
  version: number;
  /** The title a create named; empty for a publish, whose arguments name only the artifact. */
  title: string;
  /** The kind a create named, or null. */
  kind: ArtifactKind | null;
}

/** The engine's canonical artifact id: lowercase, hyphenated, 36 characters (`artifacts.ParseID`). */
const CANONICAL_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** A JSON object out of a transcript's text, or null for anything else. */
function objectOf(text: string): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(text);
    return value !== null && typeof value === "object" && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : null;
  } catch {
    // A result that is not JSON is a tool's prose, which names no artifact.
    return null;
  }
}

/** Whether a tool's name is one whose answer is a version that landed. */
export function isPublishTool(name: string): boolean {
  return (ARTIFACT_PUBLISH_TOOLS as readonly string[]).includes(name);
}

/** The artifact call `name` wrote, read from its arguments and its result, or null. */
export function writtenArtifact(
  name: string,
  args: string,
  result: string,
  failed: boolean,
): WrittenArtifact | null {
  if (failed || !isPublishTool(name)) return null;
  const answer = objectOf(result);
  if (!answer) return null;
  const id = answer[ARTIFACT_ANSWER_ID];
  const version = answer[ARTIFACT_ANSWER_VERSION];
  if (typeof id !== "string" || !CANONICAL_ID.test(id)) return null;
  if (typeof version !== "number" || !Number.isInteger(version) || version < 1) return null;
  const asked = objectOf(args) ?? {};
  const title = typeof asked.title === "string" ? asked.title.trim() : "";
  const kind = typeof asked.kind === "string" && isArtifactKind(asked.kind) ? asked.kind : null;
  return { id, version, title, kind };
}
