/**
 * The socket's own vocabulary: the push kinds a frame can carry, how much of
 * the activity feed a tab keeps, and the words a seat's state is served in.
 */

/**
 * Every `kind` a frame from `/ws/stream` carries — the pushes, the one push
 * addressed to a SEAT rather than to every tab (`inbox_changed`, sent only to
 * a socket whose `watch` of that seat was allowed), the two answers to a
 * query, and the pong.
 *
 * EXACTLY THE ENGINE'S `stream.Kind` CONSTANTS, held both ways by
 * `internal/api/stream`'s push-kind gate. A kind the engine sends that this
 * union does not name is a frame `LiveSocket.onMessage` falls straight
 * through — silently, which is right for a newer peer's kind and wrong for
 * this build's own; one named here that the engine never sends is a dispatch
 * branch nothing can reach.
 */
export type PushKind =
  | "snapshot"
  | "event"
  | "agents"
  | "seats"
  | "sandboxes"
  | "tokens"
  | "budget"
  | "schedules"
  | "org"
  | "tools"
  | "health"
  | "inbox_changed"
  | "result"
  | "error"
  | "pong";

/**
 * Longest activity feed a tab keeps.
 *
 * EXACTLY THE SERVER'S OWN (`livestate.EventFeedLimit`), held there by
 * `internal/api/livestate`'s feed gate, so a reconnect's snapshot neither
 * truncates the feed nor leaves rows the server cannot resend. It is also the
 * limit of what anything derived from the feed can HONESTLY claim to know: a
 * busy company fills it in minutes, and a panel covering an hour has to say
 * where the record actually starts rather than drawing the gap as quiet.
 */
export const MAX_EVENTS = 400;

/**
 * The longest text `colleague{q}` resolves, in UTF-8 bytes.
 *
 * EXACTLY THE ENGINE'S OWN (`queries.ColleagueQueryMax`), held there by
 * `internal/api/queries`'s colleague gate: the engine refuses a longer `q`
 * as `bad_params`, so the command palette sends nothing past it rather than
 * losing the engine's name tiers to a refusal nobody sees.
 */
export const COLLEAGUE_QUERY_MAX = 200;

/**
 * What a seat is doing: the ONE seat-state vocabulary, served on every
 * `agents` row as `activity` and computed by the engine alone
 * (`internal/api/livestate/activity.go`) from the seat's turn, its coding
 * runs' durable record, its pause, its placement across the fleet and its
 * budget windows. A screen maps it to a tone and a label and derives nothing.
 *
 * EXACTLY THE ENGINE'S `livestate.Activities`, held both ways by
 * `internal/api/livestate`'s seat-state gate.
 */
export type SeatActivity = "working" | "needs" | "stopped" | "idle";

/**
 * Why a `stopped` seat cannot take work — `stopped_reason`, null on a seat that
 * is not stopped. EXACTLY THE ENGINE'S `livestate.StoppedReasons`, held by the
 * same gate.
 */
export type StoppedReason = "paused" | "unplaced" | "budget" | "provider";

/**
 * The tool an executor hands work to its workers through, and the argument
 * that lists the tasks it hands them.
 *
 * A seat whose running call — `live_call.running_call`, the one call in flight
 * — is this tool is running that many workers, which is what the state line
 * says ("3 workers on ENG-405") instead of the phase. EXACTLY THE ENGINE'S
 * `subagent.ToolName` and the `tasks` its schema requires, held by
 * `internal/agent/subagent`'s gate: a rename there would otherwise leave every
 * fan-out drawn as an ordinary executor with nothing to say it moved.
 */
export const DELEGATE_TOOL = "delegate";
export const DELEGATE_TASKS = "tasks";
