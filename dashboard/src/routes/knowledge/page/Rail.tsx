/**
 * A page's rail: where to go on it, who read it, what points at it, and how
 * it got here — the approved Knowledge page's right column.
 *
 * # Every list here is the engine's
 *
 * The readers are `page_reads` (every node's days, the company's today), the
 * backlinks the page answer's `linked_from` (this node's index), the revisions
 * its history. Nothing is counted in the browser: a section that could not be
 * read says so — `linked_from_status` names an index still on its first lap
 * or a read that failed — and one the engine does not serve here —
 * `page_reads` with no usage domain, `linked_from` with no index — is simply
 * not drawn, rather than drawn empty, because "nobody read it" and "this node
 * cannot say" send a curator in opposite directions.
 */

import { useEffect, useId, useMemo, useState, type ReactNode } from "react";
import { AvatarStack, Button, cx } from "@crewlethq/ui";
import { FileTextGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { ClockText } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { StatusMark } from "~/components/work.tsx";
import { SeatAvatar, seatBadge } from "~/ui/SeatAvatar.tsx";
import { fmtDateCompact, fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { readCount, readVia } from "~/lib/pageReads.ts";
import { shortAge } from "~/lib/seats.ts";
import { itemPath } from "~/lib/work.ts";
import { REDUCED_MOTION } from "~/components/time/SpanBar.tsx";
import type { Heading } from "~/lib/markdown.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type {
  LinkedFromStatus,
  PageBacklinks,
  PageLink,
  PageReadsAnswer,
  TaskLink,
} from "~/contract/pages.ts";
import type { LogRefusal, PageRevision, PageSummary, QueryRefusal } from "~/protocol/index.ts";

type Who = (handle: string) => { name: string; kind?: "agent" | "human" };

/** How many readers, backlinks and revisions a section draws before "Show all". */
const RAIL_ROWS = 5;

/**
 * One rail section: a small label and what it lists. `window` names the span
 * of time the list covers, drawn after the label and part of the section's
 * name — a count beside a list with no stated window reads as the list's size.
 */
function RailSection({
  label,
  id,
  window,
  children,
}: {
  label: string;
  id: string;
  window?: string | undefined;
  children: ReactNode;
}) {
  return (
    <section className="kpage-rail-section" aria-labelledby={id}>
      <h2 className="kpage-rail-label" id={id}>
        {label}
        {window && <span className="kpage-rail-window"> · {window}</span>}
      </h2>
      {children}
    </section>
  );
}

/** The first `RAIL_ROWS` of a list, and the button that shows the rest. */
function useShown<T>(rows: T[]): { rows: T[]; more: ReactNode; listID: string } {
  const [all, setAll] = useState(false);
  // THE LIST THE BUTTON GROWS, named, and whether it has: a reader hears
  // "expanded" and which list, not only a count.
  const listID = useId();
  const hidden = rows.length - RAIL_ROWS;
  return {
    listID,
    rows: all ? rows : rows.slice(0, RAIL_ROWS),
    more:
      hidden > 0 ? (
        <Button
          size="small"
          variant="ghost"
          className="kpage-rail-more"
          aria-expanded={all}
          aria-controls={listID}
          onClick={() => setAll(!all)}
        >
          {all ? "Show fewer" : `Show ${hidden} more`}
        </Button>
      ) : null,
  };
}

// ---------------------------------------------------------------------------
// On this page
// ---------------------------------------------------------------------------

/**
 * The heading the reader is in: the last one whose top has scrolled above a
 * line a fifth of the way down the viewport — the one whose section fills
 * the screen, which is what "you are here" means for a document.
 */
function useCurrentHeading(ids: string[]): string {
  const [current, setCurrent] = useState(ids[0] ?? "");
  const key = ids.join(" ");
  useEffect(() => {
    const els = ids
      .map((id) => document.getElementById(id))
      .filter((el): el is HTMLElement => el !== null);
    if (els.length === 0 || typeof IntersectionObserver === "undefined") return;
    const visible = new Map<string, boolean>();
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          visible.set(entry.target.id, entry.boundingClientRect.top < window.innerHeight * 0.2);
        }
        const above = els.filter((el) => el.getBoundingClientRect().top < window.innerHeight * 0.2);
        setCurrent((above[above.length - 1] ?? els[0])!.id);
      },
      { rootMargin: "0px 0px -80% 0px", threshold: [0, 1] },
    );
    for (const el of els) observer.observe(el);
    return () => observer.disconnect();
    // THE IDS AS ONE STRING: a new array per render is not a new outline.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return current;
}

