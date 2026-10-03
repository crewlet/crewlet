/**
 * The knowledge base — the company's own pages.
 *
 * # Browsing and searching are different questions
 *
 * This screen BROWSES: a container, a tree, a page and its history. The
 * Knowledge screen SEARCHES, and ranks. Folding them together would make the
 * common case — "show me what the platform team has written down" — a search
 * for a word somebody has to guess.
 *
 * # A company on Confluence has none of this
 *
 * The `pages` question is registered only where this node runs the native
 * knowledge base. On Confluence there is no local copy to browse, by design:
 * search there is live at query time and there is no index to walk.
 *
 * # Written as you
 *
 * "New page" files a page in the space being browsed through `write_page`, as
 * the principal the request resolves to (ADR-0024) — the same tool a seat
 * writes with, attributed to somebody who can be asked why. The page itself,
 * its editor and its thread are `page/`.
 */

import { useMemo, useRef, useState } from "react";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { NewPageDialog } from "./NewPage.tsx";
import { Button, Card, EmptyState, FilterChip, Input, Select, Skeleton, Tag } from "@crewlethq/ui";
// OURS, DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel neither of these rows controls. Both
// rows here drive a `useParam` — the kind filter re-runs this screen's query
// and the diff lens gates one of its own — which is exactly the case our
// `activate="manual"` exists for. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, NumberCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, peekRow, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import {
  NetworkGlyph,
  FileTextGlyph,
  ClockGlyph,
  PlusGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtExact, plural, tsKey } from "~/lib/format.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import type { Page, PageDetail, PageRevision, PageSummary } from "~/protocol/index.ts";
import { usePagedPages } from "./usePagedPages.ts";
import { PageNote } from "~/app/frame/PageNote.tsx";

const STATUS_TONE: Record<string, "success" | "warning" | "danger" | "info" | "neutral"> = {
  published: "success",
  draft: "warning",
  trashed: "neutral",
};

/**
 * The five facts a page is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today — the same
 * argument `nodeFacts` makes on Settings › Nodes. A reader scans a page's
 * place, its version, who wrote it, when, and who is watching, and a header
 * written twice is two orders as soon as somebody adds a sixth fact.
 *
 * # `author` IS THE CREATOR, AND A SAVE NEVER MOVES IT
 *
 * `internal/pages/apply_page.go` sets `head.Author` on the CREATE and on no
 * other path: a patch writes the revision with `at.record.Actor` and leaves
 * the head's author exactly where the create put it. So the page's own author
 * answers "who started this page", and the only record of who last SAVED it is
 * the newest revision — which is why the version fact carries a `set by` line
 * built from the history rather than from the page. This header said "Author"
 * beside an "Updated" instant, and a label and a time that near each other are
 * read as one sentence: on any page somebody else has since edited, that is
 * the wrong name attached to the wrong change. "Started by" is the same value
 * under the name it actually holds.
 */
function pageFacts({
  page,
  history,
  seatName,
}: {
  page: Page;
  /** Newest first, as the `page` answer orders it. */
  history: PageRevision[];
  seatName: (handle: string) => string;
}): Fact[] {
  const last = history[0];
  return [
    // THE CONTAINER IS A LINK, because it is the one fact here that is also a
    // place: a reader who does not recognise the page recognises the tree it
    // is filed in, and that tree is one click away.
    { label: "Container", value: page.container, path: ["knowledge", page.container] },
    {
      label: "Version",
      value: `v${page.version}`,
      // NEVER "set by —": a page whose revisions this node no longer holds
      // gets no line at all rather than one claiming a record the engine
      // cannot produce. The instant is the SAVE'S own, not the page's
      // `updated_at` — a comment or a label edit moves the page without
      // writing a revision, and pairing this name with that time would
      // credit the last writer with somebody else's change.
      setBy: last
        ? {
            actor: last.author ? seatName(last.author) : "the engine",
            // THE LINE READS ITS OWN CLOCK (`SetByLine`), so the header is
            // not a function of the second.
            at: last.created_at,
          }
        : undefined,
    },
    {
      label: "Started by",
      // NEVER A DASH. A page with no author was written by the ENGINE —
      // a sync, a migration, the tool-skill catalogue — which is a fact
      // rather than a missing one, and the history panel below has said so
      // in those words since before this header existed.
      value: page.author ? seatName(page.author) : "the engine",
    },
    {
      label: "Updated",
      value: <DateCell at={page.updated_at} />,
      // WHY THIS IS NOT THE SAVE ABOVE IT. Ten kinds of change stamp a page —
      // `internal/pages` writes a history entry for a comment, a rename, a
      // move and a label edit as well as a save — so these two instants
      // legitimately differ, and a reader who noticed would otherwise be left
      // deciding which of them is broken.
      note: last && last.created_at !== page.updated_at ? "any change, not only a save" : undefined,
    },
    {
      label: "Watchers",
      // ZERO IS THE HONEST READING, not an absence: the wire drops an empty
      // watcher list (`omitempty`), so "nobody is watching" and "the field
      // was not sent" are the same value and the first is what it means. A
      // dash here would claim the engine keeps a record it does not.
      value: <NumberCell value={page.watchers?.length ?? 0} />,
    },
  ];
}

