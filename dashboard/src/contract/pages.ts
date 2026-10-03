/**
 * What the engine says about a page beyond the page itself: who read it, who
 * loaded it as a skill, and what links to it.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestThePageScreenReadsWhatTheseAnswersSend` in both directions, the same
 * gate the memory answer has: every member declared here is a key the answer
 * carries, and every key it carries is declared.
 */

/** Where the run a read happened in sits in the tracker: "turn 2 on ENG-412". */
export interface TurnPlace {
  /** The task's id. */
  id: string;
  key: string;
  title: string;
  /** The task's own "Turn n" — 0 for a run with no counted row there. */
  ordinal: number;
}

/** One seat's reads of a page through one way of reaching it. */
export interface PageReadRow {
  handle: string;
  role?: string;
  /** `get_page`, `search`, `prefetch` or `skill_loaded`. */
  via: string;
  count: number;
  last_at: string;
  last_turn_id?: string;
  last_work_key?: string;
  /** The query a search or the turn-start prefetch ran. */
  last_query?: string;
  last_work_item?: TurnPlace;
}

/** `page_reads{page, days}`: who read a page, over up to thirty company days. */
export interface PageReadsAnswer {
  page: string;
  since: string;
  until: string;
  days: number;
  /** Newest read first — at most a hundred of `readers_total`. */
  readers: PageReadRow[];
  readers_total: number;
  /** Seats that read the page on the COMPANY's today. */
  distinct_seats_today: number;
  /** Entries the per-seat-day cap dropped across the window, COMPANY-WIDE —
   *  some may have been this page's, none may have been; 0 says the list is
   *  certainly complete. */
  elided: number;
}

/** One seat a tool-skill page reached as a skill. */
export interface SkillLoad {
  handle: string;
  last_at: string;
  /** `loaded` plus `offered`. */
  count: number;
  /** Times the seat asked for the skill's body (load_tool_skill). */
  loaded: number;
  /** Times a phase's catalogue put the skill's summary in front of it. */
  offered: number;
}

/** A page whose body links here. */
export interface PageLink {
  id: string;
  container: string;
  title: string;
}

/** A task that links here: as one of its linked pages, in its description, or both. */
export interface TaskLink {
  id: string;
  key: string;
  title: string;
  status: string;
  via: string[];
}

/**
 * Why a page answer carries no `linked_from` from a node that holds an index:
 * `building` while the index is on its first lap (an empty list would claim
 * nothing links here before every body was read), `unavailable` when the read
 * failed. Held against the engine's `LinkedFromStatuses`.
 */
export type LinkedFromStatus = "building" | "unavailable";

/** A page's "linked from", from this node's index. */
export interface PageBacklinks {
  pages: PageLink[];
  tasks: TaskLink[];
  pages_total: number;
  tasks_total: number;
}
