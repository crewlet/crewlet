/**
 * What a page is connected to, under its comments: what links to it, which
 * agents read it, and what is filed under it. And, for History, every saved
 * revision.
 *
 * # Every list here is the engine's
 *
 * The readers are `page_reads` (every data node's days), the links the page
 * answer's `linked_from` (this node's index), the revisions its history.
 * Nothing is counted in the browser. A list that could not be read says so:
 * `linked_from_status` names an index still on its first pass or a read that
 * failed. One the engine does not serve here (`page_reads` with no usage
 * domain, `linked_from` with no index) is not drawn at all, because "nobody
 * read it" and "this node cannot say" send a curator in opposite directions.
 */

import { useId, useMemo, useState, type ReactNode } from "react";
import { Button, Card } from "@crewlethq/ui";
import { FileTextGlyph, FolderGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { StatusMark } from "~/components/work.tsx";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type {
  LinkedFromStatus,
  PageBacklinks,
  PageLink,
  PageReadsAnswer,
  TaskLink,
} from "~/contract/pages.ts";
import { fmtDateCompact, fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { readCount, readVia } from "~/lib/pageReads.ts";
import { shortAge } from "~/lib/seats.ts";
import { itemPath } from "~/lib/work.ts";
import type { PageRevision, PageSummary } from "~/protocol/index.ts";
import { Face, pagePath, type Who } from "../parts.tsx";

/** How many rows a connection draws before "Show more": a glance, the rest a press away. */
const CONNECTION_ROWS = 3;

/** The first rows of a list, and the button that shows the rest. */
function useShown<T>(rows: T[], first: number): { rows: T[]; more: ReactNode; listID: string } {
  const [all, setAll] = useState(false);
  // THE LIST THE BUTTON GROWS, named, and whether it has: a reader hears
  // "expanded" and which list, not only a count.
  const listID = useId();
  const hidden = rows.length - first;
  return {
    listID,
    rows: all ? rows : rows.slice(0, first),
    more:
      hidden > 0 ? (
        <Button
          size="small"
          variant="ghost"
          aria-expanded={all}
          aria-controls={listID}
          onClick={() => setAll(!all)}
        >
          {all ? "Show fewer" : `Show ${hidden} more`}
        </Button>
      ) : null,
  };
}

/** One connection: a card named by its heading, with a count or a window beside it. */
function Connection({
  title,
  count,
  window,
  children,
}: {
  title: string;
  count?: number | undefined;
  window?: string | undefined;
  children: ReactNode;
}) {
  const id = useId();
  return (
    <section className="knowledge-connection" aria-labelledby={id}>
      <Card padding="sm">
        <Card.Header count={count} subtitle={window}>
          <Card.Title>
            <span id={id}>{title}</span>
          </Card.Title>
        </Card.Header>
        {children}
      </Card>
    </section>
  );
}

/** The three connections, side by side where there is room. */
export function Connections({
  links,
  linksStatus,
  reads,
  readsError,
  under,
  underTotal,
  who,
  now,
}: {
  links?: PageBacklinks | undefined;
  linksStatus?: LinkedFromStatus | undefined;
  reads: PageReadsAnswer | null;
  readsError: QueryErrorCode | null;
  /** The first window of the pages filed under this one, by title. */
  under: PageSummary[];
  underTotal: number;
  who: Who;
  now: number;
}) {
  const id = useId();
  return (
    <section className="knowledge-connections" aria-labelledby={id}>
      <h2 className="knowledge-section-title" id={id}>
        Connections
      </h2>
      <div className="knowledge-connections-grid">
        <LinkedFrom links={links} status={linksStatus} />
        <ReadBy reads={reads} error={readsError} who={who} now={now} />
        <UnderThisPage pages={under} total={underTotal} />
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Linked from
// ---------------------------------------------------------------------------

/** How a task links a page, in a reader's words. */
const TASK_VIA: Record<string, string> = {
  linked_page: "linked as a page",
  description: "cited in the description",
};

/** One link: tasks first, in key order, then pages, which are the engine's orders. */
type LinkRow = { kind: "task"; t: TaskLink } | { kind: "page"; p: PageLink };

/** Why the answering node sent no list, in a curator's words. */
const LINKS_STATUS: Record<LinkedFromStatus, string> = {
  building: "Still indexing. Look again in a few minutes.",
  unavailable: "Couldn’t load links. Reload to try again.",
};

/**
 * The tasks and pages that point here, or why this node cannot say. Never an
 * empty list for that: "nothing links here" is a claim only a finished index
 * can make.
 */
function LinkedFrom({
  links,
  status,
}: {
  links?: PageBacklinks | undefined;
  status?: LinkedFromStatus | undefined;
}) {
  const rows = useMemo<LinkRow[]>(
    () =>
      links
        ? [
            ...links.tasks.map((t) => ({ kind: "task" as const, t })),
            ...links.pages.map((p) => ({ kind: "page" as const, p })),
          ]
        : [],
    [links],
  );
  const { rows: shown, more, listID } = useShown(rows, CONNECTION_ROWS);
  if (!links) {
    if (!status) return null;
    return (
      <Connection title="Linked from">
        <p className="knowledge-note" role="status">
          {LINKS_STATUS[status] ?? LINKS_STATUS.unavailable}
        </p>
      </Connection>
    );
  }
  const total = links.tasks_total + links.pages_total;
  return (
    <Connection title="Linked from" count={total}>
      {rows.length === 0 ? (
        <p className="knowledge-note">No page or task links here.</p>
      ) : (
        <>
          <ul className="knowledge-links" role="list" id={listID}>
            {shown.map((row) =>
              row.kind === "task" ? (
                <li key={`t:${row.t.id}`}>
                  <a
                    className="knowledge-link"
                    // BY ITS ADDRESS ([itemPath]): a key another task claimed
                    // first opens that task, not the one linking here.
                    href={href(itemPath(row.t))}
                    title={row.t.via.map((v) => TASK_VIA[v] ?? v).join(" and ")}
                  >
                    <StatusMark status={row.t.status} />
                    <span className="knowledge-link-key">{row.t.key}</span>
                    <span className="knowledge-link-title">{row.t.title}</span>
                  </a>
                </li>
              ) : (
                <li key={`p:${row.p.id}`}>
                  <a className="knowledge-link" href={href(pagePath(row.p.id))}>
                    <FileTextGlyph size="sm" />
                    <span className="knowledge-link-title">{row.p.title}</span>
                  </a>
                </li>
              ),
            )}
          </ul>
          {more}
          {total > rows.length && (
            <p className="knowledge-note">
              {rows.length} of {total}
            </p>
          )}
        </>
      )}
    </Connection>
  );
}

// ---------------------------------------------------------------------------
// Read by agents
// ---------------------------------------------------------------------------

/**
 * Who read the page: each seat and the way it read it, newest first, which is
 * the turn it read it in or the search it was a hit for.
 */
function ReadBy({
  reads,
  error,
  who,
  now,
}: {
  reads: PageReadsAnswer | null;
  error: QueryErrorCode | null;
  who: Who;
  now: number;
}) {
  const readers = useMemo(() => reads?.readers ?? [], [reads]);
  const { rows, more, listID } = useShown(readers, CONNECTION_ROWS);
  // NOT SERVED HERE (a node with no usage domain) is not "nobody read it".
  if (error === "unknown_query") return null;
  return (
    <Connection
      title="Read by agents"
      // THE WINDOW IS STATED: a count beside a list with no window reads as
      // the list's size.
      window={reads ? `last ${plural(reads.days, "day")}` : undefined}
    >
      {error ? (
        <p className="knowledge-note">Who read it could not be read ({error}).</p>
      ) : !reads ? (
        <p className="knowledge-note">Reading&hellip;</p>
      ) : readers.length === 0 ? (
        <p className="knowledge-note">
          No agent has read it in the last {plural(reads.days, "day")}.
        </p>
      ) : (
        <>
          <ul className="knowledge-readers" role="list" id={listID}>
            {rows.map((r) => {
              const seat = who(r.handle);
              const via = readVia(r);
              const count = readCount(r);
              return (
                <li key={`${r.handle}/${r.via}`} className="knowledge-reader">
                  <Face handle={r.handle} who={who} size="sm" />
                  <span className="knowledge-reader-text">
                    {/* TWO LINES: who and how often, then how and when. The way
                        read is cut with an ellipsis (whole in its title) so the
                        age at its end never wraps onto a third line. */}
                    <span className="knowledge-reader-head">
                      <a
                        className="knowledge-reader-name"
                        href={href(["company", "org", "seats", r.handle])}
                      >
                        {seat.name}
                      </a>
                      {count && <span className="knowledge-reader-count">{count}</span>}
                    </span>
                    <span className="knowledge-reader-via">
                      {r.last_turn_id ? (
                        <a
                          className="knowledge-reader-how"
                          href={href(["live", "turns", r.last_turn_id])}
                          title={via}
                        >
                          {via}
                        </a>
                      ) : (
                        <span className="knowledge-reader-how" title={via}>
                          {via}
                        </span>
                      )}
                      <span className="knowledge-reader-when">
                        {" · "}
                        <time dateTime={r.last_at} title={fmtDateTime(r.last_at)}>
                          {relTime(r.last_at, now)}
                        </time>
                      </span>
                    </span>
                  </span>
                </li>
              );
            })}
          </ul>
          {more}
          {reads.readers_total > readers.length && (
            <p className="knowledge-note">
              The {readers.length} most recent of {reads.readers_total}.
            </p>
          )}
        </>
      )}
      {reads && reads.elided > 0 && (
        // THE CAP IS STATED, never silent, and COMPANY-WIDE: the engine counts
        // what the cap dropped across every page, so the list MAY be short.
        <p className="knowledge-note">Some reads may be missing.</p>
      )}
    </Connection>
  );
}

// ---------------------------------------------------------------------------
// Under this page
// ---------------------------------------------------------------------------

/** The pages filed under this one: the first window by title, and the count. */
function UnderThisPage({ pages, total }: { pages: PageSummary[]; total: number }) {
  const { rows, more, listID } = useShown(pages, CONNECTION_ROWS);
  return (
    <Connection title="Under this page" count={total}>
      {pages.length === 0 ? (
        <p className="knowledge-note">Nothing is filed under it.</p>
      ) : (
        <>
          <ul className="knowledge-links" role="list" id={listID}>
            {rows.map((child) => (
              <li key={child.id}>
                <a className="knowledge-link" href={href(pagePath(child.id))}>
                  {child.folder ? <FolderGlyph size="sm" /> : <FileTextGlyph size="sm" />}
                  <span className="knowledge-link-title">{child.title}</span>
                </a>
              </li>
            ))}
          </ul>
          {more}
          {total > pages.length && (
            <p className="knowledge-note">
              {pages.length} of {total}
            </p>
          )}
        </>
      )}
    </Connection>
  );
}

// ---------------------------------------------------------------------------
// Revisions
// ---------------------------------------------------------------------------

/** "2h" inside a day, "Sep 18" past one: a revision's age in a narrow column. */
function when(at: string, now: number): string {
  return now - Date.parse(at) < 86_400_000 ? shortAge(at, now) : fmtDateCompact(at, now);
}

/**
 * Every saved revision, newest first, in History's left column; a press reads
 * that one in the page. The note kept with a save is its title.
 */
export function Revisions({
  history,
  open,
  onOpen,
  who,
  now,
}: {
  history: PageRevision[];
  open: number;
  onOpen: (version: number) => void;
  who: Who;
  now: number;
}) {
  const id = useId();
  return (
    <nav className="knowledge-revisions" aria-labelledby={id}>
      <h2 className="knowledge-side-title" id={id}>
        Revisions
      </h2>
      {history.length === 0 ? (
        <p className="knowledge-note">Only this version exists.</p>
      ) : (
        <ul className="knowledge-revision-list" role="list">
          {history.map((rev) => {
            const by = rev.author ?? "";
            return (
              <li key={rev.version}>
                <button
                  type="button"
                  className="knowledge-revision"
                  aria-pressed={rev.version === open}
                  title={rev.message || "No note was kept with this save"}
                  onClick={() => onOpen(rev.version)}
                >
                  {by ? (
                    <Face handle={by} who={who} />
                  ) : (
                    <span className="knowledge-revision-engine" />
                  )}
                  <span className="truncate">
                    rev {rev.version} · {by ? who(by).name : "the engine"}
                  </span>
                  <time
                    className="knowledge-revision-when"
                    dateTime={rev.created_at}
                    title={fmtDateTime(rev.created_at)}
                  >
                    {when(rev.created_at, now)}
                  </time>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </nav>
  );
}
