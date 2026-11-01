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
  /** The key is one another task claimed first, so it opens THAT task and this
   *  one is reached by its id — every link to it goes by `itemAddress`. */
  key_collision?: boolean;
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
  /** The key is one another task claimed first, so it opens THAT task and this
   *  one is reached by its id — every link to it goes by `itemAddress`. */
  key_collision?: boolean;
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

/**
 * The most a page's TITLE holds, in UTF-8 bytes: `internal/doclet`' `MaxTitle`.
 * Every form that names a page bounds its title here, before the press: the
 * engine refuses a title past it rather than cutting it, and the fields used to
 * carry `maxLength={200}`, which the browser applies by cutting a paste —
 * silently, in characters, short of what the engine would take. Held against the
 * engine's figure by `internal/doclet/client_gate_test.go`.
 */
export const PAGE_TITLE_MAX_BYTES = 256;

/**
 * The most a page save's MESSAGE holds, in UTF-8 bytes: `internal/doclet`'
 * `MaxMessage`. Bounded on the editor for the reason a title is, by the same
 * gate.
 */
export const PAGE_MESSAGE_MAX_BYTES = 256;

/**
 * The most a page's BODY holds, in UTF-8 bytes: `internal/doclet`' `MaxBody`.
 * The composer counts against it and holds Publish past it, because the
 * engine refuses a body past the cap rather than cutting it.
 */
export const PAGE_BODY_MAX_BYTES = 524288;

/** The most labels a page carries: `internal/doclet`' `MaxLabels`. */
export const PAGE_LABELS_MAX = 32;

/** The most one label holds, in UTF-8 bytes: `internal/doclet`' `MaxLabelLength`. */
export const PAGE_LABEL_MAX_BYTES = 64;

/**
 * The orders a `pages` listing is read in — `internal/doclet`' `ListOrders`:
 * `title`, the knowledge base's own (by space, title and id, the order every
 * tree is drawn in), and `newest`, by when each page last changed, which the
 * engine serves beside `skills: true` alone. Agent skills offers both, and
 * Home's Agent skills widget asks for the newest.
 *
 * Held against the engine's list in both directions by
 * `internal/doclet/client_gate_test.go`: an order the engine does not read is
 * a `bad_params` refusal of the whole listing, and one it grew that nothing
 * here names is an order nobody can ask for.
 */
export const PAGE_LIST_ORDERS = ["title", "newest"] as const;

/**
 * What each change to a page reads as, in the two places a change is drawn:
 * `verb` before the page's title on the company's feed ("Ana saved Deploy
 * runbook"), and `phrase` on the page's own history, where the page goes
 * without saying ("Ana saved it").
 *
 * ONE DECLARATION, held against `doclet.ChangeKinds` by
 * `internal/doclet/client_gate_test.go`: a kind the engine writes and this list
 * does not name reads as the bare kind after the writer's name, silently, and
 * a kind named here the engine never writes is a sentence nothing reaches.
 */
export const PAGE_CHANGES = [
  { kind: "created", verb: "wrote", phrase: "wrote it" },
  { kind: "saved", verb: "saved", phrase: "saved it" },
  { kind: "renamed", verb: "renamed", phrase: "renamed it" },
  { kind: "moved", verb: "moved", phrase: "moved it" },
  { kind: "status", verb: "changed the status of", phrase: "changed its status" },
  { kind: "comment", verb: "commented on", phrase: "commented" },
  { kind: "comment_edited", verb: "edited a comment on", phrase: "edited a comment" },
  { kind: "labels", verb: "changed the labels of", phrase: "changed its labels" },
  { kind: "watchers", verb: "changed who watches", phrase: "changed who watches it" },
  { kind: "removed", verb: "removed", phrase: "removed it" },
] as const;
