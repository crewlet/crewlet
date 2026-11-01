/**
 * A space and a page, in the rail: what a list elsewhere opens beside itself
 * rather than navigating away (`app/frame/peeks.tsx`).
 *
 * # What a peek answers
 *
 * Recognition, place and freshness: is this the one I meant, where does it
 * sit, and is anybody still writing in it. What a peek deliberately does not
 * answer is everything else: the comments, the whole history with its diffs,
 * the controls that change it. A rail that grew those would be the page in a
 * 420 px column, and the list behind it is the point.
 *
 * # Drawn as Doclet draws them
 *
 * A space is a Space here as on its own page, a page's author is whoever the
 * record names (a seat's handle, or the login of a credential bound to no
 * seat; see `parts.tsx`), and a page's kind wears a neutral word while only
 * its state wears a tone ([PageTags]).
 */

import { useMemo } from "react";
import {
  Card,
  EmptyState,
  EmptyValue,
  Skeleton,
  TableNumberCell,
  TableDateCell,
  peekClickHandler,
} from "@crewlethq/ui";
import {
  ClockGlyph,
  FileTextGlyph,
  FolderGlyph,
  NetworkGlyph,
  TargetGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";

import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime, tsKey, fmtCount } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  LogRefusal,
  PageContainer,
  PageDetail,
  PageSummary,
  QueryRefusal,
} from "~/protocol/index.ts";
import { PageTags, usePeople, type Who } from "./parts.tsx";
import { firstLines } from "./page/body.ts";
import { PAGES_WINDOW } from "./usePagedPages.ts";
import { ownerSentence, useSpaceOwners, type SpaceOwners } from "./spaceOwners.ts";

/**
 * How many of a space's pages and writers the rail lists before it counts the
 * rest. Eight fits under the panels above it without the rail becoming a
 * scroll of its own, and the space's own page lists every one.
 */
const PEEK_ROWS = 8;

/**
 * How much of a body the rail shows: about one screen of the 420 px panel,
 * where prose wraps near sixty characters. "Is this the one I meant" is
 * answered by the first heading and the paragraph under it.
 */
const PEEK_LINES = 40;

/**
 * How many saves the rail lists. What it answers is whether the page is
 * still moving; the page's own History is where each save is read.
 */
const PEEK_SAVES = 4;

/**
 * A link to another page that opens it in the rail rather than navigating.
 *
 * Drawn only inside a peek, which is what makes the plain click right: the
 * reader is beside the list they came from. The `href` is the page's own
 * route, so a modified click, the middle button and the status bar still say
 * where it goes.
 */
function PageLink({ page }: { page: Pick<PageSummary, "id" | "title"> }) {
  const { open } = usePeekControls();
  const ref = { kind: "page" as const, id: page.id };
  return (
    <a className="t-link truncate" href={peekHref(ref)} onClick={peekClickHandler(() => open(ref))}>
      {page.title}
    </a>
  );
}

// ---------------------------------------------------------------------------
// A space
// ---------------------------------------------------------------------------

/**
 * The four facts a space is read by. Two of them the space record does not
 * carry and no single read states on its own (when it last moved, and who
 * files into it), so they are derived here once.
 */