/**
 * The state a page wears beside its title — its status, and what KIND of page
 * it is when it is not prose somebody wrote to be read.
 *
 * The tool-skill and onboarding marks sit here rather than in the facts
 * because they change how the body below should be read: a tool skill is
 * machinery the engine injects into a phase, and a reader who takes it for
 * guidance has misread the page rather than missed a field.
 */
function pageFlags(detail: PageDetail): React.ReactNode {
  const page = detail.page;
  return (
    <span className="row gap-1">
      <Tag variant={STATUS_TONE[page.status] ?? "neutral"} dot>
        {page.status}
      </Tag>
      {/* THE DETAIL'S FLAGS, not the page's: the page is the record's own
          document and these are what the applier derived from it, sent
          beside it — `page.skill` on a detail was never set. */}
      {detail.skill && (
        <Tag variant="info" title="Injected into a phase by the tool-skill registry">
          tool skill
        </Tag>
      )}
      {detail.onboarding && (
        <Tag variant="warning" title="Where a new seat's reading starts">
          onboarding
        </Tag>
      )}
    </span>
  );
}

/**
 * A link to another page that opens it in the RAIL rather than navigating.
 *
 * Only ever drawn inside a peek, which is what makes the plain click right:
 * the reader is already beside a list they came from, and walking a page's
 * ancestors or its children by throwing that list away is the navigation the
 * rail exists to avoid. The `href` is the page's own route, so ⌘-click, the
 * middle button and the status bar all still say where it goes.
 *
 * EXPORTED for the container rail, which lists the pages written in a
 * container and wants exactly this click: the two peeks are the only frames a
 * page link is drawn in, and a second copy of "peek rather than navigate" is
 * how one of them comes to forget the middle button.
 */
export function PageLink({ page }: { page: PageSummary }) {
  const { open } = usePeekControls();
  const ref = { kind: "page" as const, id: page.id };
  return (
    <a className="t-link truncate" href={peekHref(ref)} onClick={rowPeekHandler(() => open(ref))}>
      {page.title}
    </a>
  );
}