/**
 * "On this page": the body's outline, each entry taking the reader to its
 * section.
 *
 * BUTTONS, NOT `#fragment` LINKS: the address's fragment IS the route here
 * (`#/knowledge/pages/{id}`), so a fragment link to a heading would navigate
 * away from the page it names. The press scrolls the heading into view and
 * moves focus to it, which is what a skip link does for a keyboard.
 */
export function OnThisPage({ headings }: { headings: Heading[] }) {
  const top = Math.min(...headings.map((h) => h.level));
  // THE TWO TOP LEVELS THE DOCUMENT USES — its sections and their parts. A
  // third would turn the outline into the document.
  const shown = headings.filter((h) => h.level <= top + 1);
  const current = useCurrentHeading(shown.map((h) => h.id));
  if (shown.length < 2) return null;
  return (
    <RailSection label="On this page" id="kpage-rail-toc">
      <ul className="kpage-toc" role="list">
        {shown.map((h) => (
          <li key={h.id}>
            <button
              type="button"
              className={cx("kpage-toc-link", h.level > top && "is-nested")}
              aria-current={h.id === current ? "location" : undefined}
              onClick={() => {
                const el = document.getElementById(h.id);
                if (!el) return;
                const still =
                  typeof matchMedia === "function" && matchMedia(REDUCED_MOTION).matches;
                el.scrollIntoView({ behavior: still ? "auto" : "smooth", block: "start" });
                el.setAttribute("tabindex", "-1");
                el.focus({ preventScroll: true });
              }}
            >
              {h.text}
            </button>
          </li>
        ))}
      </ul>
    </RailSection>
  );
}

// ---------------------------------------------------------------------------
// Read by agents
// ---------------------------------------------------------------------------

/**
 * Who read the page: each seat and the way it read it, newest first — the
 * turn it read it in, or the search it was a hit for.
 */
