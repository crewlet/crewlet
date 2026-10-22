/**
 * The machine-readable codes a rejected query carries.
 *
 * The ENGINE's vocabulary plus the two the socket produces itself (`timeout`,
 * `closed`), held against every `Code…` constant `internal/api/stream` sends by
 * that package's vocabulary gate, in both directions: a code only the
 * dashboard knows is a branch that never runs, and a code only the engine
 * sends is a failure every screen renders as unknown. Screens compare an
 * error narrowed to this union, so a code outside it is a type error.
 */
export type QueryErrorCode =
  | "unknown_query"
  | "unauthorized"
  | "query_failed"
  /** This node understood the question and REFUSED it: a parameter missing,
   *  malformed, or outside the set the field accepts. The caller's fault, not
   *  the engine's — retrying sends the same bad request again. */
  | "bad_params"
  | "not_found"
  /** This node understood the question and cannot answer it YET: a
   *  projection still catching up after a restart or a fresh join, or a
   *  coordination store it could not reach. A screen asks again in a moment
   *  (`useQuery` does so itself), and never says "there is nothing": the
   *  second is an answer a person acts on. A source this node does not have
   *  at all is `unknown_query`, which waiting never changes. */
  | "unavailable"
  /** A question about the CONTENT of an artifact in the trash — its
   *  datastore, its room — asked by somebody it is disclosed to: the REST
   *  routes' `409 artifact_trashed`. Its owner's restore ends it, so a screen
   *  says the artifact is in the trash rather than that it does not exist
   *  (`not_found`, the answer for a reader it is not disclosed to). */
  | "artifact_trashed"
  | "timeout"
  | "closed";

/**
 * Every `error` a write through `POST /operator/act/{tool}` can come back
 * with, and the sentence the dashboard shows for it.
 *
 * EXACTLY THE ENGINE'S TWO SETS, held both ways by `internal/api/operator`'s
 * `TestTheDashboardKnowsExactlyTheActRefusals`: the transport's own codes
 * (`operator.ActTransportCodes` — the request guard's, the authority table's,
 * the operation key's, a body, a drain) and the tool refusal classes
 * (`mcp.Refusals` — what the tool itself refused, or a fault of the node it
 * met). One union because a caller branching on `error` must never need to
 * know which half a code came from, and the engine keeps them disjoint for
 * that reason. A code missing here is a refusal rendered as "something went
 * wrong"; one listed that nothing sends is a sentence nobody will ever read.
 *
 * NO SENTENCE MENTIONS A TOKEN. The dashboard holds no credential a script can
 * read — the browser's session cookie is the whole of it — so a person is
 * told to sign in again, never to set anything.
 *
 * THE SENTENCE IS SECOND PERSON, and it is ours, except where it is `null`,
 * which means "show the engine's own words". Those are the four classes whose
 * words say what to change (`invalid`), whom to ask (`forbidden`), the state
 * that refused the change (`conflict`: a room archived, an item in the trash,
 * or a write that lost its race and may be made again), or what is already
 * there (`exists`). The engine answers them as a `detail` worded for a person,
 * or, where nobody has worded that refusal for one yet, with no detail and its
 * code's own `message` (ADR-0060): never the tool's sentence, which is written
 * for a model and names the next call, its arguments and record ids. One fixed
 * sentence for every `conflict` ended "retry", which re-trying an archived
 * room's post never changes.
 */
export const ACT_ERRORS = {
  // Who is asking — the request guard's, before any tool is named.
  invalid_token: "Signed out. Sign in again.",
  identity_unavailable: "Couldn’t confirm who you are. Try again.",
  seat_unavailable: "Your seat was removed. Ask an admin to move you.",
  second_factor_enrolment_required: "Set up two-step verification first.",
  reader_changed:
    "Someone else is signed in here now, so nothing changed. Reload to continue as them.",
  csrf_origin:
    "The engine refused a change sent from another site. Make it from this page's own address.",
  // The authority table's, through the one refusal mapping.
  unauthorized: "You don’t have access to this.",
  step_up_required: "Confirm it’s you first.",
  // The transport's own.
  unknown_tool:
    "This engine does not support that change. It may be running a different version. Reload the page.",
  read_only_tool: "The dashboard sent a read as a change. Reload; if it persists, it is a bug.",
  unsupported_media_type: "The dashboard sent the change in a form the engine does not take.",
  op_id_invalid:
    "The dashboard sent the change without a usable operation key. Reload; if it persists, it is a bug.",
  invalid_input:
    "What this change was about moved since you opened it. Look again, then make the change.",
  invalid_body: "The dashboard sent a change the engine could not read.",
  body_too_large: "The change is larger than the engine accepts in one request.",
  unreadable_body: "The change could not be read in full by the engine.",
  no_active_revision:
    "This node isn’t running a company yet, so there’s nothing to change. Try again once it is.",
  draining: "This node is shutting down and takes no changes now. Try again in a moment.",
  starting: "This node is still starting and takes no changes yet. Try again in a moment.",
  // The tool's own refusal classes. `internal_error` is one of them — a fault
  // of the node, which no retry clears — and is also what a failure that
  // carried no class at all is answered with.
  invalid: null,
  not_found: "It is not there any more. Somebody may have removed or moved it.",
  forbidden: null,
  stale_version: "Changed by somebody else since you opened it. Look again, then retry.",
  conflict: null,
  exists: null,
  already_answered: "Somebody has already answered it.",
  reassignment_budget:
    "It has been handed between seats too many times. A person has to pick it up now.",
  inbox_full: "Your inbox holds too many marks. Mark everything read up to here first.",
  quota_exceeded:
    "This would pass a limit on what the company keeps. Remove what it no longer needs, or ask an admin to raise the limit.",
  notes_full:
    "The turn already holds as many notes as it can before it reads them. Send this again once it has read them, at its next step.",
  notes_spent:
    "The turn has taken as many notes as a turn takes, so sending this again changes nothing. Put the rest on the work item.",
  not_running: "It is not running any more.",
  budget_exhausted:
    "The token budget for this window is spent. Raise it, or wait for the window to reset.",
  unavailable: "This node could not make the change just now. Try again in a moment.",
  internal_error:
    "The change failed inside the engine, and trying again will not fix it. Its log has the reason; nothing was refused on purpose.",
} as const;