export function Pages({ container: fromPath }: { container?: string }) {
  // `/` FOCUSES THIS SCREEN'S SEARCH rather than opening the palette over it.
  const searchBox = useRef<HTMLInputElement>(null);
  useSearchTarget(searchBox);
  const org = useOrg();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);

  // THE CONTAINER IS THE PATH (`#/knowledge/ENG`), because a container is an
  // object — it has an owning unit, a page tree and a purpose — and a filter
  // key made it unlinkable. Choosing one NAVIGATES rather than filtering.
  const container = fromPath ?? "";
  const setContainer = (key: string) => nav.to(key ? ["knowledge", key] : ["knowledge"]);
  const [title, setTitle] = useParam("title", "");
  // THREE STATES on the wire and three here: only the tool-skill pages
  // (auditing the catalogue), everything but them (an ordinary browse), and
  // everything. A checkbox would make one of the three unreachable.
  const [kind, setKind] = useParam("kind", "prose");

  // THE COLUMNS HOLD STILL until the chart moves (`index`, which names an
  // author): every row is memoised on this list, so one built inline drew every
  // page on every twenty-second poll.
  const columns = useMemo<GridColumn<PageSummary>[]>(
    () => [
      {
        key: "title",
        header: "Title",
        sortValue: (r) => r.title,
        // NOT AN ANCHOR: the row is one now, and a title that was also
        // a link would be the one part of the row where a plain click
        // meant something different from everywhere else on it.
        cell: (r) => <TextCell icon="file-text">{r.title}</TextCell>,
      },
      {
        key: "container",
        header: "Container",
        shrink: true,
        sortValue: (r) => r.container,
        // A CHIP RATHER THAN A `KeyCell`, which is the one identifier
        // cell and would otherwise be right: the containers above this
        // grid are the filter for this very column, drawn as exactly
        // this badge, and a column that spelled them differently would
        // make the control and the thing it controls look like two
        // different vocabularies.
        cell: (r) => (
          <Tag appearance="outline" monospace>
            {r.container}
          </Tag>
        ),
      },
      {
        key: "kind",
        header: "Kind",
        shrink: true,
        sortValue: (r) => (r.skill ? "skill" : r.onboarding ? "onboarding" : "page"),
        cell: (r) =>
          r.skill ? (
            // A TOOL SKILL IS MACHINERY, marked so a reader does not
            // take it for guidance somebody wrote to be read: it is
            // documentation the engine injects into a phase.
            <Tag variant="info" title="Injected into a phase by the tool-skill registry">
              tool skill
            </Tag>
          ) : r.onboarding ? (
            <Tag variant="warning" title="Where a new seat's reading starts">
              onboarding
            </Tag>
          ) : (
            <span className="muted">page</span>
          ),
      },
      {
        key: "status",
        header: "Status",
        shrink: true,
        sortValue: (r) => r.status,
        cell: (r) => (
          <Tag variant={STATUS_TONE[r.status] ?? "neutral"} dot>
            {r.status}
          </Tag>
        ),
      },
      {
        key: "version",
        header: "Version",
        shrink: true,
        align: "right",
        sortValue: (r) => r.version,
        // A VERSION IS AN IDENTIFIER, not a quantity: v10 does not
        // mean ten of anything, so it wears the mono face a key wears
        // rather than the tabular one a `NumberCell` counts in.
        cell: (r) => <KeyCell value={`v${r.version}`} />,
      },
      {
        key: "author",
        header: "Author",
        shrink: true,
        sortValue: (r) => r.author ?? "",
        // HALF A CELL, and the other half is the reason: `SeatCell` is
        // the one rendering of a person in a grid, and a page with no
        // author was written by the ENGINE rather than by nobody. The
        // `—` this column drew said the opposite — that the author is
        // a thing nothing recorded — about every page the tool-skill
        // catalogue publishes.
        cell: (r) => {
          if (!r.author) return <span className="muted">the engine</span>;
          const who = index.byHandle.get(r.author);
          return <SeatCell handle={r.author} name={who?.name} kind={who?.kind} />;
        },
      },
      {
        key: "updated",
        header: "Updated",
        shrink: true,
        align: "right",
        sortValue: (r) => tsKey(r.updated_at),
        cell: (r) => <DateCell at={r.updated_at} />,
      },
    ],
    [index],
  );

  const containers = useQuery("containers", undefined, { pollMs: 60_000 });

  const params: Record<string, unknown> = {};
  if (container) params.container = container;
  if (title) params.title = title;
  if (kind === "skills") params.skills = true;
  if (kind === "prose") params.skills = false;

  // EVERY PAGE, NOT THE FIRST FIFTY. This read took the default window and
  // drew what came back as the container, so a space of four hundred pages
  // was fifty rows with nothing to say the rest existed. It reads windows of
  // 500 now, with the listing's own total and "Load more" from its cursor.
  const listing = usePagedPages(params, { pollMs: 20_000 });
  const { loading, error, refusal } = listing;

  const rows = useMemo(
    () => [...listing.rows].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [listing.rows],
  );
  const containerKeys = useMemo(
    () => (containers.data?.containers ?? []).map((c) => c.key).sort(),
    [containers.data],
  );
  const { open: openPeek } = usePeekControls();
  // WHAT `[` AND `]` WALK: the pages this screen actually loaded, filtered by
  // whatever the toolbar above is set to. Published rather than handed to the
  // rail, because only the list knows that order; see `PeekHost`.
  //
  // THIS SCREEN'S OWN ORDER — newest first, which is also the grid's default
  // sort. A column sort lives inside the grid and is not something this screen
  // can read back, so a reader who re-sorts steps in updated order instead:
  // one order both halves agree on beats a stepper that claims to follow a
  // sequence it cannot see.
  usePeekNeighbours(useMemo(() => rows.map((r) => ({ kind: "page" as const, id: r.id })), [rows]));
  // A NEW PAGE IS FILED IN THE SPACE BEING BROWSED — the reader chose the
  // place by where they pressed, so the button is drawn on a space's own page
  // and nowhere a space would have to be guessed.
  const canWrite = useWriteAccess("write_page");
  const [writing, setWriting] = useState(false);

  return (
    <>
      {container && (
        <PageActions>
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph size="sm" />}
            disabledReason={canWrite.can ? undefined : canWrite.reason}
            title={canWrite.can ? undefined : canWrite.reason}
            onClick={() => setWriting(true)}
          >
            New page
          </Button>
        </PageActions>
      )}
      {writing && <NewPageDialog container={container} onClose={() => setWriting(false)} />}
      <PageNote>
        The company's own knowledge base, browsed. To find pages about a subject rather than in a
        place, search from the Knowledge screen — it ranks.
      </PageNote>

      <div className="toolbar">
        <div style={{ flex: 1, maxWidth: 360 }}>
          {/* THEIR FIELD, WITH THE GLYPH IN ITS LEADING SLOT — which is what
              our own `SearchInput` was, plus the name carried as an
              `aria-label` rather than a prop of its own. */}
          <Input
            type="search"
            width="full"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            aria-label="Find a page by title"
            ref={searchBox}
            leading={<SearchGlyph size="sm" />}
            placeholder="Words from the title"
          />
        </div>
        {/* THE "ANY" ROW IS AN OPTION RATHER THAN A PLACEHOLDER: their
            `placeholder` only labels the empty trigger, and a reader who has
            chosen a container needs a row to choose their way back out. */}
        <Select
          // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
          // `width: 100%` unless told otherwise, and its own doc says why that
          // is wrong here: "a filter row of full-width selects is one question
          // per line, which is not what a filter bar is".
          width="auto"
          value={container}
          onChange={(value) => setContainer(String(value))}
          ariaLabel="Container"
          // NEVER "ACTIVE". The container is this page's own address, so the
          // picker navigates between objects rather than narrowing one — and
          // the accent on it, at rest on every container page, said a filter
          // was on that nobody had set.
          options={[
            { value: "", label: "Every container" },
            ...containerKeys.map((key) => ({ value: key, label: key })),
          ]}
        />
        <Segmented
          value={kind}
          onChange={setKind}
          ariaLabel="Pages or tool skills"
          options={[
            { value: "prose", label: "Pages", title: "Everything but the tool-skill pages" },
            { value: "skills", label: "Tool skills", title: "The machinery a phase is offered" },
            { value: "", label: "All" },
          ]}
        />
      </div>

      {containers.data?.containers?.length ? (
        <div
          className="row wrap"
          style={{ gap: "var(--spacing-2)", marginBottom: "var(--spacing-3)" }}
        >
          {containers.data.containers.map((c) => (
            // A BADGE THAT ACTS IS A `FilterChip` OVER THERE, which is the
            // whole of this change: our own `Badge` grew an `onClick` and a
            // `pressed` so a count could filter the list, and theirs is that
            // control outright — a button, its pressed state and a hit area
            // that clears the 24px floor, none of which this screen has to
            // spell any more.
            <FilterChip
              key={c.key}
              title={c.purpose || c.name || c.key}
              onClick={() => setContainer(container === c.key ? "" : c.key)}
              pressed={container === c.key}
            >
              {c.key}
            </FilterChip>
          ))}
        </div>
      ) : null}

      {loading && <Skeleton variant="text" rows={6} label="Loading the pages" />}

      <QueryState
        error={error}
        refusal={refusal}
        loading={loading}
        // ONE EMPTY STATE. There used to be two, and on a company with no
        // pages at all they rendered TOGETHER: `QueryState` fired on
        // `rows.length === 0` under one heading and a trailing `Empty` fired
        // on no containers under another, so the screen said "No pages here"
        // and "Nothing has been written down yet" one above the other, in two
        // different chromes. They are two facts, so this is one component
        // telling them apart rather than two components each telling one.
        empty={
          rows.length
            ? undefined
            : containerKeys.length === 0
              ? {
                  title: "Nothing has been written down yet",
                  hint: "A container is created the first time somebody writes into it. Give a unit a `space` and its seats will have somewhere to file what they learn.",
                }
              : {
                  title: "No pages here",
                  hint: "Nothing in this node's copy of the knowledge base matches. Seats write pages with write_page, and a page's container comes from the unit's `space` field.",
                }
        }
      >
        <Card>
          <DataGrid<PageSummary>
            rows={rows}
            rowKey={(r) => r.id}
            defaultSort="-updated"
            // THE ROW IS A REAL LINK to the page, so ⌘-click, the middle button
            // and the status bar all behave — and a plain click opens the page
            // beside the list instead, because "is this the one I meant" is
            // answered by the first paragraph and a browse is the one place
            // where losing the list to read one title costs the most.
            //
            // THE FRAME'S OWN ANSWER to where a page lives rather than a second
            // copy of the route: the rail's `Open ↗` is built from the same
            // reference, so a row and the panel it opens can never name
            // different pages.
            rowHref={(r) => peekHref({ kind: "page", id: r.id })}
            onRowActivate={peekRow<PageSummary>((r) => openPeek({ kind: "page", id: r.id }))}
            columns={columns}
          />
          {listing.more && (
            // A WINDOW LABELLED AS ONE: what is drawn of how many there are.
            // The order above is newest first AMONG THE LOADED PAGES — the
            // engine pages by title, so an unloaded window can hold a newer
            // page, and the foot says so rather than implying a whole sort.
            <Card.Footer variant="meta">
              <span className="row wrap gap-2">
                <span>
                  {fmtExact(rows.length)} of {fmtExact(listing.total ?? 0)} pages loaded — sorted
                  among these.
                </span>
                <Button
                  variant="secondary"
                  size="small"
                  onClick={listing.loadMore}
                  loading={listing.paging}
                >
                  Load more
                </Button>
                {listing.pageFailure && (
                  // THE FAILURE WHOLE: the grant a refusal named, or that the
                  // state log will not lift it — never a bare code.
                  <QueryState
                    error={listing.pageFailure.error}
                    refusal={listing.pageFailure.refusal}
                    detail={listing.pageFailure.detail ?? undefined}
                    loading={false}
                  />
                )}
              </span>
            </Card.Footer>
          )}
        </Card>
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The peeks
// ---------------------------------------------------------------------------

