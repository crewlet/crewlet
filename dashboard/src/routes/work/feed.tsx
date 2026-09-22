/**
 * What was purged, and how much of the log each activity read asks for.
 *
 * THE FEED ITSELF IS GONE FROM HERE. This file opened by saying two readers
 * shared its rows — a project's History lens and the workspace-wide
 * `#/work/history` screen — and both of those render `HistoryView`
 * (`routes/work/History.tsx`) now, which is the sharing that sentence asked
 * for. What was left behind was an `ActivityFeed` with no caller at all, kept
 * alive by one test: a component whose only reader is its own test is
 * indistinguishable to the next person from one whose caller nobody found. The
 * invariant that test protected — that an excerpt is drawn as the prose its
 * markdown renders to — belongs to `describeChange`, and is asserted over the
 * function itself in `lib/work.test.ts`.
 *
 * WHAT IS GENUINELY THIS FILE'S is the purge band, which is not a feed: it
 * draws activity records on a screen that has no log, because a purge is the
 * one change whose subject does not survive it.
 */

import { Card, EmptyValue, Tag } from "@crewlethq/ui";
import { DeleteGlyph } from "@crewlethq/icons/glyphs";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { plainText } from "~/lib/markdown.ts";
import { pageCount } from "~/lib/work.ts";
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
          {/* FLATTENED LIKE EVERY OTHER EXCERPT IN THE PRODUCT. The operator's
              own reason is free text, so it can carry markdown; one that is
              only a rule or whitespace flattens to "" and falls through to the
              word rather than drawing an empty cell. `plainText` rather than
              `describeChange` because a purge has no delta to prefer over it:
              the reason IS the row. */}
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
 * NAMED, WITH REASONS, because a page size is what a count MEANS here: a card
 * that draws `records.length` and cannot page reports this constant on any
 * company busier than it, which is honest only because such a count says `+`
 * when the page was capped — see [pageCount]. The two log surfaces are no
 * longer in that position: `HistoryView` pages, so its number is how much has
 * been LOADED and its size is how much arrives per press rather than a ceiling
 * on what a reader can see.
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
 *    `HistoryView` takes it from its own `embedded` flag, which is the same
 *    fact that decides who publishes the frame's coverage — and until it did,
 *    this entry was a documented size with no caller while the lens asked for
 *    the page screen's hundred.
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
