/**
 * A task's activity as ONE list: what changed on it, what was said on it and
 * the stories of the agent turns charged to it, in the order it happened.
 *
 * # Three histories, each paged on its own
 *
 * The three are three questions (`work_activity`, `work_comments`,
 * `turn_stories`), each answered a page at a time from the newest. Merged
 * naively, a page of fifty changes beside a page of twenty turns draws a
 * stretch of time in which only changes appear — the turns of that stretch are
 * on the turns' second page, not yet asked for — and reads as a fortnight in
 * which no agent touched the task.
 *
 * So the merge is CUT AT A HORIZON: the newest point any history with more
 * pages stopped at. Everything newer than it is complete in all three, and
 * nothing older is drawn until the history that stopped there is asked for
 * its next page. It is the rule the fleet's own keyset merge keeps
 * (`internal/eventfan`), for the same reason — and it holds only because each
 * entry is placed at the instant its own history pages by: a change at when it
 * took effect, a comment at when it was written, a story at when the session
 * began, which is what the story list orders and cuts its cursor on. The one
 * exception is the fleet's: a session resumed on another node, whose first
 * half names no work item, is paged where the half that names this task is
 * listed (`api-endpoints.md`, "Reading the fleet's history") — so its card can
 * sit older than the page it came on, and the stretch between the two can be
 * drawn before every story in it is read. Nothing is lost: the next page of
 * stories fills it.
 *
 * # A comment is drawn once
 *
 * A comment writes a change record as well as a comment, so the change feed
 * carries every comment again as a one-line excerpt. The comment is the thing
 * itself — whole, threaded, with its ask — and the change that announced it is
 * dropped from both the merged list and the Changes tab.
 */

import type { TurnStory, WorkActivityRecord, WorkComment } from "~/protocol/index.ts";

/** The four views of a task's activity. */
export const ACTIVITY_TABS = ["all", "comments", "turns", "changes"] as const;
export type ActivityTab = (typeof ACTIVITY_TABS)[number];

/** The three histories. */
export type Source = "changes" | "comments" | "turns";

export type Entry =
  | { kind: "change"; id: string; at: number; record: WorkActivityRecord }
  | { kind: "comment"; id: string; at: number; comment: WorkComment }
  | { kind: "turn"; id: string; at: number; story: TurnStory };

/** One history as far as it has been read. */
export interface Loaded<T> {
  items: readonly T[];
  /** A page older than every one loaded exists. */
  more: boolean;
}

export interface Timeline {
  /** Oldest first — a conversation is read down. */
  entries: Entry[];
  /** The histories whose next page moves the horizon back; empty when every
   *  one shown is read to its start. */
  earlier: Source[];
}

/** Which histories a tab draws. */
const SOURCES: Record<ActivityTab, readonly Source[]> = {
  all: ["changes", "comments", "turns"],
  comments: ["comments"],
  turns: ["turns"],
  changes: ["changes"],
};

function instant(at: string | undefined): number {
  const ms = at ? Date.parse(at) : Number.NaN;
  return Number.isFinite(ms) ? ms : 0;
}

/** Whether a change record is the announcement of a comment. */
export function isCommentRecord(record: WorkActivityRecord): boolean {
  return Boolean(record.comment_id);
}

export function timeline(
  tab: ActivityTab,
  loaded: {
    changes: Loaded<WorkActivityRecord>;
    comments: Loaded<WorkComment>;
    turns: Loaded<TurnStory>;
  },
): Timeline {
  const shown = SOURCES[tab];
  const bySource: Record<Source, Entry[]> = {
    changes: loaded.changes.items
      .filter((r) => !isCommentRecord(r))
      .map((record) => ({
        kind: "change" as const,
        id: `change:${record.id}`,
        at: instant(record.effective_at || record.at),
        record,
      })),
    comments: loaded.comments.items.map((comment) => ({
      kind: "comment" as const,
      id: `comment:${comment.id}`,
      at: instant(comment.created_at),
      comment,
    })),
    turns: loaded.turns.items.map((story) => ({
      kind: "turn" as const,
      id: `turn:${story.turn_id}`,
      at: instant(story.started_at),
      story,
    })),
  };
  // THE HORIZON: the newest point any shown history with more pages stopped
  // at. A history with more and nothing loaded yet stops everything.
  const oldest = (source: Source): number => {
    // The OLDEST INSTANT a history's loaded entries reach, measured over
    // every entry it returned — a comment record dropped from the list still
    // marks how far back the change feed was read.
    const times =
      source === "changes"
        ? loaded.changes.items.map((r) => instant(r.effective_at || r.at))
        : bySource[source].map((e) => e.at);
    return times.length === 0 ? Number.POSITIVE_INFINITY : Math.min(...times);
  };
  let horizon = Number.NEGATIVE_INFINITY;
  for (const source of shown) {
    if (loaded[source].more) horizon = Math.max(horizon, oldest(source));
  }
  const entries = shown
    .flatMap((source) => bySource[source])
    .filter((e) => e.at >= horizon)
    .sort((a, b) => a.at - b.at || a.id.localeCompare(b.id));
  const earlier = shown.filter((source) => loaded[source].more && oldest(source) >= horizon);
  return { entries, earlier };
}