/**
 * How much of a body the rail shows.
 *
 * FORTY LINES, which is about one screen of the panel at its default width —
 * the rail is 420 px and prose in it wraps at roughly sixty characters. The
 * question a peek answers is "is this the one I meant", and on a page written
 * for people that is answered by its first heading and the paragraph under it.
 * More would make the rail a narrow copy of the page, which is the one thing
 * `peeks.tsx` says a peek must never become.
 */
const PEEK_LINES = 40;

/**
 * How many saves the rail lists before it stops and says how many are left.
 *
 * FOUR, because what this panel answers is "is this page still moving" — the
 * last few edits and who made them. The whole list is on the page, where it
 * is a history with diffs rather than a freshness signal.
 */
const PEEK_SAVES = 4;

/**
 * The head of a body, in LINES rather than characters.
 *
 * A markdown document cut at a character count stops mid-construct — half a
 * link, a table row with no cells, an image with no closing paren — and the
 * renderer draws the broken half as literal text, which is exactly the
 * `white-space: pre-wrap` look `lib/markdown.ts` exists to have ended. Cut on
 * a line boundary and every block it knows is either whole or absent. The one
 * exception is a fence the cut leaves open, and `parseBlocks` is explicit
 * about that case: it renders what the fence opened rather than dropping the
 * rest of the document.
 */
