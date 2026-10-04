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
 * # A turn card is the task's own account
 *
 * `work_item_turns` is read from the tracker's rows — the account the task's
 * cost sums — so "Turn 3" on a card and "3 turns" in the cost panel are one
 * count, and a turn that ran months ago on a node that has since left still
 * says what it did. Its Trace link is the event history's, which keeps a
 * month; the card does not depend on it.
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
  CheckGlyph,
  ChevronDownGlyph,
  ChevronRightGlyph,
  ChevronUpGlyph,
  CircleQuestionMarkGlyph,
  ClockGlyph,
  CornerDownLeftGlyph,
  UsersGlyph,
  XGlyph,
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
import { renderMarkdown, plainText } from "~/lib/markdown.ts";
import {
  fmtCount,
  fmtDateCompact,
  fmtDateTime,
  fmtDuration,
  fmtElapsed,
  fmtMinute,
  humanize,
} from "~/lib/format.ts";
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
  WorkItem,
  WorkItemTurn,
  WorkRoutingAnswer,
} from "~/protocol/index.ts";
import { ACTIVITY_TABS, timeline, type ActivityTab, type Entry, type Source } from "./timeline.ts";
import { usePaged } from "~/lib/usePaged.ts";

/** How often the newest page of each history is asked again: nothing pushes them. */
const ACTIVITY_POLL_MS = 30_000;