function spaceFacts({
  space,
  newest,
  capped,
  unread,
  owners,
  now,
}: {
  space: PageContainer;
  /** The newest `updated_at` among the pages this node returned, if any. */
  newest?: string | undefined;
  /** The read came back at its own limit, so there may be newer pages behind it. */
  capped: boolean;
  /** The page list has not answered: still in flight, or it failed. */
  unread: boolean;
  owners: SpaceOwners;
  now: number;
}): Fact[] {
  const units = owners.state === "read" ? owners.units : null;
  const lead = units?.[0];
  return [
    // ZERO IS A REAL COUNT: a space whose pages were all trashed is a state an
    // operator comes here for. Trashed pages are not counted by the engine.
    { label: "Pages", value: <TableNumberCell value={space.pages} format={fmtCount} /> },
    {
      label: "Last written",
      // A LIST THAT DID NOT ANSWER IS NOT AN EMPTY SPACE: this value is derived
      // from that read alone, and "never" would be a claim nothing was read for.
      value: unread ? <EmptyValue label="Unknown" /> : <TableDateCell value={newest} now={now} />,
      // A listing is ordered by title, so a capped read's newest row is a
      // floor on the answer rather than the answer, and the note says so.
      note: capped ? "newest page" : undefined,
    },
    {
      // WHO FILES HERE comes from the config (a unit's `space:`), so it answers
      // even for a space nobody has written in yet.
      label: "Filed by",
      value: !units ? (
        <EmptyValue label={ownerSentence(owners)} />
      ) : units.length > 0 ? (
        units.map((u) => u.name).join(", ")
      ) : (
        <EmptyValue label="No team owns this space" />
      ),
      // A LINK ONLY WHERE THERE IS ONE PLACE TO GO: two units may share a space.
      // BY ITS ID, which a rename does not move: a unit's name is prose two
      // units may share, and a link by it opened whichever came first.
      path: units?.length === 1 && lead?.id ? ["company", "org", "teams", lead.id] : undefined,
    },
    { label: "Created", value: <TableDateCell value={space.created_at} now={now} /> },
  ];
}

/**
 * One space, in the rail: what it is for, what has been written in it lately,
 * and whose it is.
 *
 * Two reads, and only one may blank the panel: the space comes from
 * `containers` and its pages from `pages`, so a failed page list is drawn
 * where the rows would have been rather than as an empty space.
 */
export function ContainerPeek({ id }: { id: string }) {
  const who = usePeople();
  const now = useNow();
  const containers = useQuery("containers", undefined, { enabled: id !== "", pollMs: 60_000 });
  // THE SAME SET THE COUNT COUNTS (trashed pages excluded), in one window of
  // `PAGES_WINDOW` so what moved last and who writes here are said of the
  // whole of a space that fits in it.
  const list = useQuery(
    "pages",
    { container: id, status: "published,draft", limit: PAGES_WINDOW },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const found = containers.data?.containers.find((c) => c.key === id);
  // NEWEST FIRST: the engine orders a listing by title, which is the order a
  // browse wants and the opposite of what "written lately" asks for.
  const recent = useMemo(
    () => [...(list.data?.pages ?? [])].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [list.data],
  );
  const total = list.data ? (list.data.total ?? recent.length) : null;
  const capped = Boolean(list.data && (list.data.after || (total ?? 0) > recent.length));
  const owners = useSpaceOwners({ enabled: id !== "" })(id);

  return (
    <>
      {containers.loading && !containers.data && (
        <Skeleton variant="text" rows={6} label="Loading the space" />
      )}
      <QueryState
        error={containers.error}
        refusal={containers.refusal}
        loading={containers.loading}
      >
        {containers.data &&
          (found ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Space"
                icon="folder"
                identifier={found.key}
                title={found.name || found.key}
                facts={spaceFacts({
                  space: found,
                  newest: recent[0]?.updated_at,
                  capped,
                  unread: !list.data,
                  owners,
                  now,
                })}
              />
              <div className="col gap-3">
                {/* WHAT IT IS FOR, drawn even when nobody wrote one: a purpose
                    missing from the panel reads as one the reader failed to
                    scroll to. */}
                <Card>
                  <Card.Header icon={<TargetGlyph size="sm" />}>
                    <Card.Title>Purpose</Card.Title>
                  </Card.Header>
                  {found.purpose ? (
                    <p className="t-body measure">{found.purpose}</p>
                  ) : (
                    <span className="muted">No purpose is written for this space.</span>
                  )}
                </Card>
                <SpacePages
                  space={found}
                  recent={recent}
                  total={total}
                  capped={capped}
                  list={list}
                  now={now}
                />
                <SpaceWriters recent={recent} total={total} capped={capped} who={who} />
              </div>
            </>
          ) : (
            // AN ADDRESS THAT DID NOT RESOLVE, named, so a stale link and a
            // node that has not caught up are told apart from a typo.
            <EmptyState
              size="compact"
              icon={<FolderGlyph size="xl" />}
              title={`No space called “${id}”`}
              description="It may not exist yet, or this node is catching up."
            />
          ))}
      </QueryState>
    </>
  );
}

