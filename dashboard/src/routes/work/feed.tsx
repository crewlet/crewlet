/**
 * What changed, as a card beside the work — and what was purged, which has no
 * rows at all.
 *
 * TWO READERS SHARE THESE. A project's History lens and the workspace-wide
 * `#/work/history` screen ask `work_activity` with different scopes and draw
 * the same rows, and the trash's purge band is the same records filtered
 * differently. Written once, so a change to how a delta reads reaches every
 * surface that reads one.
 *
 * A DIFFERENT QUESTION FROM WHAT IS ON THE BOARD: the feed is ordered by the
 * LOG rather than by anything the rows sort on, so a change that moved nothing
 * on screen is still visible.
 */

import { Card, EmptyValue, Tag } from "@crewlethq/ui";
import { DeleteGlyph, TimelineGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { RowChrome } from "~/components/work.tsx";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { plainText } from "~/lib/markdown.ts";
import { describeChange, pageCount, pageNote } from "~/lib/work.ts";
import type { WorkActivityAnswer, WorkActivityRecord } from "~/protocol/index.ts";

/**
 * What was PURGED, which is the half of the trash with no rows.
 *
 * A removal hides a task and a restore brings it back at any age. A purge
 * destroys the rows — so there is nothing for the grid above to list, and this
 * history entry is the only evidence the work ever existed. Drawing it as a
 * grid row would be drawing a task that is gone; drawing it nowhere would make
 * an emptied trash indistinguishable from a company that has removed nothing.
 *
 * ITS OWN BAND, SAYING IT IS IRREVERSIBLE, because every other row on this
 * screen carries a way back and these do not. And the key is rendered from
 * whatever the entry itself holds: a purged task has no row for the feed to
 * resolve a key against, which is exactly the point.
 */
/**
 * `records` are the purges; `answer` is the page they were filtered OUT of.
 * Both, because the cap is the answer's: a tombstone page spent on removals and
 * restores can end before the older purges do, and a band counting only its own
 * subset would report that page boundary as the number of purges this company
 * has ever made.
 */
export function PurgeBand({
  records,
  answer,
  now,
}: {
  records: WorkActivityRecord[];
  answer?: WorkActivityAnswer;
  now: number;
}) {
  if (records.length === 0) return null;
  const more = !!answer?.next_cursor;
  return (
    <Card padding="none">
      <Card.Header
        icon={<DeleteGlyph size="sm" />}
        count={pageCount(records.length, more)}
        subtitle={
          "A purge destroys the rows. These cannot be restored — what survives is that it happened, to which key, by whom, and the reason the operator gave." +
          (more ? " Older purges exist beyond this page of the history." : "")
        }
      >
        <Card.Title>Purged</Card.Title>
      </Card.Header>
      {records.map((record) => (
        <div key={record.id} className="work-feed-row">
          <span className="work-feed-when" title={fmtDateTime(record.at)}>
            {relTime(record.at, now)}
          </span>
          <span className="work-feed-kind">
            <Tag variant="danger" appearance="outline">
              irreversible
            </Tag>
          </span>
          <span className="mono">
            {/* NO LINK. The task is gone, so an anchor here would lead to a
                NotFound on every row — and the key is the entry's own,
                because there is no task row left to resolve one from. */}
            {record.subject_key || <EmptyValue label="No key recorded" />}
          </span>
          {/* FLATTENED LIKE THE FEED'S. The same record reaches both this band
              and the activity feed below it, and an excerpt rendered two ways on
              one screen is the drift a shared helper exists to stop. The
              operator's own reason is free text, so it can carry markdown too;
              one that is only a rule or whitespace flattens to "" and falls
              through to the word rather than drawing an empty cell. */}
          <span className="work-feed-what truncate">
            {plainText(record.excerpt ?? "") || "purged"}
          </span>
          <span className="work-feed-who">{record.actor || "the engine"}</span>
        </div>
      ))}
    </Card>
  );
}

/**
 * How much of the log each activity read on this screen asks for.
 *
 * NAMED, WITH REASONS, because a page size is what a count MEANS here: these
 * cards draw `records.length`, so on any company busier than these numbers the
 * figure a reader sees is this constant. That is honest only because the count
 * now says `+` when the page was capped — see [pageCount].
 *
 * The engine's own maximum for one page is 200 (`tracker.MaxActivityRows`) and
 * the audit screen takes all of it, because that screen IS the record. Two of
 * these are companions to something else on the page and are sized to the room
 * they have; the third is not decoration at all.
 *
 *  - `page` — the HISTORY SCREEN's own, which is the one read here that is not
 *    a companion to anything: the log is the whole page, it carries a time
 *    axis and facets over what it holds, and a hundred rows is what fills a
 *    screen of them twice over without asking for the engine's whole ceiling
 *    on every poll.
 *  - `board` — a project's History lens, which sits under a header and beside
 *    two other lenses: a screenful of one-line rows, re-read once a minute.
 *  - `rail` — above the fold in a project's rail, beside a census and three
 *    other cards: a glance, not a page.
 *  - `trash` — THE ENGINE'S MAXIMUM, and load-bearing. The trash grid asks for
 *    100 rows (`buildItemsParams`) and resolves each row's "Removed" and
 *    "Removed by" out of THIS page, so a restore or a purge on it is budget
 *    spent on a row the grid is not showing. At 100 the page could not cover a
 *    full grid page even in principle, and the surplus rows drew "its removal is
 *    older than the loaded history" on an ordinary, recent trash. 200 is the
 *    most one read can ask for and still cannot guarantee it — which is why that
 *    dash stays, and why the band's count carries the `+`.
 */
export const FEED_PAGE = { page: 100, board: 20, rail: 8, trash: 200 } as const;

/**
 * What happened in this container lately.
 *
 * A DIFFERENT QUESTION from what is on the board: the feed is ordered by the
 * log rather than by anything the rows sort on, so a change that moved nothing
 * on screen — a comment, a watcher, a quiet re-type — is still visible. It
 * renders the AUTHORED instant, which is what the writer's clock said and what
 * "yesterday" has to keep meaning.
 */
/**
 * THE WHOLE ANSWER, not its rows. The count, the note and the coverage line are
 * three readings of one read, and a component handed only `records` can state
 * none of them — which is how this card came to draw its own page size as though
 * it were the company's history.
 */
export function ActivityFeed({
  records,
  answer,
  now,
  chrome = {},
}: {
  records: WorkActivityRecord[];
  /** The answer those rows came out of, where the caller holds one. */
  answer?: WorkActivityAnswer;
  now: number;
  /** The company's own words for a status, a type, a tag and a handle. A feed
   *  row's delta is stored as the LOG's text — `in_review`, a bare handle, a
   *  whole RFC3339 instant — and this is what turns it into what the rest of the
   *  product says. */
  chrome?: RowChrome;
}) {
  if (records.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header
        icon={<TimelineGlyph size="sm" />}
        // A NON-EMPTY CURSOR IS THE ENGINE'S OWN "there is at least one more":
        // `readActivity` asks for limit+1 rows and mints a cursor only when the
        // extra one came back. Never derived from `records.length === limit`,
        // which is wrong on the boundary in both directions.
        count={pageCount(records.length, !!answer?.next_cursor)}
        // TWO DIFFERENT FACTS, and never one figure. `next_cursor` is a page
        // boundary, which a reader resolves by looking further back. `complete`
        // is the state-log read's COVERAGE verdict — this node holds records it
        // cannot DECODE covering this feed — which a reader resolves with a build
        // that can read them. Folded into the count, a decode gap would be
        // reported as "there are more rows" and send somebody scrolling for
        // commits that are not missing.
        //
        // `|| undefined`, never `""`: an empty subtitle still renders its own
        // element.
        subtitle={
          [
            pageNote(records.length, !!answer?.next_cursor, "change"),
            answer?.complete === false
              ? "This node holds records it cannot read, so changes may be missing from this list."
              : "",
          ]
            .filter(Boolean)
            .join(" ") || undefined
        }
      >
        <Card.Title>Recent activity</Card.Title>
      </Card.Header>
      {records.map((record) => (
        <div key={record.id} className="work-feed-row">
          <span className="work-feed-when" title={fmtDateTime(record.at)}>
            {relTime(record.at, now)}
          </span>
          <span className="work-feed-kind">
            <Tag appearance="outline">{record.kind.replaceAll("_", " ")}</Tag>
          </span>
          <span>
            {record.subject_key ? (
              <a className="mono t-link" href={href(["work", record.subject_key])}>
                {record.subject_key}
              </a>
            ) : (
              <EmptyValue label="No work item" />
            )}
          </span>
          <span className="work-feed-what truncate">{describeChange(record, chrome)}</span>
          <span className="work-feed-who">{record.actor || "the engine"}</span>
        </div>
      ))}
    </Card>
  );
}
