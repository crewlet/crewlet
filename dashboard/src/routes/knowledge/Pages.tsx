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
 * # Read-only, for the reason the tracker is
 *
 * A page is written by a seat's own tools or by an operator through MCP, both
 * attributed to somebody. A dashboard button would write as "the dashboard",
 * which is nobody and cannot be asked why.
 */

import { useMemo } from "react";
import { plainText, renderMarkdown } from "~/lib/markdown.ts";
import { collapse, diffLines, diffStat, type DiffSection } from "~/lib/diff.ts";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { CutNote, QueryState, SeatChip } from "~/components/common.tsx";
import {
  Button,
  Card,
  cx,
  EmptyState,
  FilterChip,
  Input,
  Select,
  Skeleton,
  Tag,
} from "@crewlethq/ui";
// OURS, DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel neither of these rows controls. Both
// rows here drive a `useParam` — the kind filter re-runs this screen's query
// and the diff lens gates one of its own — which is exactly the case our
// `activate="manual"` exists for. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, NumberCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, peekRow, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import {
  AccountTreeGlyph,
  CheckGlyph,
  DescriptionGlyph,
  ScheduleGlyph,
  SearchGlyph,
  TimelineGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
// THE TRACKER'S OWN SPELLING OF A CAPPED COUNT — `50+` rather than `50` —
// because a header count is the same number in a different slot wherever it
// is drawn, and this screen bounds three reads. See [pageCount].
//
// THE SENTENCE IS NOT TAKEN THE SAME WAY. [CutNote] renders `pageNote` under a
// card whose rest is somewhere else, and this screen's children card is one of
// those; the browse's own pager is NOT, and [PageBar] says why it writes its
// own sentence rather than composing that one.
import { pageCount } from "~/lib/work.ts";
import { useNow } from "~/lib/clock.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import type { Page, PageRevision, PageSummary } from "~/protocol/index.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { ToolCallBlock } from "~/components/ToolCall.tsx";

const STATUS_TONE: Record<string, "success" | "warning" | "danger" | "info" | "neutral"> = {
  published: "success",
  draft: "warning",
  trashed: "neutral",
};

/**
 * How many pages one window of the browse asks for.
 *
 * FIFTY, which is `internal/pages.DefaultLimit` — the size this read has always
 * answered at, now stated at the call site rather than left to the engine's
 * default. It is stated because the WINDOW is this screen's arithmetic now: the
 * pager below the grid steps by exactly this many, so a page size the engine
 * chose and the screen guessed would step over rows or repeat them the first
 * time either number moved.
 *
 * NOT `internal/pages.MaxLimit` (500), although the read would serve it. A
 * browse is read a screenful at a time, and five hundred rows is a scroll
 * nobody finishes — the ceiling is how a report is drawn, not a list. What the
 * ceiling bought before this screen could page was reach, and the pager buys
 * that outright: every row in a container is now behind a next click rather
 * than behind a bigger single answer.
 */
const PAGE = 50;

/**
 * A page's address, which is what the frame carries and what the engine reads.
 *
 * `CONTAINER/Title`, in one place: the `page` query takes exactly this string,
 * `KINDS.page` splits it back into a route on the first slash, and the title
 * is matched the way the fleet CLAIMED it — case-insensitively, whitespace
 * collapsed. Written at each call site it would be spelled with a uuid
 * somewhere, which resolves for the query and gives the rail a `peek=` token
 * no reader can recognise and no grid row can be found by.
 *
 * EXPORTED for the Knowledge screen, whose ranked hits address the same pages
 * and had the interpolation written out three times in one component — which
 * is how the browse and the search come to open two different rails for one
 * page the day either of them learns about a uuid.
 */
export function pageAddress(page: { container: string; title: string }): string {
  return `${page.container}/${page.title}`;
}