/** What has been written in a space lately, with its own `QueryState`. */
function SpacePages({
  space,
  recent,
  total,
  capped,
  list,
  now,
}: {
  space: PageContainer;
  recent: PageSummary[];
  total: number | null;
  capped: boolean;
  list: { error: string | null; refusal: QueryRefusal | LogRefusal | null; loading: boolean };
  now: number;
}) {
  const shown = Math.min(recent.length, PEEK_ROWS);
  // THE REST OF THE SPACE, counted off the total rather than the window.
  const rest = (total ?? recent.length) - shown;
  return (
    <Card>
      <Card.Header
        icon={<FileTextGlyph size="sm" />}
        count={total ?? undefined}
        subtitle={
          capped ? `newest of the first ${plural(recent.length, "page")} by title` : undefined
        }
      >
        <Card.Title>Recent pages</Card.Title>
      </Card.Header>
      <QueryState
        error={list.error}
        refusal={list.refusal}
        loading={list.loading}
        empty={
          recent.length
            ? undefined
            : {
                title: "Nothing is filed here",
                hint: "Its pages may have been trashed.",
              }
        }
      >
        <div className="col gap-1">
          {recent.slice(0, PEEK_ROWS).map((page) => (
            <span key={page.id} className="row gap-2">
              <PageLink page={page} />
              <span className="spacer" />
              <TableDateCell value={page.updated_at} now={now} />
            </span>
          ))}
          {rest > 0 && (
            <a className="t-link t-caption" href={href(["doclet", space.key])}>
              {plural(rest, "more page")} in this space
            </a>
          )}
        </div>
      </QueryState>
    </Card>
  );
}

/**
 * Who has written in this space: the AUTHORS, who started each page, and the
 * subtitle says so, because a page one seat started and another rewrote is
 * not the first seat's alone.
 */
