/**
 * Whether this browser may make one change to an artifact now, and why not.
 *
 * THE SAME ANSWER AND THE SAME WORDS as every other control (`useWriteAccess`):
 * offline, still checking who the reader is, signed out, missing the app, and
 * the reasons particular to an artifact: a role on it below the change's, and
 * a change only a person makes. A write control is never hidden; it is drawn
 * disabled with the sentence, and one the reader may press can still be
 * refused on its object, which the engine says in its own words.
 *
 * WHY NOT THE TOOL LIST: the artifacts' changes are the HTTP surface's
 * (`internal/api/artifactsapi`), decided on the artifact's owner and shares,
 * which no list of tools a viewer may call can say. So the parts that can be
 * known here are checked here (the link, the reader, the app, the role the
 * engine told this reader they hold, whether they are a person), and the
 * engine decides the rest.
 */

import type { ArtifactRole } from "~/protocol/index.ts";
import { holds, needsWords } from "../access.ts";
import { useConnection } from "../store-hooks.ts";
import { useViewer, type ViewerState } from "../viewer.ts";
import { WRITE_REASONS, linkOf, type Link, type WriteAccess } from "../useWriteAccess.ts";
import { ROLE_WORDS, roleIncludes } from "./words.ts";

/** What one change to an artifact needs. */
export interface ArtifactNeed {
  /** The artifacts app: creating, publishing and reshaping an artifact (D01). */
  app?: boolean;
  /** The least role on the artifact the change needs. */
  role?: ArtifactRole;
  /** The reader's own role on the artifact, as the engine answered it. */
  held?: ArtifactRole | "";
  /** Only a person makes it (the trash, a transfer, a forced publish). */
  person?: boolean;
  /** Administrators only (a purge). */
  admin?: boolean;
}

/** The sentence a change is held with when the reader's role is below the change's. */
export function roleReason(role: ArtifactRole): string {
  return `This needs the “${ROLE_WORDS[role]}” role on the artifact. Its owner can share it with you at that role.`;
}

/** The decision, over values: what the hook reads and what a test pins. */
export function artifactAccess(need: ArtifactNeed, viewer: ViewerState, link: Link): WriteAccess {
  if (link !== "open") return { can: false, block: link, reason: WRITE_REASONS[link] };
  if (viewer.loading) return { can: false, block: "loading", reason: WRITE_REASONS.loading };
  if (viewer.anonymous) return { can: false, block: "anonymous", reason: WRITE_REASONS.anonymous };
  if (need.admin && !viewer.admin) {
    return { can: false, block: "no_access", reason: needsWords("This change", ["admin"]) };
  }
  if (need.app && !holds(viewer.grants, "artifacts")) {
    return { can: false, block: "no_access", reason: needsWords("This change", ["artifacts"]) };
  }
  if (need.person && viewer.kind !== "human") {
    return { can: false, block: "not_served", reason: "Only a person can make this change." };
  }
  if (need.role && !roleIncludes(need.held, need.role)) {
    return { can: false, block: "not_served", reason: roleReason(need.role) };
  }
  return { can: true, as: viewer.owner || viewer.login, acts: viewer.acts };
}

/** Whether this browser may make a change needing `need` now. */
export function useArtifactAccess(need: ArtifactNeed): WriteAccess {
  const viewer = useViewer();
  const link = linkOf(useConnection());
  return artifactAccess(need, viewer, link);
}
