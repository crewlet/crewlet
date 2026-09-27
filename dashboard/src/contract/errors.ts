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
 * (`operator.ActTransportCodes` — a token, a body, a drain) and the tool
 * refusal classes (`mcp.Refusals` — what the tool itself refused). One union
 * because a caller branching on `error` must never need to know which half a
 * code came from, and the engine keeps them disjoint for that reason. A code
 * missing here is a refusal rendered as "something went wrong"; one listed
 * that nothing sends is a sentence nobody will ever read.
 *
 * THE SENTENCE IS SECOND PERSON, and it is ours — except where it is `null`,
 * which means "show the tool's own `detail`". Those are the two classes whose
 * sentence names the argument that was wrong (`invalid`) or the rule that
 * forbade it (`forbidden`); every other class's sentence is written for a
 * model reading a tool result, and a person reading it would be told about
 * `if_match` and record ids.
 */
export const ACT_ERRORS = {
  // The transport's own.
  invalid_token: "The engine refused your API token. Set it again to make changes.",
  unbound: "This token is not bound to a person, so there is nobody to record the change under.",
  unknown_tool:
    "This engine does not make that change. It may be running a different version from this page — reload.",
  read_only_tool: "The dashboard sent a read as a change. Reload; if it persists, it is a bug.",
  unsupported_media_type: "The dashboard sent the change in a form the engine does not take.",
  invalid_request_id: "The dashboard sent the change without a usable request id.",
  invalid_body: "The dashboard sent a change the engine could not read.",
  body_too_large: "The change is larger than the engine accepts in one request.",
  unreadable_body: "The change could not be read in full by the engine.",
  draining: "This node is shutting down and takes no changes now. Try again in a moment.",
  internal_error:
    "The change failed inside the engine. Its log has the reason; nothing was refused on purpose.",
  // The tool's own refusal classes.
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
} as const;
