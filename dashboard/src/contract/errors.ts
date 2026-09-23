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