function SpaceWriters({
  recent,
  total,
  capped,
  who,
}: {
  recent: PageSummary[];
  total: number | null;
  capped: boolean;
  who: Who;
}) {
  const writers = useMemo(() => {
    const seen: string[] = [];
    for (const page of recent) {
      const by = page.author ?? "";
      if (by && !seen.includes(by)) seen.push(by);
    }
    return seen;
  }, [recent]);

  return (
    <Card>
      <Card.Header
        icon={<UsersGlyph size="sm" />}
        count={writers.length}
        subtitle={
          capped && total !== null
            ? `authors of the first ${recent.length} by title`
            : "authors, not editors"
        }
      >
        <Card.Title>Who writes here</Card.Title>
      </Card.Header>
      {writers.length > 0 ? (
        <div className="row wrap gap-2">
          {writers.slice(0, PEEK_ROWS).map((handle) => (
            <SeatChip key={handle} handle={handle} {...who(handle)} />
          ))}
          {writers.length > PEEK_ROWS && (
            <span className="t-caption">+{writers.length - PEEK_ROWS} more</span>
          )}
        </div>
      ) : (
        // TWO ABSENCES, told apart: pages with no author were written by the
        // engine (the tool-skill catalogue and the syncs), which is not nobody.
        <span className="muted">
          {recent.length > 0 ? "Written by the engine." : "Nothing has been written here yet."}
        </span>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// A page
// ---------------------------------------------------------------------------

/**
 * The facts a page is read by, in the order its own page draws them: where it
 * is filed, who wrote it, who last saved what it says, and who follows it.
 */
function pageFacts(detail: PageDetail, who: Who, now: number): Fact[] {
  const page = detail.page;
  const last = detail.history?.[0];
  const author = page.author ?? "";
  const saver = last?.author ?? "";
  return [
    { label: "Space", value: page.container, path: ["doclet", page.container] },
    // AN AUTHOR OF NOBODY IS THE ENGINE (a sync, the tool-skill catalogue),
    // which is a fact rather than a missing one.
    { label: "Author", value: author ? who(author).name : "The engine" },
    {
      label: "Last saved",
      // THE NEWEST REVISION, never the page's `updated_at`: a comment or a
      // label moves that without changing what the page says.
      value: last
        ? `${relTime(last.created_at, now)} by ${saver ? who(saver).name : "the engine"}`
        : "Not saved over since it was written",
    },
    // ZERO IS THE READING: the wire drops an empty watcher list.
    {
      label: "Watchers",
      value: <TableNumberCell value={page.watchers?.length ?? 0} format={fmtCount} />,
    },
  ];
}

/**
 * One page, in the rail: the first screen of its body, where it sits, and its
 * last few saves.
 */
export function PagePeek({ id }: { id: string }) {
  const who = usePeople();
  const now = useNow();
  const { data, loading, error, refusal } = useQuery(
    "page",
    { id },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const page = data?.page;
  const excerpt = useMemo(() => firstLines(page?.body ?? "", PEEK_LINES), [page?.body]);
  const ancestors = data?.ancestors ?? [];
  const children = data?.children ?? [];
  const history = data?.history ?? [];

  // NOT FOUND IS NOT A FAILURE, and naming the address is the difference
  // between "that link is stale" and "this node has not caught up".
  if (error === "not_found") {
    return (
      <EmptyState
        size="compact"
        icon={<FileTextGlyph size="xl" />}
        title={`No page at “${id}”`}
        description="It may have been trashed."
      />
    );
  }

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the page" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {data && page && (
          <>
            <ObjectHeader
              size="peek"
              kind="Page"
              icon={data.folder ? "folder" : "file-text"}
              title={page.title}
              status={
                <span className="row gap-1">
                  <PageTags status={page.status} skill={data.skill} onboarding={data.onboarding} />
                </span>
              }
              facts={pageFacts(data, who, now)}
            />
            <div className="col gap-3">
              <Card>
                <Card.Header icon={<FileTextGlyph size="sm" />}>
                  <Card.Title>The page</Card.Title>
                </Card.Header>
                {page.body ? (
                  <>
                    <div className="prose md">{renderMarkdown(excerpt.head)}</div>
                    {excerpt.more > 0 && (
                      <p className="t-caption">{plural(excerpt.more, "more line")} on the page.</p>
                    )}
                  </>
                ) : (
                  <span className="muted">
                    {data.folder
                      ? "A folder: it groups the pages filed under it."
                      : "This page has no body."}
                  </span>
                )}
              </Card>

              <Card>
                <Card.Header
                  icon={<NetworkGlyph size="sm" />}
                  count={data.children_total ?? children.length}
                >
                  <Card.Title>Where it sits</Card.Title>
                </Card.Header>
                <div className="col gap-2">
                  {/* THE ANCESTOR CHAIN, outermost first: a title alone does
                      not say whose tree it is in. */}
                  <span className="row wrap gap-1">
                    <a className="t-link" href={href(["doclet", page.container])}>
                      {page.container}
                    </a>
                    {ancestors.map((a) => (
                      <span key={a.id} className="row gap-1">
                        <span className="muted">/</span>
                        <PageLink page={a} />
                      </span>
                    ))}
                  </span>
                  {children.length > 0 ? (
                    <div className="col gap-1">
                      {children.map((child) => (
                        <PageLink key={child.id} page={child} />
                      ))}
                    </div>
                  ) : (
                    <span className="muted">Nothing is filed under it.</span>
                  )}
                </div>
              </Card>

              <Card>
                <Card.Header icon={<ClockGlyph size="sm" />} count={history.length}>
                  <Card.Title>Saves</Card.Title>
                </Card.Header>
                {history.length > 0 ? (
                  <div className="col gap-2">
                    {history.slice(0, PEEK_SAVES).map((rev) => {
                      const by = rev.author ?? "";
                      return (
                        <span key={rev.version} className="row gap-2">
                          <span className="mono">v{rev.version}</span>
                          <span className="truncate">{by ? who(by).name : "The engine"}</span>
                          {rev.message && <span className="muted truncate">{rev.message}</span>}
                          <span className="spacer" />
                          <TableDateCell value={rev.created_at} now={now} />
                        </span>
                      );
                    })}
                    {history.length > PEEK_SAVES && (
                      <p className="t-caption">
                        {plural(history.length - PEEK_SAVES, "older save")}
                      </p>
                    )}
                  </div>
                ) : (
                  <span className="muted">Only this version exists.</span>
                )}
              </Card>
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}