/** The pages each history is read in — the engine's own ceilings. */
const CHANGES_PAGE = 50;
const COMMENTS_PAGE = 50;
const TURNS_PAGE = 20;

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
  // BY ITS ID, which names this task and no other: a key two tasks hold
  // reads the history and the comments of whichever claimed it first.
  const changesAsked = { task: item.id, limit: CHANGES_PAGE };
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
  const commentsAsked = { item: item.id, limit: COMMENTS_PAGE };
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
  const turnsAsked = { id: item.key, limit: TURNS_PAGE };
  const turns = usePaged(
    useQuery("work_item_turns", turnsAsked, { pollMs: ACTIVITY_POLL_MS }),
    turnsAsked,
    (cursor) =>
      socket.query(
        "work_item_turns",
        withFloor("work_item_turns", JSON.stringify({ ...turnsAsked, cursor })),
      ),
    pickTurns,
    turnId,
    nextCursor,
  );
  const paged = { changes, comments, turns };

  // WHAT A DELTA'S OTHER END IS CALLED, from every page of the change feed —
  // a relation, a re-parent and a cascade name the other task by its id.
  const labels = useMemo<RowChrome & LabelContext>(() => {
    const keys: Record<string, string> = {};
    for (const answer of changes.answers) Object.assign(keys, answer.keys ?? {});
    return { ...chrome, taskKey: (id: string) => keys[id] ?? "" };
  }, [chrome, changes.answers]);

  const drawn = useMemo(
    () =>
      timeline(tab, {
        changes: { items: changes.items, more: changes.more },
        comments: { items: comments.items, more: comments.more },
        turns: { items: turns.items, more: turns.more },
      }),
    [tab, changes.items, changes.more, comments.items, comments.more, turns.items, turns.more],
  );
  const shown: readonly Source[] =
    tab === "all" ? ["changes", "comments", "turns"] : [tab === "turns" ? "turns" : tab];
  const loading = shown.some((source) => paged[source].loading && paged[source].items.length === 0);
  const failed = shown.map((source) => paged[source]).filter((read) => read.error !== null);
  const paging = shown.some((source) => paged[source].paging);
  const pageError = shown.map((source) => paged[source].pageError).find(Boolean) ?? null;
  const runningHere = live && (tab === "all" || tab === "turns") ? live : null;

  // THE NUMBER THE RUNNING TURN WILL HAVE: its own, where a parked segment was
  // already charged, and otherwise the one after every turn counted so far.
  const liveOrdinal = useMemo(() => {
    const id = live?.turn?.turn_id ?? live?.live_call?.turn_id ?? "";
    const charged = turns.items.find((t) => t.turn_id === id);
    if (charged?.ordinal) return charged.ordinal;
    return (item.spend?.turns ?? 0) + 1;
  }, [live, turns.items, item.spend?.turns]);

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

      {failed.map((read, i) => (
        <QueryState key={i} error={read.error} refusal={read.refusal} loading={false} />
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
          {pageError && <QueryState error={pageError} refusal={null} loading={false} />}
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
                {`Replying to ${chrome.seatName?.(replyTo.author) ?? replyTo.author}`}
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
const pickTurns = (a: { turns: WorkItemTurn[] }) => a.turns ?? [];
const turnId = (t: WorkItemTurn) => t.turn_id;

function FeedEntry({
  entry,
  item,
  chrome,
  index,
  now,
  onReply,
}: {
  entry: Entry;
  item: WorkItem;
  chrome: RowChrome & LabelContext;
  index: OrgIndex;
  now: number;
  onReply: (c: WorkComment) => void;
}) {
  switch (entry.kind) {
    case "change":
      return <ChangeRow record={entry.record} item={item} chrome={chrome} now={now} />;
    case "comment":
      return (
        <CommentRow
          comment={entry.comment}
          chrome={chrome}
          index={index}
          now={now}
          onReply={onReply}
        />
      );
    case "turn":
      return <TurnCard turn={entry.turn} chrome={chrome} now={now} />;
  }
}

/**
 * Who a record names as its author (`iam.ActorFor`): the seat a bound person
 * writes as, or the login of anybody bound to none.
 */
function actorOf(record: WorkActivityRecord, chrome: RowChrome): string {
  const who = record.actor || "";
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
      if (to) return { what: `moved it to ${statusLabel(to, chrome.statuses)}`, why: "" };
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
              <a className="prose-link task-when" href={href(["live", "turns", record.turn_id])}>
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
  chrome,
  index,
  now,
  onReply,
}: {
  comment: WorkComment;
  chrome: RowChrome;
  index: OrgIndex;
  now: number;
  onReply: (c: WorkComment) => void;
}) {
  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the identity directory
  // binds to a seat comments AS that seat, so the author is who they are;
  // anybody bound to none is their login, which is the name their own record
  // is kept under. A comment with no author at all is the engine's.
  const handle = comment.author;
  const person = handle ? index.byHandle.get(handle) : undefined;
  const name = handle ? (chrome.seatName?.(handle) ?? handle) : "The engine";
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

/** What a phase is called on a turn card. */
const PHASE_WORD: Record<string, string> = {
  execute: "Execute",
  review: "Review",
  sandbox: "Coding run",
  subagent: "Workers",
  plan: "Plan",
  judge: "Judge",
  onboarding: "Onboarding",
  auxiliary: "Auxiliary",
};

/** The pills a turn wears for how it went — only for what is worth a pill. */
export function turnPills(
  turn: WorkItemTurn,
): { label: string; variant: "warning" | "danger" | "info" | "neutral" }[] {
  const out: { label: string; variant: "warning" | "danger" | "info" | "neutral" }[] = [];
  if (turn.sent_back) {
    out.push({
      label:
        turn.sent_back === 1
          ? "sent back for another pass"
          : `sent back ${turn.sent_back} times for another pass`,
      variant: "warning",
    });
  }
  switch (turn.outcome) {
    case "failed":
      // THE PILL NAMES THE STEP THAT BROKE where the record says: "failed"
      // alone beside a row of ticked phases left the reader to guess.
      out.push({
        label: turn.failed_in
          ? `failed in ${(PHASE_WORD[turn.failed_in] ?? humanize(turn.failed_in)).toLowerCase()}`
          : "failed",
        variant: "danger",
      });
      break;
    case "suspended":
      out.push({ label: "parked on a coding run", variant: "info" });
      break;
    case "skipped":
      out.push({ label: "skipped — nothing to do", variant: "neutral" });
      break;
  }
  return out;
}

/** One agent turn charged to the task, as the card the artboard draws. */
export function TurnCard({
  turn,
  chrome,
  now,
}: {
  turn: WorkItemTurn;
  chrome: RowChrome;
  now: number;
}) {
  const name = chrome.seatName?.(turn.seat) ?? turn.seat;
  const kind = chrome.seatKind?.(turn.seat) === "human" ? "human" : "agent";
  return (
    <li className="task-entry" data-kind="turn">
      <span className="task-entry-avatar">
        <SeatAvatar name={name} kind={kind} size={28} decorative />
      </span>
      <article className="task-turn" aria-label={turn.ordinal ? `Turn ${turn.ordinal}` : "A turn"}>
        <header className="task-turn-head">
          <b>{name}</b>
          <span className="muted">
            {turn.ordinal ? `ran turn ${turn.ordinal}` : "continued a turn charged elsewhere"}
          </span>
          <span className="task-phases">
            {turn.phases.map((phase, i) => {
              // A TICK SAYS THE PHASE RAN AND HELD; the one a failed turn
              // broke in wears the cross, in the danger tone, and says so.
              const broke = turn.outcome === "failed" && turn.failed_in === phase;
              return (
                <span key={phase} className="row gap-1">
                  {i > 0 && <span className="task-phase-join" aria-hidden="true" />}
                  <span className="task-phase" data-failed={broke || undefined}>
                    {broke ? <XGlyph size="xs" /> : <CheckGlyph size="xs" />}
                    {PHASE_WORD[phase] ?? humanize(phase)}
                    {broke && <span className="sr-only"> (failed)</span>}
                  </span>
                </span>
              );
            })}
          </span>
          {turnPills(turn).map((pill) => (
            <Tag key={pill.label} variant={pill.variant} size="sm">
              {pill.label}
            </Tag>
          ))}
          <span className="spacer" />
          <time className="task-when" dateTime={turn.at} title={fmtDateTime(turn.at)}>
            {fmtMinute(turn.at)}
          </time>
        </header>
        <div className="task-turn-body">
          {/* A TURN AN OLDER BUILD RECORDED CARRIES NO ACCOUNT, and says so
              in one quiet line: a card per turn repeating the reason in full
              body text read as the turn's content. The reason is the title. */}
          {turn.summary ? (
            <p className="task-turn-summary">{turn.summary}</p>
          ) : (
            <p
              className="task-turn-nosummary"
              title="Recorded before a turn's own account was kept. The trace has the detail while the event history holds it."
            >
              No summary recorded — see the trace
            </p>
          )}
          {turn.review && (
            <div className="task-turn-review">
              <CornerDownLeftGlyph size="sm" />
              <p>
                <b>Reviewer:</b> {turn.review}
              </p>
            </div>
          )}
          {(turn.tools ?? []).length > 0 && (
            <ul className="task-turn-tools" aria-label="Tools it called">
              {(turn.tools ?? []).map((tool) => (
                <li key={tool.name} className="task-tool mono">
                  {tool.calls > 1 ? `${tool.name} ×${tool.calls}` : tool.name}
                </li>
              ))}
            </ul>
          )}
          {/* WHAT IT TOOK, on its own line under the chips and read from the
              left, as the artboard sets it: a figure floated to the far edge
              of a row of chips reads as belonging to the last chip. */}
          <p className="task-turn-meta">
            <span className="task-num">
              {`${fmtDuration(turn.wall_ms)} · ${fmtCount(turn.tokens)} tokens`}
            </span>
            <a className="t-link task-trace" href={href(["live", "turns", turn.turn_id])}>
              Trace
            </a>
          </p>
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
  const turn = row.turn?.turn_id ?? row.live_call?.turn_id ?? "";
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
        href={turn ? href(["live", "turns", turn]) : href(["live"])}
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
      <QueryState error={routing.error} refusal={routing.refusal} loading={routing.loading}>
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