function firstLines(body: string, max: number): { head: string; more: number } {
  const lines = body.split("\n");
  if (lines.length <= max) return { head: body, more: 0 };
  return { head: lines.slice(0, max).join("\n"), more: lines.length - max };
}

/**
 * One page, in the rail.
 *
 * ADDRESSED BY THE STRING THE PAGE ITSELF IS ADDRESSED BY. `CONTAINER/Title`
 * is what the `page` query takes, so the rail's `peek=` id is handed straight
 * over and this component never learns how a title is matched — which is what
 * lets a pasted URL open the same page the grid row above it would have.
 *
 * WHAT IT ANSWERS: the first screen of the body, where the page sits, and
 * whether anybody has touched it lately — recognition, place, freshness. What
 * it deliberately does not answer is everything else the page has — the
 * comments, the whole history with its diffs, the tool call that would edit
 * it. A rail that grew those would be the page in a 420 px column, and the
 * list behind it is the point.
 */
export function PagePeek({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { data, loading, error, refusal } = useQuery(
    "page",
    { id },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const page = data?.page;
  const excerpt = useMemo(() => firstLines(page?.body ?? "", PEEK_LINES), [page?.body]);
  const ancestors = data?.ancestors ?? [];
  const children = data?.children ?? [];
  const history = data?.history ?? [];

  // NOT FOUND IS NOT A FAILURE, and the shared refusal cannot say which
  // address failed: it answers "there is no such record" about a page
  // addressed by a name somebody TYPED or pasted. Naming the address is the
  // whole difference between "that link is stale" and "I spelled the
  // container wrong", and the rail is the one place a reader arrives at a
  // page they never saw in a list.
  if (error === "not_found") {
    return (
      <EmptyState
        size="compact"
        icon={<FileTextGlyph size="xl" />}
        title={`No page at “${id}”`}
        description="A link to a page carries its id, which a rename does not change. It may have been trashed or purged — or this node's copy of the knowledge base has not caught up with it yet."
      />
    );
  }

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the page" />}
      <QueryState error={error} refusal={refusal} loading={loading}>
        {page && (
          <>
            <ObjectHeader
              size="peek"
              kind="Page"
              icon="file-text"
              title={page.title}
              status={pageFlags(data)}
              facts={pageFacts({ page, history, seatName })}
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
                  // DRAWN EVEN WITH NOTHING IN IT. "This page has no body" is
                  // an answer to the question the rail was opened to ask; a
                  // panel that simply vanished would read as a body the reader
                  // failed to scroll to.
                  <span className="muted">This page has no body.</span>
                )}
              </Card>

              <Card>
                <Card.Header
                  icon={<NetworkGlyph size="sm" />}
                  count={data?.children_total ?? children.length}
                >
                  <Card.Title>Where it sits</Card.Title>
                </Card.Header>
                <div className="col gap-2">
                  {/* THE ANCESTOR CHAIN, outermost first — the same breadcrumb
                      the page draws, because a title alone says nothing about
                      which team's tree it is in and that is half of "is this
                      the one I meant". */}
                  <span className="row wrap gap-1">
                    <a className="t-link" href={href(["knowledge", page.container])}>
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
                    {history.slice(0, PEEK_SAVES).map((rev) => (
                      <span key={rev.version} className="row gap-2">
                        <span className="mono">v{rev.version}</span>
                        {/* THE ENGINE, not a dash — see [pageFacts]. */}
                        <span className="truncate">
                          {rev.author ? seatName(rev.author) : "the engine"}
                        </span>
                        {rev.message && <span className="muted truncate">{rev.message}</span>}
                        <span className="spacer" />
                        <DateCell at={rev.created_at} />
                      </span>
                    ))}
                    {history.length > PEEK_SAVES && (
                      <p className="t-caption">
                        {plural(history.length - PEEK_SAVES, "older save")} on the page, with what
                        each one changed.
                      </p>
                    )}
                  </div>
                ) : (
                  <span className="muted">
                    Only this version exists — nobody has saved over it.
                  </span>
                )}
              </Card>
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}
