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
 * THE SENTENCE IS SECOND PERSON, and it is ours — except where it is `null`,
 * which means "show the tool's own `detail`". Those are the two classes whose
 * sentence names the argument that was wrong (`invalid`) or the rule that
 * forbade it (`forbidden`); every other class's sentence is written for a
 * model reading a tool result, and a person reading it would be told about
 * `if_match` and record ids.
 */
export const ACT_ERRORS = {
  // Who is asking — the request guard's, before any tool is named.
  invalid_token: "Your sign-in has ended. Sign in again, then make the change.",
  identity_unavailable:
    "This node cannot confirm who you are just now. Try again in a moment; your sign-in is fine.",
  seat_unavailable:
    "The seat you act as is no longer in the org chart, so nothing can be made in its name.",
  second_factor_enrolment_required:
    "This deployment requires a second factor. Enrol one before you make changes.",
  csrf_origin:
    "The engine refused a change sent from another site. Make it from this page's own address.",
  // The authority table's, through the one refusal mapping.
  unauthorized: "You do not hold what this change needs.",
  step_up_required: "This change needs you to confirm who you are first.",
  // The transport's own.
  unknown_tool:
    "This engine does not make that change. It may be running a different version from this page — reload.",
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
    "This node has not been handed a company yet, so there is nothing to change. Try again once it has.",
  draining: "This node is shutting down and takes no changes now. Try again in a moment.",
  // The tool's own refusal classes. `internal_error` is one of them — a fault
  // of the node, which no retry clears — and is also what a failure that
  // carried no class at all is answered with.
  invalid: null,
  not_found: "It is not there any more. Somebody may have removed or moved it.",
  forbidden: null,
  stale_version: "Changed by somebody else since you opened it. Look again, then retry.",
  conflict: "It collided with another change landing at the same moment. Look again, then retry.",
  exists: "That already exists.",
  already_answered: "Somebody has already answered it.",
  reassignment_budget:
    "It has been handed between seats too many times. A person has to pick it up now.",
  inbox_full: "Your inbox holds too many marks. Mark everything read up to here first.",
  not_running: "It is not running any more.",
  steer_unsupported: "That turn cannot take a note: it runs in a coding agent's own loop.",
  budget_exhausted:
    "The token budget for this window is spent. Raise it, or wait for the window to reset.",
  unavailable: "This node could not make the change just now. Try again in a moment.",
  peer_upgrading: "The node that would make this change is mid-upgrade. Try again in a moment.",
  internal_error:
    "The change failed inside the engine, and trying again will not fix it. Its log has the reason; nothing was refused on purpose.",
} as const;
