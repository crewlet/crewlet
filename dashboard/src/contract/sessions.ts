/**
 * A seat's executor sessions, as `executor_sessions` and `executor_session`
 * send them: every turn whose executor captured its harness's session, or that
 * read its conversation as its first launch began — how the turn ended, what it
 * was asked, whether its session is still kept, and which launch would
 * continue it — and one session's transcript, a page at a time.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS, held there by
 * `TestTheSessionsScreenReadsWhatTheAnswersSend` in both directions: every
 * member declared here is a key an answer carries, every key an answer carries
 * is declared, the six unions are the engine's own closed sets — so a screen
 * keyed by them (`routes/agents/seat/Sessions.tsx`) has a word for every value
 * the engine sends — and a schedule's key is known by the engine's own prefix.
 *
 * Every text a harness, a model or a person wrote — an entry's text, the ask's
 * excerpt, an ending's prose — reaches the client REDACTED: the engine runs its
 * one redaction pass over each at the read, whatever the capture did.
 */

import type { LogPosition } from "./positions.ts";

/**
 * What a schedule's conversation key starts with (`notify.SchedulePrefix`): a
 * schedule is a conversation of each seat that runs it, keyed this, then the
 * schedule's scope, the scope's id and its name, each escaped as a URL path
 * segment (`notify.ScheduleConversation`) — which a row prints decoded.
 */
export const SCHEDULE_CONVERSATION_PREFIX = "schedule/";

/** How a turn ended, in the engine's four words (`executorkeep.Outcomes`). */
export type SessionOutcome = "completed" | "failed" | "stopped" | "lost";

/**
 * Where a launch would continue a turn's session (`sandbox.ContinuesIns`): in
 * whichever box the launch gets — a relaunch from the turn's kept capture, or
 * the conversation's next turn from its head; or, a relaunch's alone, only in
 * the box the turn's run parked in, while that box is held.
 */
export type SessionContinuesIn = "any_box" | "parked_box";

/**
 * Which launch would continue a turn's session (`sandbox.ContinuedBys`): a
 * relaunch of the turn, while it is live; or its conversation's next turn, once
 * it has ended — or is ending, its end not yet on the log — which continues only
 * the conversation's head, the newest session in its line; or, for a turn of a
 * schedule's conversation, the schedule's next fire, the same launch told in
 * the schedule's words — a schedule is a conversation of each seat that runs
 * it, every fire a turn of it.
 */
export type SessionContinuedBy = "relaunch" | "next_turn" | "next_fire";

/**
 * A capture of a turn's session no row names (`executorkeep.Unkept`): its
 * place among the turn's captures, and why it was not kept — the engine's
 * words, which quote what refused it, redacted.
 */
interface SessionUnkept {
  launch_seq: number;
  capture_seq: number;
  reason: string;
}

/**
 * The engine's account of how a turn ended (`executorkeep.Ending`): what a
 * harness's transcript cannot know. Every member but the outcome is sent only
 * where the way the turn ended establishes it.
 */
interface SessionEnding {
  outcome: SessionOutcome;
  /** The reviewer's final decision word. */
  decision?: string;
  /** The turn's reply reached the party waiting on it. */
  delivered?: boolean;
  /** The surface the party waiting on the turn waits on: where its reply was
   *  owed, whether or not it went. */
  delivered_to?: string;
  /** Whether the reply went is not known: the turn's record of its calls was
   *  cut to stay bounded, and the calls it kept show no delivery. Never sent
   *  beside `delivered`. */
  delivery_unknown?: boolean;
  /** The engine's own outcome of a colleague's ask the turn answered. */
  answer?: string;
  /** The reviewer's account of what landed — a model's words. */
  completed_work?: string;
  /** The executor's own evidence of what stopped a blocked round. */
  blocked_on?: string;
  /** Who stopped the turn, and the reason they gave in their own words. */
  stopped_by?: string;
  stop_reason?: string;
  /** The engine's account of the guard, the budget or the loss that ended it. */
  failure?: string;
  /** The turn's newest capture of its session, where it is newer than the
   *  one its row names: the row then names an older one its turn went on
   *  past (`went_on`), or none of the turn's. */
  unkept?: SessionUnkept;
  /** The turn went on past the session its row names, which was kept while
   *  the turn still worked: it holds what the turn did up to there, not
   *  after. */
  went_on?: boolean;
  /** The sessions a launch of the turn ran out of its model's context in. No
   *  launch continues any of them. */
  exhausted?: string[];
}

