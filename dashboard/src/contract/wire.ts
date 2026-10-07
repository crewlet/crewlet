/**
 * The socket's own vocabulary: the push kinds a frame can carry, how much of
 * the activity feed a tab keeps, and the words a seat's state is served in.
 */

/**
 * Every `kind` a frame from `/ws/stream` carries — the pushes, the two answers
 * to a query, and the pong.
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
  | "result"
  | "error"
  | "pong";

/**
 * A live call's HEAVY fields, by the version that numbers each — the engine's
 * `livestate.CallDetail`, held there both ways by `internal/api/livestate`'s
 * `TestTheDashboardMergesEveryVersionedField`.
 *
 * An `agents` push carries these only when their version moved since the last
 * push for the same call — they move once a round or once a tool call, while
 * the row is pushed five times a second as a round streams — and names every
 * version always. The store keeps the copy it holds of a field the push left
 * out, and asks for the call whole (`live_call`) when a push names a version
 * newer than the one it holds: the push that moved it was dropped. A field
 * named here that the engine does not version is one a push never leaves out
 * and the merge waits on for ever; one the engine versions that is not named
 * here is a field the merge drops from the screen.
 */
export const LIVE_CALL_DETAIL = {
  prompt: ["prompt", "prompt_messages"],
  response: ["response"],
  narration: ["round_narration"],
  executions: ["tool_executions"],
  rounds: ["rounds"],
} as const;

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
 * How long a query waits for its answer ONCE SENT.
 *
 * The clock starts when the frame goes out, not when the query is made, so time
 * spent waiting for a socket is not counted against the server. Ten seconds is
 * far beyond the slowest query's normal latency and still short enough that a
 * screen shows an error rather than an eternal skeleton.
 *
 * AND LONGER THAN THE ENGINE'S OWN READ OF THE BROKER GROUP
 * (`engine.BrokerReadWait`), held there by `internal/api`'s
 * `TestTheDashboardWaitsPastAReadOfTheGroup`: the `fleet_broker` query asks a
 * member for the metadata group and may spend that long on it, and a screen
 * that gave up first would report a slow answer as a failed one.
 */
export const QUERY_TIMEOUT_MS = 10_000;

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
 * The longest phrase the engine's two ranked searches take — `work_search`,
 * `knowledge` and the `answer_knowledge` question — in UTF-8 bytes.
 *
 * EXACTLY THE ENGINE'S OWN (`knowledge.MaxQueryBytes`), held there by
 * `internal/api/queries`'s search gate: the engine refuses a longer phrase as
 * `bad_params` rather than cutting it, so a screen sends nothing past it and
 * says why — a search box that silently lost its results to a refusal reads as
 * a company with nothing written down.
 */
export const SEARCH_QUERY_MAX = 400;

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
