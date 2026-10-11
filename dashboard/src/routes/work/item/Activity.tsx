/**
 * Everything that happened on a task, as one timeline read down: who filed it,
 * who handed it to whom and why, what was said, and every agent turn charged
 * to it — with the turn running on it now at the foot, and the box to write
 * in under that.
 *
 * # Four views of three histories
 *
 * All | Comments | Agent turns | Changes. The three histories are paged on
 * their own and merged at a horizon ([timeline]), so a stretch of time drawn
 * here is complete in every history it shows.
 *
 * # A turn card is the session's story
 *
 * `turn_stories` tells each agent session charged to the task as the
 * published facts of it — why it started, what it did and left behind, how it
 * ended — and nothing the machine processed, so the tab reads the same to
 * every teammate. "Turn 3" on a card is the task's own count, the one the cost
 * panel's "3 turns" is, read where the count lives. A story is turn-level
 * detail and lives as long as the event history does — a month; what outlives
 * it is the count. Its Trace link is offered to an admin alone, who may read
 * how the session ran.
 *
 * # The live row is the turn running on THIS task
 *
 * Joined on the item the engine charges the running turn to
 * (`live_call.work_item`, then the turn's own) — never on a `work_key` a
 * trigger named — and drawn only while that seat's own state is `working`.
 *
 * # Who a change reached
 *
 * Every announced change carries an expander that asks `work_routing` for its
 * recipients: the one reason of eighteen each person was reached under, and
 * whether it asked them anything. The three EMPTY answers are three different
 * facts and keep three sentences ([NobodyWoken]).
 */