/**
 * The five facts a page is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today — the same
 * argument `nodeFacts` makes on the fleet screen. A reader scans a page's
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
  now,
  seatName,
}: {
  page: Page;
  /** Newest first, as the `page` answer orders it. */
  history: PageRevision[];
  now: number;
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
            at: last.created_at,
            ago: relTime(last.created_at, now),
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
      value: <DateCell at={page.updated_at} now={now} />,
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
function pageFlags(page: Page): React.ReactNode {
  return (
    <span className="row gap-1">
      <Tag variant={STATUS_TONE[page.status] ?? "neutral"} dot>
        {page.status}
      </Tag>
      {page.skill && (
        <Tag variant="info" title="Injected into a phase by the tool-skill registry">
          tool skill
        </Tag>
      )}
      {page.onboarding && (
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
  const ref = { kind: "page" as const, id: pageAddress(page) };
  return (
    <a className="t-link truncate" href={peekHref(ref)} onClick={rowPeekHandler(() => open(ref))}>
      {page.title}
    </a>
  );
}

export function Pages({ container: fromPath }: { container?: string }) {
  const org = useOrg();
  const nav = useNavigator();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();

  // THE CONTAINER IS THE PATH (`#/knowledge/ENG`), because a container is an
  // object — it has an owning unit, a page tree and a purpose — and a filter
  // key made it unlinkable. Choosing one NAVIGATES rather than filtering.
  const container = fromPath ?? "";
  const setContainer = (key: string) => nav.to(key ? ["knowledge", key] : ["knowledge"]);

  // WHERE THIS WINDOW STARTS, and the reason the browse has one at all: the
  // `pages` read takes `limit` AND `offset` (`internal/pages.Filter`, clamped
  // in `internal/api/queries.pageList`), and this screen sent neither — so a
  // container holding four hundred pages answered its alphabetically first
  // fifty for ever, and the note under the grid could only point at filters
  // that narrow rather than at the rest of the set. Every row is now behind a
  // Next click.
  //
  // A URL PARAMETER, like every other piece of this screen's state: a window
  // is a place, so it is linkable and survives a reload.
  //
  // PARSED DEFENSIVELY because a hash is hand-editable: a non-number, a
  // negative or a fraction reads as the first page rather than as an offset
  // the engine would refuse.
  const [offsetRaw, setOffsetParam] = useParam("offset", "");
  const offset = Math.max(0, Math.trunc(Number(offsetRaw)) || 0);
  const setOffset = (next: number) => setOffsetParam(next > 0 ? String(next) : "");
  // A NEW FILTER IS A NEW LIST, so it starts at that list's first page. Written
  // here rather than taken from `useParam`'s own setter because the reset and
  // the filter are ONE move: two `nav.filter` calls in a row would leave the
  // old offset on the new filter for one render, and that render is a read —
  // narrowing from page nine of every container to one container answered
  // nothing at all, which reads exactly like a container with no pages.
  const narrow = (patch: Record<string, string | null>) => nav.filter({ ...patch, offset: null });

  const [title] = useParam("title", "");
  const setTitle = (next: string) => narrow({ title: next || null });
  // THREE STATES on the wire and three here: only the tool-skill pages
  // (auditing the catalogue), everything but them (an ordinary browse), and
  // everything. A checkbox would make one of the three unreachable.
  //
  // AND ALL THREE ARE SPELLED, `all` included, because this state has to be
  // LINKABLE: the detail's "every child" link carries it — a page's children
  // are not filtered by kind at all, so a link that left this on `prose` landed
  // on a list holding fewer rows than the card it was under — and `href` drops
  // an empty value from a query record (see `buildHash`), so `kind=""` is a
  // state a click can reach and an anchor cannot. `kind=all` also says what it
  // is in an address bar, where `kind=` reads as a key somebody half-typed.
  const [kind] = useParam("kind", "prose");
  const setKind = (next: string) => narrow({ kind: next === "prose" ? null : next });
  // ONE PAGE'S CHILDREN, AND THIS IS WHERE THEY ARE REACHABLE. A page detail
  // read takes no paging parameter — `internal/pages` says so at
  // `Detail.ChildrenTruncated` — so it answers the first window of a page's
  // children and a flag, and names `list_pages` with `parent` as where the
  // rest is. That question is this screen's, so the filter is this screen's
  // too: without it the fifty-first child of a container index page was
  // reachable by an agent's tool and by nobody reading the dashboard.
  //
  // A URL PARAMETER rather than component state, like every other filter here:
  // the children of a page is a place, so it is linkable, bookmarkable and
  // what the detail's own "more children" link points at.
  const [parent] = useParam("parent", "");
  const setParent = (next: string) => narrow({ parent: next || null });

  const containers = useQuery("containers", undefined, { pollMs: 60_000 });

  const params: Record<string, unknown> = { limit: PAGE };
  if (offset > 0) params.offset = offset;
  if (title) params.title = title;
  if (kind === "skills") params.skills = true;
  if (kind === "prose") params.skills = false;
  // A PAGE'S CHILDREN ARE NOT A CONTAINER'S, so a `parent` REPLACES the
  // container term rather than joining it. `internal/pages.Reader.Get`
  // collects children on `p.parent_id` alone, and nothing constrains a child
  // to its parent's container: `pages.Store.Create` stores the parent id
  // without checking it against the container, and the `write_page` tool lets
  // a model name `container` and `parent` independently. A container term here
  // would therefore drop exactly the children a reader has no way to know are
  // missing — under a link whose words are "Every child of this page".
  //
  // THE PATH SEGMENT STAYS WHAT IT IS. `#/knowledge` with no container is the
  // SEARCH screen (`app/App.tsx`), so a browse can only be addressed inside
  // one; while a parent is set that segment is where the reader came from
  // rather than a filter, which is why the container controls are not drawn
  // beside the chip that names the scope.
  if (parent) params.parent = parent;
  else if (container) params.container = container;

  const { data, loading, error } = useQuery("pages", params, { pollMs: 20_000 });
  // THE FILTER SAYS WHOSE CHILDREN, which is a title and not the uuid in the
  // address bar. Read only when one is set — a second question on every browse
  // to label a chip that is not drawn would be a read nobody asked for — and a
  // read that has not answered falls back to naming no page rather than
  // claiming one: the chip's job is to say the list is narrowed, which is true
  // before the title lands.
  const parentPage = useQuery("page", { id: parent }, { enabled: parent !== "" });

  // THE ENGINE'S OWN ORDER, UNTOUCHED. `internal/pages` lists `ORDER BY
  // p.container, p.title` and this screen is a NUMBERED WINDOW over that
  // ordering — the bar under the grid reads "Pages 201–250" — so re-sorting
  // the rows before they are drawn numbers positions in an order the reader
  // cannot see: a range in the alphabet printed under rows arranged by time.
  //
  // A COLUMN HEAD STILL SORTS, and that is a different question: it reorders
  // the rows of THIS window, which is a claim about what is on screen rather
  // than about where the window was cut from.
  const rows = useMemo(() => data?.pages ?? [], [data]);
  const containerKeys = useMemo(
    () => (containers.data?.containers ?? []).map((c) => c.key).sort(),
    [containers.data],
  );
  const { open: openPeek } = usePeekControls();
  // WHAT `[` AND `]` WALK: the pages this screen actually loaded, filtered by
  // whatever the toolbar above is set to. Published rather than handed to the
  // rail, because only the list knows that order; see `PeekHost`.
  //
  // THIS SCREEN'S OWN ORDER — the engine's, which is what the grid draws and
  // what the window under it is numbered in. A column sort lives inside the
  // grid and is not something this screen can read back, so a reader who
  // re-sorts steps in the engine's order instead: one order both halves agree
  // on beats a stepper that claims to follow a sequence it cannot see.
  usePeekNeighbours(
    useMemo(() => rows.map((r) => ({ kind: "page" as const, id: pageAddress(r) })), [rows]),
  );

  return (
    <>
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
            leading={<SearchGlyph size="sm" />}
            placeholder="Words from the title"
          />
        </div>
        {/* THE "ANY" ROW IS AN OPTION RATHER THAN A PLACEHOLDER: their
            `placeholder` only labels the empty trigger, and a reader who has
            chosen a container needs a row to choose their way back out.

            AND NOT WHILE A PARENT IS SET, because the read drops its container
            term then (see `params` above): a picker showing `LEAD` selected
            would name a filter the list is not applying. The chip below is the
            scope while one is set, and clearing it brings this back. */}
        {!parent && (
          <Select
            // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
            // `width: 100%` unless told otherwise, and its own doc says why
            // that is wrong here: "a filter row of full-width selects is one
            // question per line, which is not what a filter bar is".
            width="auto"
            value={container}
            onChange={(value) => setContainer(String(value))}
            ariaLabel="Container"
            active={container !== ""}
            options={[
              { value: "", label: "Every container" },
              ...containerKeys.map((key) => ({ value: key, label: key })),
            ]}
          />
        )}
        <Segmented
          value={kind}
          onChange={setKind}
          ariaLabel="Pages or tool skills"
          options={[
            { value: "prose", label: "Pages", title: "Everything but the tool-skill pages" },
            { value: "skills", label: "Tool skills", title: "The machinery a phase is offered" },
            { value: "all", label: "All", title: "Prose and tool skills together" },
          ]}
        />
      </div>

      {parent && (
        <div className="row wrap" style={{ gap: "var(--space-2)", marginBottom: "var(--space-3)" }}>
          {/* PRESSED AND CLEARABLE, which is what a `FilterChip` is: this is
              the one filter on the screen with no control of its own, because
              it is arrived at from a page rather than chosen from a list. A
              banner would say the same thing in a shape a reader cannot switch
              off. */}
          <FilterChip pressed onClick={() => setParent("")}>
            {parentPage.data?.page.title
              ? `Children of ${parentPage.data.page.title}`
              : "Children of one page"}
          </FilterChip>
          {/* WHICH CONTAINERS THIS IS OVER, said rather than left to the
              missing picker. A child is filed wherever its author put it and
              need not share its parent's container, so this list is every
              container — and a reader who has just watched the container
              controls disappear is owed the reason. */}
          <span className="t-caption">In every container.</span>
        </div>
      )}

      {!parent && containers.data?.containers?.length ? (
        <div className="row wrap" style={{ gap: "var(--space-2)", marginBottom: "var(--space-3)" }}>
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
            : offset > 0
              ? // PAST THE END IS NOT AN EMPTY CONTAINER, and it is a state the
                // pager can be linked into: a bookmark taken on page nine is
                // still a window after the pages behind it are trashed. Said
                // plainly, with the way back, rather than as "No pages here" —
                // which is the one wording that makes a reader believe a
                // container they are looking at is empty.
                {
                  title: "Nothing on this page of the list",
                  hint: `This window starts at page ${offset + 1} and the list no longer reaches that far. Go back to the first page below.`,
                }
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
        <Card padding="none">
          {/* WHAT THIS GRID IS A GRID OF, and whether it is all of it.
              `PagesAnswer.truncated` has been declared with its rule written
              out — ASKED, NOT INFERRED — and read by the sibling container peek
              alone, while THIS screen, the one the product calls "every page,
              browsed", drew the engine's page size as the company's knowledge
              base and had no paging of its own to get past it.

              THE SAME TREATMENT AS THAT PEEK: the flag is read, never
              `rows.length >= limit`, because a container holding exactly the
              limit holds every row it has and the inference puts a caution
              about missing data on a complete answer.

              `padding="none"` because a card built from header and footer slots
              draws its own body inset, and the grid sits flush inside it — the
              shape every other listing card on this screen's siblings takes. */}
          <Card.Header
            icon={<DescriptionGlyph size="sm" />}
            count={data ? pageCount(rows.length, Boolean(data.truncated)) : undefined}
          >
            <Card.Title>Pages</Card.Title>
          </Card.Header>
          <DataGrid<PageSummary>
            rows={rows}
            rowKey={(r) => r.id}
            // NO DEFAULT SORT, so the grid draws the rows in the order the
            // engine picked this window in — see `rows` above. A default of
            // `-updated` rearranged the window by time under a bar numbering
            // it by title.
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
            rowHref={(r) => peekHref({ kind: "page", id: pageAddress(r) })}
            onRowActivate={peekRow<PageSummary>((r) =>
              openPeek({ kind: "page", id: pageAddress(r) }),
            )}
            columns={[
              {
                key: "title",
                header: "Title",
                sortValue: (r) => r.title,
                // NOT AN ANCHOR: the row is one now, and a title that was also
                // a link would be the one part of the row where a plain click
                // meant something different from everywhere else on it.
                cell: (r) => <TextCell icon="description">{r.title}</TextCell>,
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
                cell: (r) => <DateCell at={r.updated_at} now={now} />,
              },
            ]}
          />
        </Card>
      </QueryState>

      {/* THE WINDOW, AND THE WAY THROUGH IT.
          OUTSIDE THE `QueryState`, which is the one thing that decides where
          this lives: `empty` renders INSTEAD of its children, so a pager in the
          card would disappear on exactly the state that needs it most — a
          window past the end of a list that has since been trimmed, where
          Previous is the only way back to anything at all. */}
      <PageBar
        shown={rows.length}
        more={Boolean(data?.truncated)}
        offset={offset}
        set={setOffset}
      />
    </>
  );
}

/**
 * The browse's window: which rows these are, and the two clicks either side.
 *
 * WHY THIS SCREEN PAGES AT ALL. `internal/api/queries.pageList` clamps `limit`
 * to `pages.MaxLimit` and passes `offset` straight through, and this screen
 * sent neither — so a container holding four hundred pages answered its
 * alphabetically first fifty on every read, and the note under the grid could
 * only offer filters that NARROW. Narrowing is not reaching: a reader after the
 * fifty-first page by title had to guess a substring of a title they had never
 * seen.
 *
 * NOT [CutNote], and not [pageNote] either. `CutNote` draws only when there is
 * MORE — right for a card whose rest is in a table or behind a link, wrong for
 * a control: on the last page of nine there is nothing more and the reader
 * still has to get back. So the bar draws whenever the list is windowed in
 * EITHER direction.
 *
 * AND ITS SENTENCE IS THE RANGE'S OWN. Every sentence [pageNote] has begins
 * "The first" or "The newest", which is true of a page taken from the START of
 * a set and false of every window after it — the one thing this bar exists to
 * move. Composed here it printed "The first 50 pages in title order" beside
 * "Pages 201–250": a marker contradicting the range next to it, which is
 * strictly worse than no marker. The order was right; the shape of the claim
 * was not.
 *
 * THE RANGE IS COUNTED FROM THE OFFSET rather than from the rows, because those
 * are two different facts: the rows say how many came back and the offset says
 * where they start, and a page of forty at offset 200 is rows 201–240.
 *
 * THE ORDER IS STATED, NEVER "the newest". `internal/pages` lists `ORDER BY
 * p.container, p.title`, so the window is a range in THAT ordering — which is
 * also what makes an offset meaningful at all, a stable order being the whole
 * of what a numbered window rests on. The grid above draws the rows in it
 * unsorted, so the sentence names an order the reader can see.
 */
function PageBar({
  shown,
  more,
  offset,
  set,
}: {
  shown: number;
  more: boolean;
  offset: number;
  set: (next: number) => void;
}) {
  if (offset === 0 && !more) return null;
  const range =
    shown > 0
      ? `Pages ${(offset + 1).toLocaleString()}–${(offset + shown).toLocaleString()} · ordered by container, then title`
      : "";
  // ONLY WHEN THERE IS MORE, on [pageNote]'s own reasoning: "and that is all of
  // them" under every last page is how the one case that matters arrives as a
  // changed word nobody reads. The disabled Next button says the same thing in
  // the shape a control has.
  const rest = more ? "there are more" : "";
  return (
    <div className="row wrap" style={{ gap: "var(--space-2)", marginTop: "var(--space-3)" }}>
      <span className="t-caption">
        {range}
        {range && rest ? " · " : ""}
        {rest}
      </span>
      <span className="spacer" />
      <Button
        size="small"
        variant="secondary"
        disabled={offset === 0}
        onClick={() => set(Math.max(0, offset - PAGE))}
      >
        Previous
      </Button>
      <Button size="small" variant="secondary" disabled={!more} onClick={() => set(offset + PAGE)}>
        Next
      </Button>
    </div>
  );
}

/** One page: its body, where it sits, and everything that changed it. */
/**
 * One page.
 *
 * ADDRESSED BY CONTAINER AND TITLE, which is what a person was given: the
 * engine's own `Get` takes `CONTAINER/Title` and matches the title the way the
 * fleet CLAIMED it — case-insensitively, with runs of whitespace collapsed —
 * so `ENG/deploy runbook` reaches a page called "Deploy  Runbook". A uuid
 * still resolves, because every internal link carries one.
 */
export function PageView({ container, title }: { container: string; title: string }) {
  const org = useOrg();
  const viewer = useViewer();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const id = `${container}/${title}`;
  const { data, loading, error } = useQuery(
    "page",
    { id },
    { enabled: id !== "/", pollMs: 20_000 },
  );
  usePageLabels(data?.page ? { [container]: container, [title]: data.page.title } : {});

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const page = data?.page;
  // NEWEST FIRST — `internal/pages` reads the revisions `ORDER BY version
  // DESC`, and the header's "set by" line is the head of this list. Read once
  // here so the panel below and the header cannot disagree about which save
  // was the last one.
  const history = data?.history ?? [];

  return (
    <>
      <PageActions>
        {page ? (
          <>
            <Tag variant={STATUS_TONE[page.status] ?? "neutral"} dot>
              {page.status}
            </Tag>
            <Tag appearance="outline" monospace>
              v{page.version}
            </Tag>
            {page.skill && <Tag variant="info">tool skill</Tag>}
          </>
        ) : undefined}
      </PageActions>
      {/* THE ANCESTOR CHAIN, outermost first — a page's place is what makes
            it findable, and a title alone says nothing about which team's tree
            it is in.

            A CHAIN OF ONE IS NOT A CHAIN. On a page filed directly in its
            container — which is most of them — this rendered the container and
            nothing else: a lone accent word in an otherwise empty band above
            the header, reading as a stray button, saying exactly what the
            `Container` fact three lines below it already says as a link to the
            same place. The trail earns its line when it has something the fact
            cannot carry, which is the path THROUGH the tree; until then the
            fact is the whole answer.

            THE GUARD WRAPS THE NOTE, NOT ITS CONTENTS. `PageNote` renders its
            `<p class="page-note">` whatever it is handed, and that paragraph
            carries a `margin-bottom` of its own — so guarding only the
            breadcrumb inside it swapped a stray link for an empty band, which
            is the same gap with nothing in it. */}
      {page && (data.ancestors ?? []).length > 0 ? (
        <PageNote>
          <span className="row wrap" style={{ gap: "var(--space-1)" }}>
            <a href={href(["knowledge", page.container])}>{page.container}</a>
            {(data.ancestors ?? []).map((a) => (
              <span key={a.id}>
                {" / "}
                <a href={href(["knowledge", page.container, a.title])}>{a.title}</a>
              </span>
            ))}
          </span>
        </PageNote>
      ) : undefined}

      {loading && <Skeleton variant="text" rows={8} label="Loading the page" />}

      <QueryState error={error} loading={loading}>
        {page && (
          <>
            {/* THE OBJECT'S OWN HEADER. The title used to be the page bar's
                crumb and nothing else, so the screen opened straight into a
                body with no statement of what it was — and of the five facts a
                page is read by, two were a badge row, one was the breadcrumb,
                one was a panel of chips, and WHO WROTE IT AND WHEN appeared
                nowhere at all: the grid a reader arrived from showed both, and
                the page they clicked into showed neither. They come out of the
                same builder the rail uses, so a reader scans them in one order
                wherever a page appears.

                THE PAGE BAR KEEPS ITS OWN BADGES, which is not a duplicate
                for the sake of one: the bar sits outside the scrolling region
                and the header scrolls away with the body, so on a long page
                the status is the one fact that must survive the scroll. */}
            <ObjectHeader
              kind="Page"
              icon="description"
              // NO IDENTIFIER BESIDE THE TITLE. A page is addressed by its
              // container and its title — both are already here, one as the
              // first fact and one as the title itself — and the only other
              // id it has is the uuid nobody types.
              title={page.title}
              status={pageFlags(page)}
              facts={pageFacts({ page, history, now, seatName })}
            />

            <Card>
              {page.body ? (
                <div className="prose md">{renderMarkdown(page.body)}</div>
              ) : (
                <span className="muted">This page has no body.</span>
              )}
            </Card>

            {data.children?.length ? (
              <Card>
                {/* THE COUNT IS THE CARD'S, not the title's, which is where
                    every other count in this product is drawn — and it has to
                    be, because this one can be a floor. A detail read takes no
                    paging parameter, so the engine answers the first window of
                    a page's children by title and says `children_truncated`;
                    the flag was declared with that rule and read nowhere, so an
                    index page with hundreds of children drew "Children (50)"
                    and a reader had no way to know the other four hundred
                    existed, let alone reach them. */}
                <Card.Header
                  icon={<AccountTreeGlyph size="sm" />}
                  count={pageCount(data.children.length, Boolean(data.children_truncated))}
                >
                  <Card.Title>Children</Card.Title>
                </Card.Header>
                {/* THE CHILD'S OWN CONTAINER, which is on the row. Addressed
                    under the PARENT's, a child filed elsewhere resolves to a
                    different page of that title or to none: `internal/pages`
                    locates `CONTAINER/Title` by matching both. Nothing keeps
                    the two the same — a create stores the parent id without
                    checking it against the container, and `write_page` lets a
                    model name `container` and `parent` independently. */}
                <ul className="list">
                  {data.children.map((child) => (
                    <li key={child.id}>
                      <a href={href(["knowledge", child.container, child.title])}>{child.title}</a>
                    </li>
                  ))}
                </ul>
                {/* AND THE REST IS ONE CLICK, which is the whole reason the
                    browse learned a `parent` filter: the engine's own note on
                    this flag names `list_pages` with `parent` as where the rest
                    is, and that is a tool an agent can call and a person
                    cannot. The link asks the same question from the screen that
                    PAGES it — which is what makes "every" the true word. The
                    detail read takes no `offset` and answers one window of
                    fifty; the browse takes both `limit` and `offset`, so a page
                    with four hundred children is walked from there.

                    `kind=all` BECAUSE A CHILD IS NOT FILTERED BY KIND. This
                    card lists what `internal/pages.Reader.Get` returned, and
                    that read applies no skills predicate at all — the browse
                    defaults to prose, so without this the "every child" link
                    landed on a list holding FEWER rows than the card it was
                    under, and a page whose children are tool skills reached an
                    empty one.

                    AND NOR IS IT FILTERED BY CONTAINER, which is the same
                    defect on the other axis: that read applies no container
                    predicate either, and a child need not share its parent's
                    container. The destination drops its own container term
                    whenever `parent` is set — see `params` in [Pages] — so the
                    path segment below only decides which address the browse is
                    reached at, never which children it shows. */}
                <CutNote
                  shown={data.children.length}
                  more={Boolean(data.children_truncated)}
                  one="child"
                  many="children"
                  slice="alphabetical"
                  whole={
                    <a
                      className="t-link"
                      href={href(["knowledge", page.container], {
                        parent: page.id,
                        kind: "all",
                      })}
                    >
                      Every child of this page →
                    </a>
                  }
                />
              </Card>
            ) : null}

            {page.watchers?.length ? (
              <Card>
                <Card.Header>
                  <Card.Title>Watching</Card.Title>
                </Card.Header>
                <div className="row wrap" style={{ gap: "var(--space-2)" }}>
                  {page.watchers.map((w) => (
                    <SeatChip key={w} name={seatName(w)} handle={w} />
                  ))}
                </div>
              </Card>
            ) : null}

            <Card>
              <Card.Header>
                <Card.Title>{`Comments (${data.comments?.length ?? 0})`}</Card.Title>
              </Card.Header>
              {data.comments?.length ? (
                <div className="col gap-3">
                  {data.comments.map((c) => (
                    <div key={c.id} className="comment">
                      <div className="row" style={{ gap: "var(--space-2)" }}>
                        <SeatChip name={seatName(c.author)} handle={c.author} />
                        <span className="muted" title={fmtDateTime(c.created_at)}>
                          {relTime(c.created_at, now)}
                        </span>
                        {c.edited_at && <span className="muted">(edited)</span>}
                      </div>
                      <div className="prose md">{renderMarkdown(c.body)}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <span className="muted">Nobody has commented.</span>
              )}
            </Card>

            <PageHistory pageID={page.id} history={history} seatName={seatName} now={now} />

            <PageChanges pageID={page.id} seatName={seatName} now={now} />
            {/* WHAT IT WOULD TAKE TO EDIT THIS, since the dashboard does not.
                The save states the version it edited, because a page has no
                per-field merge that makes overwriting prose safe. */}
            <ToolCallBlock
              subject={{ kind: "page", id: page.id, version: page.version }}
              viewer={viewer.handle}
            />
          </>
        )}
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
  const now = useNow();
  const { data, loading, error } = useQuery("page", { id }, { enabled: id !== "", pollMs: 20_000 });
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
        icon={<DescriptionGlyph size="xl" />}
        title={`No page at “${id}”`}
        description="A page is addressed by its container and its title. It may have been renamed, moved to another container, or trashed — or this node's copy of the knowledge base has not caught up with it yet."
      />
    );
  }

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the page" />}
      <QueryState error={error} loading={loading}>
        {page && (
          <>
            <ObjectHeader
              size="peek"
              kind="Page"
              icon="description"
              title={page.title}
              status={pageFlags(page)}
              facts={pageFacts({ page, history, now, seatName })}
            />
            <div className="col gap-3">
              <Card>
                <Card.Header icon={<DescriptionGlyph size="sm" />}>
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
                  icon={<AccountTreeGlyph size="sm" />}
                  // A FLOOR WHEN THE ENGINE CAPPED IT, exactly as the page
                  // itself draws the same read. A `page` answer takes no
                  // paging parameter, so `children` is the first window of
                  // them and `children_truncated` is the whole of what says
                  // so; drawn as a plain `50` it is a number a reader takes
                  // for a total.
                  count={pageCount(children.length, Boolean(data?.children_truncated))}
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
                {/* THE REST, FROM THE RAIL TOO. This is the one panel here
                    whose read the engine bounds, and a rail that said "there
                    are more" without a way to them would be a dead end inside
                    a dead end. The destination is the browse the full page
                    links at, on the same terms — see that link for why it
                    carries `kind=all` and why the container in the path does
                    not narrow it. */}
                <CutNote
                  shown={children.length}
                  more={Boolean(data?.children_truncated)}
                  one="child"
                  many="children"
                  slice="alphabetical"
                  whole={
                    <a
                      className="t-link"
                      href={href(["knowledge", page.container], {
                        parent: page.id,
                        kind: "all",
                      })}
                    >
                      Every child of this page →
                    </a>
                  }
                />
              </Card>

              <Card>
                <Card.Header icon={<ScheduleGlyph size="sm" />} count={history.length}>
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
                        <DateCell at={rev.created_at} now={now} />
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

/**
 * A page's saved versions, and the body of whichever one is open.
 *
 * # A list of version numbers is not a history
 *
 * The detail answer carries revision SUMMARIES — a version, an author, a
 * message, an instant — which says a page was edited eleven times and not what
 * any of those edits did. This panel used to render exactly that and tell the
 * reader why they could not click one: "past versions are kept as metadata
 * here; reading one back is a coordination read the engine does on demand."
 * There was no such read. The bodies sat in `pages_revisions` reachable only
 * by reading the page at its head.
 *
 * # An old version is an ordinary absence
 *
 * A page keeps a bounded number of revisions, so asking for one the node no
 * longer holds is not a failure — and the panel says which of the two happened
 * rather than rendering a blank.
 */
/**
 * A line diff, rendered as the document it is.
 *
 * MONOSPACE AND LINE-NUMBERED on both sides, because the two numbers are what
 * a reader uses to find the paragraph in the version beside it. A skipped run
 * is a row of its own saying how many lines it stands for: a gap silently
 * closed makes a document edited at both ends look like one rewritten in the
 * middle.
 */
function DiffPane({ sections }: { sections: DiffSection[] }) {
  return (
    <div className="diff">
      {sections.map((section, s) => (
        <div key={s} className="diff-section">
          {section.skipped > 0 && (
            <div className="diff-skip">{plural(section.skipped, "unchanged line")}</div>
          )}
          {section.lines.map((line, i) => (
            // THE CLASS NAMES ARE LITERALS, never assembled from the value:
            // a stylesheet gate that cannot see a class cannot tell a rule
            // this file relies on from one nothing uses.
            <div
              key={i}
              className={cx(
                "diff-line",
                line.kind === "add" && "is-add",
                line.kind === "remove" && "is-remove",
              )}
            >
              <span className="diff-no">{line.before ?? ""}</span>
              <span className="diff-no">{line.after ?? ""}</span>
              <span className="diff-mark">
                {line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " "}
              </span>
              <span className="diff-text">{line.text || " "}</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function PageHistory({
  pageID,
  history,
  seatName,
  now,
}: {
  pageID: string;
  history: PageRevision[];
  seatName: (handle: string) => string;
  now: number;
}) {
  // WHICH VERSION IS OPEN, as a FILTER: stepping through a page's versions
  // must not fill the back stack with every one the reader glanced at.
  const [open, setOpen] = useParam("version", "", "filter");
  const version = Number(open) || 0;
  const body = useQuery("page_revision", { page: pageID, version }, { enabled: version > 0 });
  // WHAT THIS SAVE CHANGED, which is the question somebody opens a history
  // for and which the panel could not answer: it showed any ONE version, so
  // the answer was to open two and read both.
  //
  // The PREVIOUS version by position in the list, not `version - 1`: a page
  // keeps a bounded number of revisions, so the one below this in the history
  // is the one that was actually saved before it — and off a trimmed page
  // `version - 1` is a read that comes back not found.
  const previous = useMemo(() => {
    const i = history.findIndex((rev) => rev.version === version);
    return i >= 0 ? (history[i + 1]?.version ?? 0) : 0;
  }, [history, version]);
  const [lens, setLens] = useParam("lens", "diff", "filter");
  const prior = useQuery(
    "page_revision",
    { page: pageID, version: previous },
    { enabled: version > 0 && previous > 0 && lens === "diff" },
  );
  const diff = useMemo(
    () =>
      body.data && prior.data
        ? collapse(diffLines(prior.data.body ?? "", body.data.body ?? ""))
        : [],
    [body.data, prior.data],
  );
  const stat = useMemo(() => diffStat(diff.flatMap((section) => section.lines)), [diff]);

  return (
    <Card>
      <Card.Header icon={<ScheduleGlyph size="sm" />}>
        <Card.Title>{`History (${history.length})`}</Card.Title>
      </Card.Header>
      {history.length ? (
        <div className="list">
          {history.map((rev) => (
            <button
              key={rev.version}
              type="button"
              className={`thread-entry as-row${rev.version === version ? " selected" : ""}`}
              onClick={() => setOpen(rev.version === version ? "" : String(rev.version))}
            >
              <span className="row gap-2">
                <DescriptionGlyph size="sm" />
                <span className="mono">v{rev.version}</span>
                <span>{rev.author ? seatName(rev.author) : "the engine"}</span>
                {rev.message && <span className="muted truncate">{rev.message}</span>}
                <span className="spacer" />
                <span className="muted" title={fmtDateTime(rev.created_at)}>
                  {relTime(rev.created_at, now)}
                </span>
              </span>
            </button>
          ))}
        </div>
      ) : (
        <span className="muted">Only this version exists — nobody has saved over it.</span>
      )}

      {version > 0 && (
        <div style={{ marginTop: "var(--space-3)" }}>
          <QueryState error={body.error} loading={body.loading}>
            {body.data ? (
              <>
                <div className="row wrap gap-2">
                  <span className="t-caption">
                    Version {body.data.version}
                    {body.data.title ? ` — “${body.data.title}”` : ""}, as it was saved.
                  </span>
                  <span className="spacer" />
                  {/* THE FIRST VERSION HAS NOTHING TO COMPARE WITH, which is
                      a fact about the page rather than a lens the reader
                      failed to pick — so the control is absent rather than
                      offering a diff that can only say "everything". */}
                  {previous > 0 && (
                    <Segmented
                      ariaLabel="What to show"
                      value={lens}
                      onChange={setLens}
                      options={[
                        { value: "diff", label: `Changes from v${previous}` },
                        { value: "full", label: "The whole version" },
                      ]}
                    />
                  )}
                </div>
                {lens === "diff" && previous > 0 ? (
                  <QueryState error={prior.error} loading={prior.loading}>
                    {prior.data &&
                      (stat.identical ? (
                        // IDENTICAL IS ITS OWN ANSWER. A save that changed
                        // only the title leaves the body untouched, and a
                        // pane of unmarked lines reads as one that failed
                        // to load.
                        <EmptyState
                          size="compact"
                          icon={<CheckGlyph size="xl" />}
                          title="This save did not change the body"
                          description="A page's title, its labels and its place in the tree are saved beside its body — this version's prose is the one before it."
                        />
                      ) : (
                        <>
                          <p className="t-caption">
                            <span className="diff-add-ink">+{stat.added}</span>{" "}
                            <span className="diff-del-ink">−{stat.removed}</span> against v
                            {previous}
                          </p>
                          <DiffPane sections={diff} />
                        </>
                      ))}
                  </QueryState>
                ) : (
                  <div className="prose md">
                    {body.data.body ? (
                      renderMarkdown(body.data.body)
                    ) : (
                      <span className="muted">This version had no body.</span>
                    )}
                  </div>
                )}
              </>
            ) : (
              // NOT FOUND IS NOT A FAILURE. A page keeps a bounded number
              // of revisions, so an older one is an ordinary absence — and
              // saying which of the two happened is the whole point.
              !body.loading && (
                <EmptyState
                  size="compact"
                  icon={<ScheduleGlyph size="xl" />}
                  title="This node no longer holds that version"
                  description="A page keeps a bounded number of revisions. The entry above is the record that it existed."
                />
              )
            )}
          </QueryState>
        </div>
      )}
    </Card>
  );
}

/**
 * Everything that happened to this page, which is not the same as its saves.
 *
 * `pages_history` has one row per change since the domain landed — ten change
 * kinds, who made it, whether it announced anything, and the TURN that made it
 * — and the schema ships an index literally named "one page's activity". Until
 * now nothing read a single row of it: a comment, a rename, a move, a label
 * edit and a status change all happened and left no trace any screen could
 * show. Only saves appeared, through the revision list.
 *
 * THE TURN IS WHAT A WIKI CANNOT HAVE. An edit made by a seat carries the turn
 * that made it, so "why did this page change" is one click rather than a
 * search of the event log.
 */
function PageChanges({
  pageID,
  seatName,
  now,
}: {
  pageID: string;
  seatName: (handle: string) => string;
  now: number;
}) {
  const feed = useQuery("page_activity", { page: pageID }, { pollMs: 60_000 });
  const changes = feed.data?.changes ?? [];
  // A NON-EMPTY CURSOR IS THE ENGINE'S OWN "there is at least one more", which
  // is the reading the tracker's own feeds take: `readActivity` asks for one
  // row past `MaxPageChanges` and mints a cursor only when that row came back,
  // so it is never `changes.length === 100` — wrong on the boundary in both
  // directions. It was declared on `PageActivityAnswer` and read nowhere, so a
  // page saved two hundred times drew "Activity (100)" as its whole history.
  const more = Boolean(feed.data?.next_cursor);
  return (
    <Card>
      <Card.Header
        icon={<TimelineGlyph size="sm" />}
        // A COUNT THAT HAS NOT ANSWERED IS NO COUNT — `Card.Header` draws every
        // value but `undefined`, 0 included, and a `0` before the read lands is
        // a quantity stated about a page nobody has read.
        count={feed.data ? pageCount(changes.length, more) : undefined}
      >
        <Card.Title>Activity</Card.Title>
      </Card.Header>
      <QueryState
        error={feed.error}
        loading={feed.loading}
        empty={
          changes.length
            ? undefined
            : {
                title: "Nothing has happened to this page",
                hint: "Every change writes an entry — a save, a comment, a rename, a move, a label. A page with none was created and left alone.",
              }
        }
      >
        <div className="list">
          {changes.map((change) => (
            <div key={change.id} className="thread-entry">
              <span className="row gap-2">
                <Tag appearance="outline">{change.kind}</Tag>
                <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                {change.quiet && (
                  <span className="t-caption" title="this change announced nothing">
                    quiet
                  </span>
                )}
                <span className="spacer" />
                {change.turn_id && (
                  <a
                    className="t-link t-caption"
                    href={href(["activity", "turns", change.turn_id])}
                  >
                    turn →
                  </a>
                )}
                <span className="muted" title={fmtDateTime(change.at)}>
                  {relTime(change.at, now)}
                </span>
              </span>
              {change.excerpt && <p className="t-caption">{plainText(change.excerpt)}</p>}
            </div>
          ))}
        </div>
      </QueryState>
      {/* NEWEST FIRST — `internal/pages` reads the feed `ORDER BY h.version
          DESC` — so the page a reader is looking at is the most recent
          changes, and the older ones are behind the cursor. Said rather than
          implied: a feed that stops at a hundred and says nothing reads as a
          page that stopped changing. */}
      <CutNote
        shown={changes.length}
        more={more}
        one="change"
        slice="newest"
        whole="Older ones are behind this page of the history; the turn links above reach the work that made them."
      />
    </Card>
  );
}