export function ReadBy({
  reads,
  error,
  refusal,
  who,
}: {
  reads: PageReadsAnswer | null;
  error: QueryErrorCode | null;
  /** Why the read was refused, beside `error` — the grant that would admit the reader. */
  refusal: QueryRefusal | LogRefusal | null;
  who: Who;
}) {
  const readers = useMemo(() => reads?.readers ?? [], [reads]);
  const { rows, more, listID } = useShown(readers);
  // NOT SERVED HERE — a node with no usage domain — is not "nobody read it".
  if (error === "unknown_query") return null;
  return (
    <RailSection
      label="Read by agents"
      id="kpage-rail-readers"
      // THE WINDOW IS STATED: the page bar's "read by n agents today" is the
      // company's day, this list is the last `days`, and side by side with no
      // window the two numbers read as a contradiction.
      window={reads ? `last ${plural(reads.days, "day")}` : undefined}
    >
      {error ? (
        // THE FAILURE WHOLE, in the words every refused read is drawn in: a
        // bare code here could not name the grant that would admit the reader.
        <QueryState error={error} refusal={refusal} loading={false} />
      ) : !reads ? (
        <p className="kpage-rail-note">Reading…</p>
      ) : readers.length === 0 ? (
        <p className="kpage-rail-note">
          No agent has read it in the last {plural(reads.days, "day")}.
        </p>
      ) : (
        <>
          <ul className="kpage-readers" role="list" id={listID}>
            {rows.map((r) => {
              const seat = who(r.handle);
              const via = readVia(r);
              const count = readCount(r);
              return (
                <li key={`${r.handle}/${r.via}`} className="kpage-reader">
                  <SeatAvatar name={seat.name} kind={seat.kind ?? "agent"} size="sm" decorative />
                  <span className="kpage-reader-text">
                    {/* TWO LINES, as the artboard draws them: who and how
                        often, then how and when. The count sits on the name's
                        line, where there is room, and the way-read line is cut
                        with an ellipsis (whole in its title) so the age at its
                        end is never pushed onto a third line. */}
                    <span className="kpage-reader-head">
                      <a className="kpage-reader-name" href={href(["agents", "seats", r.handle])}>
                        {seat.name}
                      </a>
                      {count && <span className="kpage-reader-count">{count}</span>}
                    </span>
                    <span className="kpage-reader-via">
                      {r.last_turn_id ? (
                        <a
                          className="kpage-reader-how kpage-reader-via-link"
                          href={href(["live", "turns", r.last_turn_id])}
                          title={via}
                        >
                          {via}
                        </a>
                      ) : (
                        <span className="kpage-reader-how" title={via}>
                          {via}
                        </span>
                      )}
                      <span className="kpage-reader-when">
                        {" · "}
                        <time dateTime={r.last_at} title={fmtDateTime(r.last_at)}>
                          <ClockText read={(now) => relTime(r.last_at, now)} />
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
            <p className="kpage-rail-note">
              The {readers.length} most recent of {reads.readers_total}.
            </p>
          )}
        </>
      )}
      {reads && reads.elided > 0 && (
        // THE CAP IS STATED, never silent: a seat's day keeps a bounded number
        // of (page, way) entries, and a reader whose entry was dropped is not
        // on this list. COMPANY-WIDE, and worded so: the engine counts what
        // the cap dropped across every page, not this one's, so the note says
        // the list MAY be short rather than that this page lost a read.
        <p className="kpage-rail-note">
          Some agents&rsquo; days were too busy to record every read, so this list may be missing
          some.
        </p>
      )}
    </RailSection>
  );
}

/**
 * "Read by n agents today" in the page bar: the faces of who read it on the
 * company's day, and the ENGINE's count beside them.
 */
export function ReadToday({ count, faces, who }: { count: number; faces: string[]; who: Who }) {
  if (count === 0) return null;
  return (
    <span className="kpage-today">
      {/* THE KIT'S DEFAULT STEP, the artboard's 22px: at the extra-small one
          the overlap cuts every monogram's second letter away. */}
      <AvatarStack
        max={3}
        decorative
        members={faces.map((h) => ({ id: h, ...seatBadge(who(h).name, who(h).kind) }))}
      />
      <span>read by {plural(count, "agent")} today</span>
    </span>
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

/** One backlink: tasks first, in key order, then pages — the engine's orders. */
type LinkRow = { kind: "task"; t: TaskLink } | { kind: "page"; p: PageLink };

/**
 * Why the answering node sent no list, in a curator's words — what to do next
 * differs: wait a few minutes, or read the page on another node.
 */
const LINKS_STATUS: Record<LinkedFromStatus, string> = {
  building:
    "This node is still building its search index, so it cannot yet say what links here. Look again in a few minutes.",
  unavailable:
    "This node could not read what links here. Reload, or open the page on another node.",
};

/**
 * The tasks and pages that point here: a task by its key, a page by its title
 * — or, when the node holds an index and could not answer, why not. Never an
 * empty list for that: "no page or task links here" is a claim only a
 * finished index can make.
 */
export function LinkedFrom({
  links,
  status,
}: {
  links?: PageBacklinks;
  status?: LinkedFromStatus;
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
  const { rows: shown, more, listID } = useShown(rows);
  if (!links) {
    if (!status) return null;
    return (
      <RailSection label="Linked from" id="kpage-rail-links">
        <p className="kpage-rail-note" role="status">
          {LINKS_STATUS[status] ?? LINKS_STATUS.unavailable}
        </p>
      </RailSection>
    );
  }
  const total = links.tasks_total + links.pages_total;
  return (
    <RailSection label="Linked from" id="kpage-rail-links">
      {rows.length === 0 ? (
        <p className="kpage-rail-note">No page or task links here.</p>
      ) : (
        <>
          <ul className="kpage-links" role="list" id={listID}>
            {shown.map((row) =>
              row.kind === "task" ? (
                <li key={`t:${row.t.id}`}>
                  {/* BY ITS ADDRESS, never its key: a key another task
                      claimed first opens the claimant ([itemPath]). */}
                  <a
                    className="kpage-link"
                    href={href(itemPath(row.t))}
                    title={row.t.via.map((v) => TASK_VIA[v] ?? v).join(" and ")}
                  >
                    <StatusMark status={row.t.status} />
                    <span className="kpage-link-key">{row.t.key}</span>
                    <span className="kpage-link-title">{row.t.title}</span>
                  </a>
                </li>
              ) : (
                <li key={`p:${row.p.id}`}>
                  <a className="kpage-link" href={href(["knowledge", "pages", row.p.id])}>
                    <FileTextGlyph size="sm" />
                    <span className="kpage-link-title">{row.p.title}</span>
                  </a>
                </li>
              ),
            )}
          </ul>
          {more}
          {total > rows.length && (
            <p className="kpage-rail-note">
              {rows.length} of {total}, tasks by key and pages by title.
            </p>
          )}
        </>
      )}
    </RailSection>
  );
}

// ---------------------------------------------------------------------------
// Revisions, children, watchers
// ---------------------------------------------------------------------------

/** "2h" inside a day, "Sep 18" past one — the artboard's revision column. */
function when(at: string, now: number): string {
  return now - Date.parse(at) < 86_400_000 ? shortAge(at, now) : fmtDateCompact(at, now);
}

/** Every saved revision, newest first; a press reads that one in the article. */
export function Revisions({
  history,
  open,
  onOpen,
  who,
}: {
  history: PageRevision[];
  open: number;
  onOpen: (version: number) => void;
  who: Who;
}) {
  const { rows, more, listID } = useShown(history);
  return (
    <RailSection label="Revisions" id="kpage-rail-revisions">
      {history.length === 0 ? (
        <p className="kpage-rail-note">Only this version exists — nobody has saved over it.</p>
      ) : (
        <>
          <ul className="kpage-revisions" role="list" id={listID}>
            {rows.map((rev) => {
              const seat = rev.author ? who(rev.author) : null;
              return (
                <li key={rev.version}>
                  <button
                    type="button"
                    className="kpage-revision"
                    aria-pressed={rev.version === open}
                    title={rev.message || undefined}
                    onClick={() => onOpen(rev.version === open ? 0 : rev.version)}
                  >
                    {seat ? (
                      <SeatAvatar
                        name={seat.name}
                        kind={seat.kind ?? "agent"}
                        size="xs"
                        decorative
                      />
                    ) : (
                      <span className="kpage-revision-engine" aria-hidden="true" />
                    )}
                    <span className="kpage-revision-what">
                      rev {rev.version} · {seat ? seat.name : "the engine"}
                    </span>
                    <time
                      className="kpage-revision-when"
                      dateTime={rev.created_at}
                      title={fmtDateTime(rev.created_at)}
                    >
                      <ClockText read={(now) => when(rev.created_at, now)} />
                    </time>
                  </button>
                </li>
              );
            })}
          </ul>
          {more}
        </>
      )}
    </RailSection>
  );
}

/** The pages filed under this one: the first window by title, and the count. */
export function Children({ children, total }: { children: PageSummary[]; total: number }) {
  const { rows, more, listID } = useShown(children);
  if (children.length === 0) return null;
  return (
    <RailSection label={`Under this page · ${total}`} id="kpage-rail-children">
      <ul className="kpage-links" role="list" id={listID}>
        {rows.map((child) => (
          <li key={child.id}>
            <a className="kpage-link" href={href(["knowledge", "pages", child.id])}>
              <FileTextGlyph size="sm" />
              <span className="kpage-link-title">{child.title}</span>
            </a>
          </li>
        ))}
      </ul>
      {more}
      {total > children.length && (
        <p className="kpage-rail-note">
          {children.length} of {total} by title — the page&rsquo;s space lists them all.
        </p>
      )}
    </RailSection>
  );
}

/** Who follows the page: each is told when it changes. */
export function Watchers({ watchers, who }: { watchers: string[]; who: Who }) {
  if (watchers.length === 0) return null;
  return (
    <RailSection label={`Watching · ${watchers.length}`} id="kpage-rail-watchers">
      <ul className="kpage-watchers" role="list">
        {watchers.map((h) => {
          const seat = who(h);
          return (
            <li key={h}>
              <a className="kpage-link" href={href(["agents", "seats", h])}>
                <SeatAvatar name={seat.name} kind={seat.kind ?? "agent"} size="xs" decorative />
                <span className="kpage-link-title">{seat.name}</span>
              </a>
            </li>
          );
        })}
      </ul>
    </RailSection>
  );
}
