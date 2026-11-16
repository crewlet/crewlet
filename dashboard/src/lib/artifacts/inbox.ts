/** A comment on an artifact as the Inbox says it: why it reached the reader, what it did, and its
 *  thread's address. Records over the contract's unions, so a new reason needs words here. */

import { href } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import type { ArtifactCommentOp, ArtifactInboxReason } from "~/protocol/index.ts";

/** Why an artifact's comment reached the reader, in their own voice. */
const REASON_PHRASES: Readonly<Record<ArtifactInboxReason, string>> = {
  owner: "On your artifact",
  mention: "Mentioned you",
  participant: "In a thread you're in",
  assignee: "For your work item",
};

/** Why a notice reached the reader ([REASON_PHRASES]), or a newer reason's own words. */
export function artifactReasonPhrase(reason: string): string {
  return Object.hasOwn(REASON_PHRASES, reason)
    ? REASON_PHRASES[reason as ArtifactInboxReason]
    : reason.replace(/_/g, " ");
}

/** What a record did to its thread, as the words after its author's name. */
const OP_VERBS: Readonly<Record<ArtifactCommentOp, string>> = {
  comment_open: "commented",
  comment_reply: "commented",
  comment_resolve: "resolved a comment",
  comment_reopen: "reopened a comment",
};

/** What a record did to its thread ([OP_VERBS]), or a plain verb for a newer operation. */
export function artifactCommentVerb(op: string): string {
  return Object.hasOwn(OP_VERBS, op) ? OP_VERBS[op as ArtifactCommentOp] : "changed a comment";
}

/** The query value that opens an artifact on its comments. */
export const COMMENTS_PANEL = "comments";

/** An artifact's own page opened on one comment thread (`panel=comments`, `thread=`). */
export function artifactThreadHref(artifact: string, thread: string): string {
  return href(artifactPath(artifact), { panel: COMMENTS_PANEL, thread });
}