import { useMemo, useState } from "react";
import { Button, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import {
  ArrowRightGlyph,
  BellGlyph,
  ChevronDownGlyph,
  ChevronRightGlyph,
  ChevronUpGlyph,
  CircleQuestionMarkGlyph,
  ClockGlyph,
  CornerDownLeftGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { AsksTag, type RowChrome } from "~/components/work.tsx";
import { Composer, type ComposeMode } from "~/components/Composer.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { Segmented } from "~/ui/primitives.tsx";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { turnIdOf, watchHref } from "~/lib/turns.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  OUTCOME_WORDS,
  actionLine,
  involvedLine,
  linkLine,
  noteLine,
  whyLine,
} from "~/lib/stories.ts";
import { renderMarkdown, plainText } from "~/lib/markdown.ts";
import { fmtDateCompact, fmtDateTime, fmtDuration, fmtElapsed, fmtMinute } from "~/lib/format.ts";
import { reasonAbout } from "~/lib/reasons.ts";
import { activityOf, doingWords, ringOf, type OrgIndex } from "~/lib/seats.ts";
import {
  changeClauses,
  changeMark,
  describeChange,
  statusLabel,
  type LabelContext,
} from "~/lib/work.ts";
import type {
  AgentRow,
  WorkActivityRecord,
  WorkComment,
  TurnStory,
  WorkItem,
  WorkRoutingAnswer,
} from "~/protocol/index.ts";
import { ACTIVITY_TABS, timeline, type ActivityTab, type Entry, type Source } from "./timeline.ts";
import { usePaged } from "~/lib/usePaged.ts";

/** How often the newest page of each history is asked again: nothing pushes them. */
const ACTIVITY_POLL_MS = 30_000;

/** The pages each history is read in — the engine's own ceilings. */
const CHANGES_PAGE = 50;
const COMMENTS_PAGE = 50;
/** A story costs the fleet a read of its turn's record — see `queries.DefaultStoryPage`. */
const STORIES_PAGE = 10;

const TAB_LABEL: Record<ActivityTab, string> = {
  all: "All",
  comments: "Comments",
  turns: "Agent turns",
  changes: "Changes",
};

/** What each tab says when there is nothing in it. */
const EMPTY: Record<ActivityTab, string> = {
  all: "Nothing has happened on this task yet.",
  comments: "Nobody has commented on this task yet.",
  turns: "No agent turn has been charged to this task.",
  changes: "Nothing has changed on this task since it was filed.",
};

export function Activity({
  item,
  chrome,
  index,
  now,
  live,
  param = "activity",
}: {
  item: WorkItem;
  chrome: RowChrome & LabelContext;
  index: OrgIndex;
  now: number;
  /** The seat running a turn on this task now, or null. */
  live: AgentRow | null;
  /** The address key the tab is kept under — the peek's own, so a task page
   *  and the sub-task peeked over it do not move each other's tab. */
  param?: string;
}) {
  const [raw, setTab] = useParam(param, "all", "filter");
  const tab: ActivityTab = (ACTIVITY_TABS as readonly string[]).includes(raw)
    ? (raw as ActivityTab)
    : "all";
  const [replyTo, setReplyTo] = useState<WorkComment | null>(null);

  const { socket } = useClient();
  const changesAsked = { task: item.key, limit: CHANGES_PAGE };
  const changes = usePaged(
    useQuery("work_activity", changesAsked, { pollMs: ACTIVITY_POLL_MS }),
    changesAsked,
    (cursor) =>
      socket.query(
        "work_activity",
        withFloor("work_activity", JSON.stringify({ ...changesAsked, cursor })),
      ),
    pickRecords,
    recordId,
    nextCursor,
  );
  const commentsAsked = { item: item.key, limit: COMMENTS_PAGE };
  const comments = usePaged(
    useQuery("work_comments", commentsAsked, { pollMs: ACTIVITY_POLL_MS }),
    commentsAsked,
    (cursor) =>
      socket.query(
        "work_comments",
        withFloor("work_comments", JSON.stringify({ ...commentsAsked, cursor })),
      ),
    pickComments,
    commentId,
    nextCursor,
  );
  // BY THE TASK'S IDENTITY, `native:<id>` — the one the turns it is charged
  // are filed under — never its key, which a move renames. NOT AT A READ
  // FLOOR: a story is read from the event history, which no write a person
  // makes on this screen is read back through.
  const turnsAsked = { work_item: `native:${item.id}`, limit: STORIES_PAGE };
  const turns = usePaged(
    useQuery("turn_stories", turnsAsked, { pollMs: ACTIVITY_POLL_MS }),
    turnsAsked,
    (before) => socket.query("turn_stories", { ...turnsAsked, before }),
    pickStories,
    storyId,
    nextStories,
  );
  const paged = { changes, comments, turns };

  // WHAT A DELTA'S OTHER END IS CALLED, from every page of the change feed —
  // a relation, a re-parent and a cascade name the other task by its id.
  const labels = useMemo<RowChrome & LabelContext>(() => {
    const keys: Record<string, string> = {};
    for (const answer of changes.answers) Object.assign(keys, answer.keys ?? {});
    return { ...chrome, taskKey: (id: string) => keys[id] ?? "" };
  }, [chrome, changes.answers]);
  // THE PERSON BEHIND A TOKEN'S COMMENT, from every page of the thread.
  const commentSeats = useMemo(() => {
    const seats: Record<string, string> = {};
    for (const answer of comments.answers) Object.assign(seats, answer.comment_seats ?? {});
    return seats;
  }, [comments.answers]);

  const runningHere = live && (tab === "all" || tab === "turns") ? live : null;
  const liveTurn = runningHere ? turnIdOf(runningHere) : "";
  // THE RUNNING SESSION IS THE LIVE ROW, not a card as well: its story is
  // listed from the moment it starts, and drawn twice it would say "Working"
  // above the row that says what it is doing.
  const stories = useMemo(
    () => (liveTurn ? turns.items.filter((s) => s.turn_id !== liveTurn) : turns.items),
    [turns.items, liveTurn],
  );
  const drawn = useMemo(
    () =>
      timeline(tab, {
        changes: { items: changes.items, more: changes.more },
        comments: { items: comments.items, more: comments.more },
        turns: { items: stories, more: turns.more },
      }),
    [tab, changes.items, changes.more, comments.items, comments.more, stories, turns.more],
  );
  const shown: readonly Source[] =
    tab === "all" ? ["changes", "comments", "turns"] : [tab === "turns" ? "turns" : tab];
  const loading = shown.some((source) => paged[source].loading && paged[source].items.length === 0);
  const errors = shown.map((source) => paged[source].error).filter(Boolean);
  const paging = shown.some((source) => paged[source].paging);
  const pageError = shown.map((source) => paged[source].pageError).find(Boolean) ?? null;

  // THE NUMBER THE RUNNING TURN HAS: its story's, which is the task's own
  // count read where it lives, and otherwise the one after every turn counted
  // so far — the story is a page away, or not answered yet.
  const liveOrdinal = useMemo(() => {
    const told = turns.items.find((s) => s.turn_id === liveTurn)?.work_item?.ordinal;
    return told && told > 0 ? told : (item.spend?.turns ?? 0) + 1;
  }, [liveTurn, turns.items, item.spend?.turns]);

  const compose: ComposeMode = replyTo
    ? { kind: "reply", replyTo: replyTo.id }
    : { kind: "comment" };
  const assignee = item.assignee ? (chrome.seatName?.(item.assignee) ?? item.assignee) : "";

  return (
    <section className="task-activity" aria-labelledby="task-activity-title">
      <div className="task-activity-head">
        <h2 id="task-activity-title" className="task-h2">
          Activity
        </h2>
        <span className="spacer" />
        <Segmented<ActivityTab>
          ariaLabel="Which activity"
          size="sm"
          value={tab}
          onChange={setTab}
          options={ACTIVITY_TABS.map((value) => ({ value, label: TAB_LABEL[value] }))}
        />
      </div>

      {errors.map((error, i) => (
        <QueryState key={i} error={error ?? null} loading={false} />
      ))}
      {drawn.earlier.length > 0 && (
        <div className="task-earlier">
          {/* A CONTROL THAT LOOKS LIKE ONE: the chevron says which way the
              page goes, and without it a ghost button's label at the feed's
              own weight read as a heading over the entries below it. */}
          <Button
            size="small"
            variant="ghost"
            leadingIcon={<ChevronUpGlyph size="sm" />}
            loading={paging}
            onClick={() => {
              for (const source of drawn.earlier) paged[source].older();
            }}
          >
            Earlier activity
          </Button>
          {pageError && <QueryState error={pageError} loading={false} />}
        </div>
      )}
      {loading ? (
        <Skeleton variant="text" rows={4} label="Loading the activity" />
      ) : drawn.entries.length === 0 && !runningHere ? (
        <p className="muted task-activity-empty">{EMPTY[tab]}</p>
      ) : (
        <ol className="task-feed">
          {drawn.entries.map((entry) => (
            <FeedEntry
              key={entry.id}
              entry={entry}
              item={item}
              chrome={labels}
              index={index}
              now={now}
              seatOfComment={(c) => commentSeats[c.id] ?? ""}
              onReply={setReplyTo}
            />
          ))}
          {runningHere && (
            <LiveRow
              item={item}
              row={runningHere}
              ordinal={liveOrdinal}
              chrome={chrome}
              now={now}
            />
          )}
        </ol>
      )}

      {(tab === "all" || tab === "comments") && (
        <div className="task-compose">
          {replyTo && (
            <div className="task-compose-reply">
              <CornerDownLeftGlyph size="xs" />
              <span className="truncate">
                {`Replying to ${chrome.seatName?.(commentSeats[replyTo.id] || replyTo.author) ?? replyTo.author}`}
              </span>
              <Button size="small" variant="ghost" onClick={() => setReplyTo(null)}>
                Cancel reply
              </Button>
            </div>
          )}
          <Composer
            item={item.key}
            mode={compose}
            to={replyTo ? (chrome.seatName?.(replyTo.author) ?? replyTo.author) : item.key}
            index={index}
            placeholder={
              replyTo
                ? undefined
                : assignee
                  ? `Leave a comment — Ask… to put a question to ${assignee} directly`
                  : "Leave a comment — @mention to bring someone in"
            }
            onReply={() => setReplyTo(null)}
          />
        </div>
      )}
    </section>
  );
}

/** Where each of the tracker's histories says its page before is. */
const nextCursor = (a: { next_cursor?: string }) => a.next_cursor ?? "";
const pickRecords = (a: { records: WorkActivityRecord[] }) => a.records ?? [];
const recordId = (r: WorkActivityRecord) => r.id;
const pickComments = (a: { comments: WorkComment[] }) => a.comments ?? [];
const commentId = (c: WorkComment) => c.id;
const pickStories = (a: { stories: TurnStory[] }) => a.stories ?? [];
const storyId = (s: TurnStory) => s.turn_id;
/** The story list's own cursor, `next` — not the tracker's `next_cursor`. */
const nextStories = (a: { next?: string }) => a.next ?? "";

function FeedEntry({
  entry,
  item,
  chrome,
  index,
  now,
  seatOfComment,
  onReply,
}: {
  entry: Entry;
  item: WorkItem;
  chrome: RowChrome & LabelContext;
  index: OrgIndex;
  now: number;
  seatOfComment: (c: WorkComment) => string;
  onReply: (c: WorkComment) => void;
}) {
  switch (entry.kind) {
    case "change":
      return <ChangeRow record={entry.record} item={item} chrome={chrome} now={now} />;
    case "comment":
      return (
        <CommentRow
          comment={entry.comment}
          seat={seatOfComment(entry.comment)}
          chrome={chrome}
          index={index}
          now={now}
          onReply={onReply}
        />
      );
    case "turn":
      return <StoryCard story={entry.story} chrome={chrome} />;
  }
}

/** Who a record names as its author: the person behind a token, or a seat. */
function actorOf(record: WorkActivityRecord, chrome: RowChrome): string {
  const who = record.actor_seat || record.actor || "";
  if (!who) return "The engine";
  return chrome.seatName?.(who) ?? who;
}

/**
 * One change, as a sentence about the task: "Jane Founder filed this in ENG",
 * "CTO assigned it to SWE — “owns the provisioner”", "Maya moved it to In
 * review". What no sentence here covers falls through to the delta, which
 * names every field it moved.
 */
export function changeSentence(
  record: WorkActivityRecord,
  item: WorkItem,
  chrome: RowChrome & LabelContext,
): { what: string; why: string } {
  const delta = (field: string) =>
    record.fields?.[field] as { from?: unknown; to?: unknown } | undefined;
  switch (record.kind) {
    case "created":
      return { what: `filed this in ${record.project || item.project}`, why: "" };
    case "assignee": {
      const to = String(delta("assignee")?.to ?? "");
      // THE EXCERPT OF A HAND-OFF IS ITS REASON — the one line the new
      // assignee is woken with (`update_work_item{reason}`).
      const why = record.excerpt ? plainText(record.excerpt) : "";
      return to
        ? { what: `assigned it to ${chrome.seatName?.(to) ?? to}`, why }
        : { what: "unassigned it", why };
    }
    case "status": {
      const to = String(delta("status")?.to ?? "");
      // AND OF WORK SENT BACK, its note: the one status change that carries
      // an excerpt, saying what to do again (`update_work_item{reason}`).
      const why = record.excerpt ? plainText(record.excerpt) : "";
      if (to) return { what: `moved it to ${statusLabel(to, chrome.statuses)}`, why };
      break;
    }
  }
  // A SENTENCE, NOT A LABEL: the row reads "Jane Founder ticked 1 on
  // Acceptance", so each move is a clause with a verb ([changeClauses]); a
  // record whose fields say nothing this build can phrase falls back to the
  // label the other change feeds draw, and then to its kind.
  const clauses = changeClauses(record.fields, chrome);
  if (clauses.length > 0) return { what: joinClauses(clauses), why: "" };
  const said = describeChange(record, chrome);
  return { what: said ? `changed ${said}` : record.kind.replaceAll("_", " "), why: "" };
}

/** "a", "a and b", "a, b and c" — by hand, for the reason `listOf` gives. */
function joinClauses(clauses: string[]): string {
  if (clauses.length <= 1) return clauses[0] ?? "";
  return `${clauses.slice(0, -1).join(", ")} and ${clauses[clauses.length - 1]}`;
}

function ChangeRow({
  record,
  item,
  chrome,
  now,
}: {
  record: WorkActivityRecord;
  item: WorkItem;
  chrome: RowChrome & LabelContext;
  now: number;
}) {
  const [open, setOpen] = useState(false);
  const { what, why } = changeSentence(record, item, chrome);
  const at = record.effective_at || record.at;
  return (
    <li className="task-entry">
      <span className="task-entry-mark" aria-hidden="true">
        <Mark name={changeMark(record.kind)} size="sm" />
      </span>
      <div className="task-entry-line">
        <p>
          <b>{actorOf(record, chrome)}</b> {what}
          {why && <> — “{why}”</>}{" "}
          <time className="task-when" dateTime={at} title={fmtDateTime(at)}>
            {`· ${fmtDateCompact(at, now)}`}
          </time>
          {record.turn_id && (
            <>
              {" "}
              <a
                className="prose-link task-when"
                href={href(["admin", "live", "turns", record.turn_id])}
              >
                turn →
              </a>
            </>
          )}
          {/* A QUIET DISCLOSURE AT THE END OF THE LINE, in the link tone with
              its chevron: on a line of its own in bold it added a row to every
              change and did not read as something to open. */}
          {record.notified && (
            <>
              {" "}
              <button
                type="button"
                className="task-reached"
                aria-expanded={open}
                onClick={() => setOpen((o) => !o)}
              >
                {"· Who this reached"}
                {open ? <ChevronDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
              </button>
            </>
          )}
        </p>
        {open && <Reached recordId={record.id} chrome={chrome} />}
      </div>
    </li>
  );
}

/** A comment on the task: who said it, when, what, and what it asks. */
function CommentRow({
  comment,
  seat,
  chrome,
  index,
  now,
  onReply,
}: {
  comment: WorkComment;
  /** The person behind the token that wrote it, or "" where the author is a seat. */
  seat: string;
  chrome: RowChrome;
  index: OrgIndex;
  now: number;
  onReply: (c: WorkComment) => void;
}) {
  // THE PERSON, NEVER THE CREDENTIAL: a token's comment is drawn as the seat
  // it was bound to, and one bound to nobody as "An operator".
  const handle = seat || (comment.author_kind === "operator" ? "" : comment.author);
  const person = handle ? index.byHandle.get(handle) : undefined;
  const name = handle ? (chrome.seatName?.(handle) ?? handle) : "An operator";
  const chosen = comment.choice
    ? (comment.decision?.options.find((o) => o.id === comment.choice)?.label ?? comment.choice)
    : "";
  return (
    <li className="task-entry" data-kind="comment">
      <span className="task-entry-avatar">
        <SeatAvatar
          name={name}
          kind={person?.kind === "human" ? "human" : "agent"}
          size={28}
          decorative
        />
      </span>
      <article className="task-comment" aria-label={`${name}'s comment`}>
        <header className="task-comment-head">
          <b>{name}</b>
          <time
            className="task-when"
            dateTime={comment.created_at}
            title={fmtDateTime(comment.created_at)}
          >
            {fmtDateCompact(comment.created_at, now)}
          </time>
          {comment.updated_at && <span className="task-when">(edited)</span>}
          {comment.ask && (
            <Tag variant="warning" leadingIcon={<CircleQuestionMarkGlyph size="xs" />}>
              {`asked ${chrome.seatName?.(comment.ask) ?? comment.ask}`}
            </Tag>
          )}
          {comment.answers && (
            <Tag appearance="outline" leadingIcon={<ArrowRightGlyph size="xs" />}>
              answers a question
            </Tag>
          )}
          {comment.resolved && <Tag variant="success">Resolved</Tag>}
          <span className="spacer" />
          {!comment.removed && (
            <Button size="small" variant="ghost" onClick={() => onReply(comment)}>
              Reply
            </Button>
          )}
        </header>
        <div className="prose md task-comment-body">
          {comment.removed ? (
            <span className="muted">(this comment was removed)</span>
          ) : comment.body.trim() ? (
            renderMarkdown(comment.body)
          ) : chosen ? (
            <p>{`Chose “${chosen}”.`}</p>
          ) : null}
        </div>
        {comment.decision && (
          <div className="task-comment-decision">
            <span className="task-lbl">{comment.decision.question}</span>
            <ul>
              {comment.decision.options.map((o) => (
                <li key={o.id}>
                  {o.label}
                  {o.id === comment.decision?.recommended && (
                    <span className="t-caption"> · recommended</span>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}
      </article>
    </li>
  );
}

/**
 * One agent session on the task, told as its story: who worked on it and which
 * turn on the task it was, how it ended, why it started, what it did and what
 * it left behind — in that order, because it is the order a teammate asks.
 *
 * NOTHING THE MACHINE PROCESSED: no prompt, tool, model, round or token count
 * reaches the story (`turn_stories` is a member's answer), so none can reach
 * the card. What the session cost is the cost panel's, as a count of turns;
 * how it ran is the trace's, which only an admin is offered.
 */
export function StoryCard({
  story,
  chrome,
}: {
  story: TurnStory;
  chrome: RowChrome & LabelContext;
}) {
  const viewer = useViewer();
  const nameOf = (handle: string) => chrome.seatName?.(handle) ?? handle;
  const name = nameOf(story.seat);
  const kind = chrome.seatKind?.(story.seat) === "human" ? "human" : "agent";
  const ordinal = story.work_item?.ordinal ?? 0;
  const running = story.outcome === "running" || story.outcome === "waiting_on_you";
  const outcome = OUTCOME_WORDS[story.outcome];
  const why = whyLine(story, nameOf);
  const involved = involvedLine(story, nameOf);
  return (
    <li className="task-entry" data-kind="turn">
      <span className="task-entry-avatar">
        <SeatAvatar name={name} kind={kind} size={28} decorative />
      </span>
      <article className="task-turn" aria-label={ordinal ? `Turn ${ordinal}` : "A turn"}>
        <header className="task-turn-head">
          <b>{name}</b>
          <span className="muted">
            {ordinal
              ? running
                ? `is on turn ${ordinal}`
                : `ran turn ${ordinal}`
              : "worked on this task"}
          </span>
          <Tag variant={outcome.tone} size="sm">
            {outcome.label}
          </Tag>
          <span className="spacer" />
          <time
            className="task-when"
            dateTime={story.started_at}
            title={fmtDateTime(story.started_at)}
          >
            {fmtMinute(story.started_at)}
          </time>
        </header>
        <div className="task-turn-body">
          {why && <p className="task-turn-why">{why}</p>}
          {story.did.length > 0 && (
            <ul className="task-turn-did" aria-label="What it did">
              {story.did.map((action, i) => (
                <li key={`${action.kind}:${action.at}:${i}`}>{actionLine(action)}</li>
              ))}
            </ul>
          )}
          {story.left_behind.length > 0 && (
            <ul className="task-turn-left" aria-label="What it left behind">
              {story.left_behind.map((link, i) => (
                <li key={`${link.kind}:${link.id}:${link.change}:${i}`}>
                  <a
                    className="prose-link"
                    href={
                      link.kind === "task"
                        ? href(["work", link.key || link.id])
                        : href(["knowledge", "pages", link.id])
                    }
                  >
                    {link.kind === "task" ? link.key || link.title : link.title}
                  </a>{" "}
                  {linkLine(link, chrome.statuses)}
                </li>
              ))}
            </ul>
          )}
          {involved && <p className="task-turn-involved">{involved}</p>}
          {story.your_note && (
            <p className="task-turn-note">
              <b>{noteLine(story.your_note)}:</b> “{story.your_note.note}”
            </p>
          )}
          {(story.duration_ms > 0 || viewer.admin) && (
            <p className="task-turn-meta">
              {story.duration_ms > 0 && (
                <span className="task-num">{`worked ${fmtDuration(story.duration_ms)}`}</span>
              )}
              {/* THE TRACE IS HOW IT RAN, which is an admin's to read
                  (ADR-0031): offered to anybody else it is a link to a
                  refusal. */}
              {viewer.admin && (
                <a
                  className="t-link task-trace"
                  href={href(["admin", "live", "turns", story.turn_id])}
                >
                  Trace
                </a>
              )}
            </p>
          )}
        </div>
      </article>
    </li>
  );
}

/**
 * The turn running on this task now: "SWE is on turn 2 · executing · round 7
 * of 25 · for 2m", and the way to watch it.
 */
function LiveRow({
  item,
  row,
  ordinal,
  chrome,
  now,
}: {
  item: WorkItem;
  row: AgentRow;
  ordinal: number;
  chrome: RowChrome;
  now: number;
}) {
  const handle = row.handle ?? "";
  const name = chrome.seatName?.(handle) ?? handle;
  const turn = turnIdOf(row);
  const doing = doingWords(row);
  const since = Date.parse(row.turn?.started_at ?? row.live_call?.started_at ?? "");
  // THE PROFILE CARD'S CLOCK (`fmtElapsed`), seconds under a minute: the
  // list's `shortAge` reads a sub-minute turn as "for 0m" where the seat's
  // own card said "0s" of the same turn.
  const age = Number.isFinite(since) ? fmtElapsed(now - since) : "";
  return (
    <li className="task-entry" data-kind="live">
      <span className="task-entry-avatar">
        <SeatAvatar name={name} kind="agent" size={28} ring={ringOf(activityOf(row))} decorative />
      </span>
      <a
        className="task-live"
        href={turn ? watchHref(turn) : href(["admin", "live"])}
        aria-label={`${name} is on turn ${ordinal} of ${item.key} — watch live`}
      >
        <span className="task-live-dot" aria-hidden="true" />
        <b>{`${name} is on turn ${ordinal}`}</b>
        <span className="truncate">{[doing, age && `for ${age}`].filter(Boolean).join(" · ")}</span>
        <span className="spacer" />
        <span className="task-live-go">
          Watch live <ArrowRightGlyph size="xs" />
        </span>
      </a>
    </li>
  );
}

/** Who one announced change reached, asked when the reader opens it. */
function Reached({ recordId, chrome }: { recordId: string; chrome: RowChrome }) {
  const routing = useQuery("work_routing", { record_id: recordId });
  return (
    <div className="task-routing">
      <QueryState error={routing.error} loading={routing.loading}>
        {routing.data && <Routing answer={routing.data} chrome={chrome} />}
      </QueryState>
    </div>
  );
}

/** One change's recipients, or the reason there are none to show. */
export function Routing({ answer, chrome }: { answer: WorkRoutingAnswer; chrome: RowChrome }) {
  if (answer.recipients.length === 0) {
    return <NobodyWoken answer={answer} />;
  }
  // THE EXCERPT IS USUALLY ONE SENTENCE FOR EVERYBODY, so it is shown once
  // above the list rather than repeated under every name. It CAN differ per
  // recipient — a mention's excerpt is the sentence naming them — and a row
  // that differs still carries its own. COMPARED RAW, flattened only once it
  // is chosen: two different excerpts that flatten to one string are still two.
  const shared = answer.recipients.every((r) => r.excerpt === answer.recipients[0]?.excerpt)
    ? plainText(answer.recipients[0]?.excerpt ?? "")
    : "";
  return (
    <div className="col gap-2">
      <div className="row gap-1 wrap">
        <Tag appearance="outline">
          {answer.recipients.length} {answer.recipients.length === 1 ? "person" : "people"}
        </Tag>
        {answer.addressed > 0 && <AsksTag count={answer.addressed} />}
        {answer.fallback > 0 && (
          <Tag appearance="outline" title="reached only because nobody better was found">
            {answer.fallback} by fallback
          </Tag>
        )}
      </div>
      {shared && <p className="t-caption">{shared}</p>}
      <div className="list">
        {answer.recipients.map((r) => (
          <div key={r.handle} className="thread-entry">
            <div className="row gap-1">
              <SeatChip
                name={chrome.seatName?.(r.handle) ?? r.handle}
                handle={r.handle}
                kind={chrome.seatKind?.(r.handle)}
              />
              <span className="spacer" />
              {/* WHETHER IT ASKED IS ITS OWN MARK, never a tint on the reason:
                  the accent is spent on where the READER is. */}
              {r.addressed && <AsksTag />}
              {/* THE REASON, IN THE THIRD PERSON — this list is about
                  colleagues, and "assigned to you" beside somebody else's
                  name is a sentence about the wrong person. */}
              <Tag appearance="outline">{reasonAbout(r.reason)}</Tag>
              {r.fallback && (
                <Tag appearance="outline" title="nobody better was found">
                  substitute{r.fallback_rank ? ` #${r.fallback_rank}` : ""}
                </Tag>
              )}
            </div>
            {!shared && r.excerpt && <p className="t-caption">{plainText(r.excerpt)}</p>}
          </div>
        ))}
      </div>
      {answer.truncated && (
        <p className="t-caption">
          The list was cut. A change this engine wrote cannot name this many people, so a peer on a
          different build wrote a larger recipient set.
        </p>
      )}
    </div>
  );
}

/** The empty answers, each with its own remedy. */
function NobodyWoken({ answer }: { answer: WorkRoutingAnswer }) {
  if (!answer.held) {
    return (
      <EmptyState
        size="compact"
        icon={<CircleQuestionMarkGlyph size="xl" />}
        title="No such change here"
        description="A record id this node holds no history row for — a link from before a purge, or a reanchor that has not replayed this far."
      />
    );
  }
  if (answer.delivery === "swept") {
    return (
      <EmptyState
        size="compact"
        icon={<ClockGlyph size="xl" />}
        title="Beyond the retention window"
        description={`This change announced something, and it is older than ${
          answer.retained_from ? fmtDateTime(answer.retained_from) : "the inbox horizon"
        } — so whether anybody was reached is a fact this node no longer holds. The history it points at is untouched.`}
      />
    );
  }
  if (answer.delivery === "unknown") {
    return (
      <EmptyState
        size="compact"
        icon={<BellGlyph size="xl" />}
        title="Cannot say"
        description="This change announced something and no recipients remain, and this node was not told how long an inbox is kept — so an absent set cannot be dated."
      />
    );
  }
  return (
    <EmptyState
      size="compact"
      icon={<UsersGlyph size="xl" />}
      title="It announced, and reached nobody"
      description="Every candidate was the person making the change, or has left the company. The notification was formed and had nowhere to go — which is a different fact from a quiet commit, and the only place it is visible."
    />
  );
}