/** One turn's executor session. */
export interface ExecutorSessionRow {
  turn_id: string;
  /** The surface-scoped conversation the turn served, "" for none. */
  conversation_key: string;
  work_key: string;
  /** The harness that wrote the session, and its release as the box printed it. */
  harness: string;
  harness_version: string;
  /** When the turn's newest capture was recorded; null where nothing was. */
  captured_at: string | null;
  /** When the turn's end was recorded; null while the turn runs. */
  ended_at: string | null;
  ending: SessionEnding | null;
  /** The opening of what the turn was asked. */
  ask_excerpt: string;
  /** The row still names a session's object, of `object_bytes` sealed. */
  kept: boolean;
  object_bytes: number;
  /**
   * The ended turn whose session this turn's first launch was handed to
   * continue: its conversation's head as the turn's dispatch read it, the one
   * of its ended turns with a kept session that read it last. Absent where the
   * read named none, or the log held none for it. Whether a launch took it up
   * is the turn's executor records' to say.
   */
  base_turn?: string;
  /**
   * Which launch `continuable` is about: `next_fire` for an ended turn of a
   * schedule's conversation, `next_turn` for any other's. Absent where no
   * launch would ever continue the session — a turn that served no
   * conversation — and on a transcript's later page.
   */
  continued_by?: SessionContinuedBy;
  /**
   * Whether that launch would continue this session, as far as the engine can
   * know before a box is chosen: decided as the launch decides it, for the
   * member the node holding the seat would launch it on now — a relaunch of a
   * live turn on the turn's own revision; an ended turn's conversation's next
   * turn (a schedule's next fire), which continues only the conversation's
   * head, the newest session in its line, on the revision live when it comes.
   * Whether the box a launch gets takes the session up, and whether the seat's
   * budget can carry it, are the launch's to find out. Null where it cannot be
   * decided now — a run record or a log row that could not be read, a job still
   * working or being collected, a capture still being kept, what the launch is
   * launched on not to be said (every member of the seat's chain sitting out
   * its plan, no node holding the seat, the node holding it not answering), a
   * relaunch only the resume of its job's collection decides, a turn whose end
   * is not on the log yet — which `not_continuable` then says: an unknown is
   * never sent as a no. A transcript's later page leaves it null, its first
   * page having said it.
   */
  continuable: boolean | null;
  /** Where the launch would continue it; present only where it would. */
  continues_in?: SessionContinuesIn;
  /** Why the launch would not continue it, why that cannot be said now, or —
   *  where a relaunch would only in the box the run parked in — why no other
   *  box would: the engine's own sentence, redacted. Absent where the launch
   *  would continue it in any box. */
  not_continuable?: string;
}

/** A page of a seat's executor sessions, the latest begun first. */
export interface ExecutorSessionsAnswer {
  handle: string;
  sessions: ExecutorSessionRow[];
  /** The cursor of the page after this one; absent on the last. */
  next_cursor?: string;
  read_level: string;
  position: LogPosition;
  /** False where the answering copy holds a record of the log it could not
   *  read, which may hold rows missing from this page. */
  complete: boolean;
}

/** What reading a session's object came to (`executorread.TranscriptStates`). */
export type TranscriptState =
  "read" | "none" | "missing" | "unreadable" | "malformed" | "unsupported";

/** Who an entry of a transcript is from (`executor.EntryKinds`): a note is
 *  one a person sent the run while it worked. */
export type TranscriptEntryKind =
  "brief" | "note" | "model" | "tool_call" | "tool_result" | "summary" | "unread";

/**
 * Why a page of a transcript ends before the transcript does
 * (`executor.TranscriptCuts`): at the most entries or bytes a page holds, with
 * more after it, or at the most one read takes, with nothing a later page could
 * add. Composed into the answer alone: the screen reads it off a page.
 */
type TranscriptCut = "entries" | "bytes" | "read";

/** One entry of a transcript, in the order the harness recorded it. */
export interface TranscriptEntry {
  kind: TranscriptEntryKind;
  /** When the harness recorded it, where its record says. */
  at?: string;
  /** The subagent whose transcript holds it; absent for the run's own. */
  thread?: string;
  /** The tool a call or a result is of. */
  tool?: string;
  /** A tool result the harness recorded as an error. */
  failed?: boolean;
  /** Redacted, and cut to the entry's bound where `cut` says so. */
  text: string;
  /** The size of what the whole text redacts to, where the read redacted all
   *  of it; of a longer text, the size the harness recorded, rounded up to a
   *  whole KiB. Never the recorded size to the byte, which beside a text shown
   *  whole told how long each value its redaction took out was. */
  bytes: number;
  cut?: boolean;
}

/** What every read of a transcript is held to. */
interface TranscriptBounds {
  entries: number;
  bytes: number;
  entry_bytes: number;
  line_bytes: number;
  read_bytes: number;
}

/** One turn's session and a page of its transcript. */
export interface ExecutorSessionAnswer {
  handle: string;
  session: ExecutorSessionRow;
  state: TranscriptState;
  /** Why no transcript is shown, wherever `state` is not `read`: the engine's
   *  own sentence, never anything the archive held. */
  why?: string;
  entries: TranscriptEntry[];
  /** The ordinal of the first of `entries` among the transcript's. */
  from: number;
  cut?: TranscriptCut;
  /**
   * The cursor the entries after these are read from; absent where none
   * follow. It names the capture it was read from and how the engine counts
   * entries, so the engine refuses it (`bad_params`) once the session is
   * captured again or a build counting otherwise answers: read the transcript
   * again from its start. A later page of a session whose object was released
   * since is answered as the first page would be, `none`.
   */
  next_cursor?: string;
  bounds: TranscriptBounds;
  /** How the session's row was read, as the list says it of its page. */
  read_level: string;
  position: LogPosition;
  complete: boolean;
}
