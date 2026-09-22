/**
 * Knowledge — what the company knows, and what each seat has learned.
 *
 * Two halves, and the split is the architecture rather than a layout choice:
 *
 *  - **The knowledge base** is searched LIVE, at query time, through the
 *    engine's own `knowledge.Searcher` seam. There is no local copy, no sync
 *    worker and no index to keep fresh — which is exactly why this screen has
 *    a search box and not a browsable tree. Search is BEST EFFORT by contract:
 *    every failure path is an empty result, so this screen has to say when it
 *    got one rather than drawing silence as "nothing found".
 *  - **What a seat learned** is per-agent and private: its diary, its
 *    episodes, the skills it drafted for itself. That lives on the seat.
 */

import { useState } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, Section, SeatChip } from "~/components/common.tsx";
import { Button, Callout, Card, EmptyState, EmptyValue, Input, Skeleton, Tag } from "@crewlethq/ui";
import {
  ArrowForwardGlyph,
  Book2Glyph,
  CloseGlyph,
  DatabaseGlyph,
  DescriptionGlyph,
  FolderGlyph,
  GroupGlyph,
  NeurologyGlyph,
  OpenInNewGlyph,
  ScheduleGlyph,
  SearchGlyph,
  TargetGlyph,
} from "@crewlethq/icons/glyphs";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { documentUnits, indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useMemo } from "react";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { DateCell, NumberCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import type { PageContainer, PageSummary } from "~/protocol/index.ts";
// THE BROWSE'S OWN SPELLING of a page's address and of a link that peeks,
// rather than a second one here: a hit, a grid row and a container's page list
// must resolve to the same `peek=` token, or the stepper walks past the page
// the reader just opened and one of the three forgets the middle button.
import { pageAddress, PageLink } from "./Pages.tsx";

/**
 * How many seats the memory grid names before it stops and points at the
 * roster.
 *
 * TWELVE, which is three full rows of the auto grid this section draws at a
 * typical width — enough that a company of a dozen seats is shown whole, and a
 * clean stopping point for one that is not. It was a bare `.slice(0, 12)` at
 * the call site under a heading reading "What each seat has learned for
 * itself": a literal with no name, no reason and NOTHING SAYING A CUT HAD
 * HAPPENED, so a company of forty seats was told that its twelve were the
 * ones with memory — over a section whose whole claim is "each".
 *
 * IT IS A PEEK, LIKE THE TWO BELOW IT, so it is bounded here rather than in
 * the read: the seats come from the org projection this screen already holds,
 * so nothing is fetched to draw them and the rest are one click away on the
 * roster. That is why this is a cut with a marker rather than a paged read.
 */
const PEEK_SEATS = 12;

export function Knowledge() {
  const org = useOrg();
  const [q, setQ] = useParam("q", "");
  const [draft, setDraft] = useState(q);
  const index = useMemo(() => indexOrg(org), [org]);
  // THE SET THE SECTION IS ABOUT, derived once: the empty state asks whether
  // there are any, the grid draws the first [PEEK_SEATS] of them and the link
  // under it counts the rest. Three copies of one filter is how a heading
  // comes to describe a different set from the cards under it.
  const agentSeats = useMemo(() => index.seats.filter((s) => s.kind === "agent"), [index]);

  // Searching is a real request against a real wiki, so it runs on submit
  // rather than on every keystroke: a per-character search would put one
  // request per letter through the company's own credentials.
  const { data, loading, error } = useQuery("knowledge", { q }, { enabled: q.trim().length > 0 });

  const { open: openPeek } = usePeekControls();
  // WHAT `[` AND `]` WALK: the hits this search returned, in the engine's own
  // ranked order — which is the order they are drawn in and the only order a
  // reader of a ranked list is looking at.
  //
  // THE NATIVE HITS ONLY, and that leaves no gap in the middle: there is
  // exactly ONE knowledge backend per company, so a result set is either all
  // native (every hit a page this engine holds) or all vendor (every hit a URL
  // in somebody else's wiki, which has no page here to peek at). On a vendor
  // backend this publishes nothing and the rail gets no stepper, which is the
  // honest answer rather than one that steps through something else.
  usePeekNeighbours(
    useMemo(
      () =>
        (data?.hits ?? [])
          .filter((hit) => !hit.url && hit.container)
          .map((hit) => ({ kind: "page" as const, id: pageAddress(hit) })),
      [data],
    ),
  );

  return (
    <>
      <PageActions>
        {data?.backend ? <Tag appearance="outline">{data.backend}</Tag> : undefined}
      </PageActions>
      <PageNote>
        The company knowledge base, searched live the way an agent searches it — there is no local
        copy, so what you see here is what the backend holds right now.
      </PageNote>

      <form
        className="toolbar"
        onSubmit={(e) => {
          e.preventDefault();
          setQ(draft.trim());
        }}
      >
        <div style={{ flex: 1, maxWidth: 520 }}>
          {/* THEIR FIELD, WITH THE GLYPH IN ITS LEADING SLOT — which is what
              our own `SearchInput` was, plus the name carried as an
              `aria-label` rather than a prop of its own. */}
          <Input
            type="search"
            width="full"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            aria-label="Search the knowledge base"
            leading={<SearchGlyph size="sm" />}
            placeholder="Search the knowledge base — plain text, not a query language"
          />
        </div>
        <Button variant="primary" type="submit" leadingIcon={<SearchGlyph size="sm" />}>
          Search
        </Button>
        {q && (
          <Button
            variant="secondary"
            leadingIcon={<CloseGlyph size="sm" />}
            onClick={() => {
              setDraft("");
              setQ("");
            }}
          >
            Clear
          </Button>
        )}
      </form>

      {!q && (
        <EmptyState
          icon={<Book2Glyph size="xl" />}
          title="Search the company's shared knowledge"
          description="The engine runs this against the configured knowledge backend at query time — the same live search an agent gets at turn start and can re-run itself with search_knowledge. Nothing is cached here, so there is no staleness window."
        />
      )}

      {loading && <Skeleton variant="text" rows={4} label="Searching" />}

      {/* The search DID NOT RUN. `available: false` covers four states — no
          company, no backend, a backend with no org-wide read scope, and an
          index still building — so the engine's `note` is rendered rather
          than restated, and the REMEDY is chosen off `reason`. Neither is
          guessed from `backend`: it is empty for "no backend" and "no
          company" alike, and telling somebody with no company configured to
          go and wire a wiki is the wrong fix. */}
      {q && data?.available === false && (
        <Callout
          variant={data.reason === "building" ? "warning" : "neutral"}
          icon={data.reason === "building" ? <ScheduleGlyph size="md" /> : <Book2Glyph size="md" />}
        >
          <span>
            This search could not run: {data.note || "the engine gave no reason"}.
            {data.reason === "no_backend" && (
              <>
                {" "}
                Set <code className="inline">knowledge.backend</code> to{" "}
                <code className="inline">native</code> to use the engine's own knowledge base, or to{" "}
                <code className="inline">confluence</code> alongside an{" "}
                <code className="inline">integrations.confluence</code> block.
              </>
            )}
            {data.reason === "no_scope" && (
              <>
                {" "}
                The backend itself is fine — add the containers to search to{" "}
                <code className="inline">knowledge.scope</code>.
              </>
            )}
            {/* NOT A MISCONFIGURATION, and the banner must not read as one:
                this node is still indexing what it has projected, which is
                where a freshly joined node spends its first minutes. There is
                nothing to fix and nothing is lost. */}
            {data.reason === "building" && (
              <>
                {" "}
                Nothing is wrong — this node joined recently and is still indexing. Pages that exist
                are simply not findable from here yet. Try again in a moment.
              </>
            )}
          </span>
        </Callout>
      )}

      {/* The search DID run and came back degraded — a different banner,
          because an empty result that ran is not the same fact as one that
          never started. */}
      {q && data?.available !== false && data?.note && (
        // NO EXPLICIT ICON: `Callout` draws the variant's own mark, and for
        // `warning` that is the same glyph our banner reached for by name.
        <Callout variant="warning">
          The search did not complete: {data.note}. Knowledge search is best effort by design — a
          turn never dies because a wiki was slow — so an empty result here is not proof that
          nothing matches.
        </Callout>
      )}

      {q && (
        <QueryState
          error={error}
          loading={loading}
          empty={
            data?.hits?.length
              ? undefined
              : data?.available === false || data?.note
                ? undefined
                : {
                    title: `Nothing matched “${q}”`,
                    hint: "This is the backend's own answer, taken just now.",
                  }
          }
        >
          <Card padding="none">
            <Card.Header icon={<SearchGlyph size="sm" />} count={data?.hits?.length ?? 0}>
              <Card.Title>Results</Card.Title>
            </Card.Header>
            <div className="list">
              {(data?.hits ?? []).map((hit) => (
                <div key={hit.id} className="hit">
                  {/* A HIT THAT LINKS SOMEWHERE.
                      `internal/pages/search.go` builds a native hit with an
                      empty `URL` — deliberately, because the seam must not
                      know this dashboard's routes — and this rendered it
                      anyway, so every result on the engine's own backend was
                      an `href=""` that resolves to the dashboard root. A
                      vendor backend (Confluence) does send one, and that one
                      is external.

                      So the destination is chosen HERE, where the routes are
                      known: the page's own route for a native hit, the
                      vendor's link for a vendor one. */}
                  {hit.url ? (
                    <a className="hit-title" href={hit.url} target="_blank" rel="noreferrer">
                      {hit.title} <OpenInNewGlyph size="xs" style={{ display: "inline" }} />
                    </a>
                  ) : hit.container ? (
                    // THE CONTAINER AND THE TITLE, which is how a page is
                    // addressed now: the engine's own Get takes
                    // `CONTAINER/Title` and matches the title the way the
                    // fleet claimed it. A hit with no container has no page
                    // route, so it renders as text rather than as a link to
                    // nowhere.
                    //
                    // A PLAIN CLICK PEEKS. A ranked list is read by comparing
                    // the top few against each other, and the answer to "which
                    // of these did I mean" is the first paragraph of each —
                    // which is the one thing the snippet is capped too short to
                    // be. The `href` is still the page's own route, built from
                    // the frame's reference rather than a second copy of it, so
                    // ⌘-click and the middle button open the page as before.
                    <a
                      className="hit-title"
                      href={peekHref({ kind: "page", id: pageAddress(hit) })}
                      onClick={rowPeekHandler(() =>
                        openPeek({ kind: "page", id: pageAddress(hit) }),
                      )}
                    >
                      {hit.title}
                    </a>
                  ) : (
                    <span className="hit-title">{hit.title}</span>
                  )}
                  <div className="row gap-1">
                    {hit.container && <Tag appearance="outline">{hit.container}</Tag>}
                    {hit.updated_at && (
                      <span className="t-caption">updated {fmtDateTime(hit.updated_at)}</span>
                    )}
                  </div>
                  {hit.snippet && <p className="hit-snippet">{hit.snippet}</p>}
                </div>
              ))}
            </div>
            {/* THEIR FOOTER SLOT, `meta` rather than `actions`: this is a
                sentence about the list above it, not a row of buttons. */}
            <Card.Footer variant="meta">
              A snippet is capped by contract — it exists to say WHICH page to read, not to be the
              page.
            </Card.Footer>
          </Card>
        </QueryState>
      )}

      <Section
        title="What each seat has learned for itself"
        hint="private to the seat: its diary, its past turns, the skills it drafted"
      >
        {/* A HEADING OVER NOTHING. A company with no agent seats — which the
            quickstart's own example is — rendered this section's title and
            hint above an empty grid, so the screen appeared to be broken
            rather than to be describing a company that has none. */}
        {agentSeats.length === 0 ? (
          <EmptyState
            size="compact"
            icon={<NeurologyGlyph size="xl" />}
            title="No agent seats to have learned anything"
            description="Memory is per agent seat: a diary, past episodes, and the skills it drafted for itself. This company's seats are all human, so there is nothing private to show."
          />
        ) : (
          <div className="grid grid-auto">
            {agentSeats.slice(0, PEEK_SEATS).map((seat) => (
              <a
                key={seat.handle}
                className="seat-card"
                href={href(["company", "people", seat.handle], { tab: "memory" })}
              >
                <div className="row">
                  <span className="attention-icon" data-severity="info">
                    <DatabaseGlyph size="sm" />
                  </span>
                  <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
                    <strong className="truncate t-cell">{seat.name}</strong>
                    <span className="truncate t-caption mono">@{seat.handle}</span>
                  </span>
                  <ArrowForwardGlyph size="sm" />
                </div>
                <span className="t-caption truncate">
                  {seat.goal || "memory, episodes and skills"}
                </span>
              </a>
            ))}
          </div>
        )}
        {/* THE CUT, SAID, AND WHERE THE REST ARE. The same shape the container
            rail below uses for its own peek — a caption link counting what is
            not drawn — rather than a second visual language for one fact.
            Without it the grid stopped at twelve cards under a heading
            promising "each seat", which is a claim about a company the screen
            had decided not to show. */}
        {agentSeats.length > PEEK_SEATS && (
          <a className="t-link t-caption" href={href(["company", "people"])}>
            {plural(agentSeats.length - PEEK_SEATS, "more agent seat")} on the roster →
          </a>
        )}
      </Section>
    </>
  );
}

// ---------------------------------------------------------------------------
// The container
// ---------------------------------------------------------------------------

/**
 * How many pages a container's rail lists before it stops and points at the
 * browse.
 *
 * EIGHT, which is what fits under the panels above it without the rail
 * becoming a scroll of its own. The container's own browse is one click away
 * and is where a reader goes to see all of them — the rail's job is to say
 * what is being written here NOW, and eight is enough to recognise that.
 */
const PEEK_PAGES = 8;

/**
 * How many of a container's writers the rail names before it counts the rest.
 *
 * EIGHT, on the same reasoning and against the same width: past that the chips
 * wrap into a block that is read as a crowd rather than as names, and the
 * question this panel answers — "whose tree is this" — is answered by the
 * first few.
 */
const PEEK_WRITERS = 8;

/**
 * The four facts a container is read by, in one order, wherever it appears.
 *
 * ONE BUILDER, which is `ObjectHeader`'s own rule. Two of the four are things
 * the container document does NOT carry and no single read can state on its
 * own — when it last moved, and who writes to it — so they are derived here,
 * once, rather than assembled differently by whatever frame is drawing the
 * container this time.
 */
function containerFacts({
  container,
  newest,
  capped,
  unread,
  units,
  now,
}: {
  container: PageContainer;
  /** The newest `updated_at` among the pages this node returned, if any. */
  newest?: string;
  /** The page read came back at its own limit, so there may be more behind it. */
  capped: boolean;
  /** The page list has not answered — it is still in flight, or it failed. */
  unread: boolean;
  /**
   * The units whose `space:` names this container, or NULL when the company
   * document could not be read: `space` is guarded, so an anonymous reader
   * does not know who files here, and an empty list would say nobody does.
   */
  units: { name: string }[] | null;
  now: number;
}): Fact[] {
  const lead = units?.[0];
  return [
    // A COUNT, and ZERO IS A REAL ONE: a container exists from the first write
    // into it, so one whose pages have all been trashed is a state an operator
    // comes here for rather than an absence. Trashed pages are not counted —
    // `internal/pages` says so where it counts them — which is why this read
    // asks for the same set the list below shows.
    { label: "Pages", value: <NumberCell value={container.pages} /> },
    {
      label: "Last written",
      // A PAGE LIST THAT DID NOT ANSWER IS NOT AN EMPTY CONTAINER, which is
      // the same rule the panel below keeps and matters more here: this value
      // is DERIVED from that read alone, so a failed or in-flight one would
      // otherwise render as `DateCell`'s "never" — a container nobody has ever
      // written in, stated about a container nothing has been read about.
      value: unread ? (
        <EmptyValue label="The container's page list has not answered, so nothing here says when it last moved" />
      ) : (
        <DateCell at={newest} now={now} />
      ),
      // WHAT THE VALUE COVERS, which is what a note is for. The engine orders
      // a page list by container and TITLE, so a read that came back at its
      // limit is an alphabetical slice rather than the newest pages — and the
      // newest row in it is then a FLOOR on the real answer. Said plainly
      // rather than silently: a date that is merely the best of what was read
      // is indistinguishable from the truth until it is wrong.
      note: capped ? "newest of the pages this read returned" : undefined,
    },
    {
      // WHO WRITES HERE, from the CONFIG rather than from the pages: a unit's
      // `space:` is what sends its seats' pages into this container, so it
      // answers the question even for a container nobody has written in yet.
      // The panel below answers the other half — who actually has.
      label: "Filed by",
      value: !units ? (
        <EmptyValue label="Needs an operator token to read" />
      ) : units.length > 0 ? (
        units.map((u) => u.name).join(", ")
      ) : (
        <EmptyValue label="No unit names this container in its space:" />
      ),
      // A LINK ONLY WHERE THERE IS ONE PLACE TO GO. Two units filing into one
      // container is legal and happens — a shared space — and a fact line that
      // linked the first of them would be a link that is right half the time.
      path: units?.length === 1 && lead ? ["company", "units", lead.name] : undefined,
    },
    { label: "Created", value: <DateCell at={container.created_at} now={now} /> },
  ];
}

/**
 * One container, in the rail.
 *
 * A CONTAINER IS AN OBJECT rather than a filter value — it has a name, a
 * purpose, a creation instant, a page count and a unit that files into it —
 * and its "page" is the browse filtered to it, which states none of that. So
 * the rail answers what the browse cannot: what this container is FOR, what
 * has been written in it lately, and whose tree it is.
 *
 * # It lives here rather than beside the browse
 *
 * The browse renders a LIST of pages filtered to a container; it is not the
 * container's own frame, and the fact it cannot state — who files here — comes
 * from the ORG TREE rather than from any page read, which is the source this
 * screen already holds.
 *
 * # Two reads, and only one of them may blank the panel
 *
 * The container itself comes from `containers` and its pages from `pages`, so
 * a failed page list is a failed SUBTREE read rather than an empty container —
 * the same rule the tracker's subtask panel keeps. It is drawn where the rows
 * would have been, and only a read that actually answered is allowed to
 * conclude that nothing has been written here.
 */
export function ContainerPeek({ id }: { id: string }) {
  const org = useOrg();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const containers = useQuery("containers", undefined, { enabled: id !== "", pollMs: 60_000 });
  // THE SAME SET THE COUNT COUNTS. `containers` excludes trashed pages from
  // `pages` deliberately — "a reader clicks 12 and finds nine" is the reason
  // in its own source — and an unfiltered list here would put the trashed ones
  // back under a number that does not include them.
  const list = useQuery(
    "pages",
    { container: id, status: "published,draft" },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const found = containers.data?.containers.find((c) => c.key === id);
  // NEWEST FIRST, sorted here: the engine orders a page list by container and
  // title, which is the order a browse wants and the opposite of what "what
  // has been written lately" asks for.
  const recent = useMemo(
    () => [...(list.data?.pages ?? [])].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [list.data],
  );
  // WHETHER THAT READ SAW THE WHOLE CONTAINER, ASKED rather than inferred:
  // the engine takes one row past the limit as evidence and answers
  // `truncated`, so a container holding exactly the limit reports itself
  // whole. `recent.length >= list.data.limit` was the inference, and it put
  // "there may be more behind this" on every container that happened to hold
  // a round number of pages.
  const capped = Boolean(list.data?.truncated);
  // WHO FILES HERE IS GUARDED. A unit's `space:` is the knowledge container
  // it owns, and `internal/api/orgprojection_test.go` classifies it as guarded
  // ("a knowledge container key: where this unit's pages are written"), so the
  // anonymous org projection carries none of it and this is read from the
  // company document. NULL rather than an empty list when it could not be:
  // "no unit files here" is a fact about the company and an unread document is
  // not evidence for it.
  //
  // A UNIT'S `space:` IS CASE-INSENSITIVE against the key, because the engine
  // upper-cases a container key on the way in and a config file says whatever
  // its author typed.
  const doc = useQuery("config", undefined, { enabled: id !== "" });
  const units = useMemo(() => {
    if (doc.error || !doc.data) return null;
    return documentUnits(doc.data)
      .filter((u) => (u.space ?? "").toUpperCase() === id.toUpperCase())
      .map((u) => ({ name: u.name }));
  }, [doc.data, doc.error, id]);

  return (
    <>
      {containers.loading && !containers.data && (
        <Skeleton variant="text" rows={6} label="Loading the container" />
      )}
      <QueryState error={containers.error} loading={containers.loading}>
        {containers.data &&
          (found ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Container"
                icon="folder"
                // THE KEY IS THE IDENTIFIER and the name is the title, which
                // are different strings often enough to matter: a container
                // declared by a unit's `space:` carries the unit's name and is
                // addressed by a short upper-case key nobody would guess from
                // it.
                identifier={found.key}
                title={found.name || found.key}
                facts={containerFacts({
                  container: found,
                  newest: recent[0]?.updated_at,
                  capped,
                  unread: !list.data,
                  units,
                  now,
                })}
              />
              <div className="col gap-3">
                {/* WHAT IT IS FOR, always drawn — including when nobody wrote
                    one. "Is this the one I meant" is the question the rail
                    answers, and a purpose missing from the panel reads as one
                    the reader failed to scroll to. */}
                <Card>
                  <Card.Header icon={<TargetGlyph size="sm" />}>
                    <Card.Title>Purpose</Card.Title>
                  </Card.Header>
                  {found.purpose ? (
                    <p className="t-body measure">{found.purpose}</p>
                  ) : (
                    <span className="muted">No purpose is written for this container.</span>
                  )}
                </Card>

                <ContainerPages container={found} recent={recent} list={list} now={now} />
                <ContainerWriters recent={recent} index={index} />
              </div>
            </>
          ) : (
            // AN ADDRESS THAT DID NOT RESOLVE, named. The rail is the one
            // place a reader arrives at a container they never saw in a list —
            // from a pasted URL, or from a sidebar row read before the key was
            // renamed — and "no such record" would leave them unable to tell a
            // stale link from a container this node has not caught up with.
            <EmptyState
              size="compact"
              icon={<FolderGlyph size="xl" />}
              title={`No container called “${id}”`}
              description="A container is created the first time somebody writes into it — give a unit a `space` and its seats will have somewhere to file what they learn. This node may also simply not have caught up with one that exists."
            />
          ))}
      </QueryState>
    </>
  );
}

/**
 * What has been written in a container lately.
 *
 * ITS OWN `QueryState`, drawn WHERE THE ROWS WOULD HAVE BEEN: this is the only
 * read that can see the container's pages, so a refusal rendered as no panel
 * would say "nothing is filed here" about a container holding four hundred
 * pages. Only a read that answered may conclude the container is empty.
 */
function ContainerPages({
  container,
  recent,
  list,
  now,
}: {
  container: PageContainer;
  recent: PageSummary[];
  list: { error: string | null; loading: boolean };
  now: number;
}) {
  return (
    <Card>
      <Card.Header icon={<DescriptionGlyph size="sm" />} count={recent.length}>
        <Card.Title>Recent pages</Card.Title>
      </Card.Header>
      <QueryState
        error={list.error}
        loading={list.loading}
        empty={
          recent.length
            ? undefined
            : {
                title: "Nothing is filed here",
                hint: "A container is created by the first write into it. Its pages may since have been trashed, or this node's copy has not caught up.",
              }
        }
      >
        <div className="col gap-1">
          {recent.slice(0, PEEK_PAGES).map((page) => (
            <span key={page.id} className="row gap-2">
              <PageLink page={page} />
              <span className="spacer" />
              <DateCell at={page.updated_at} now={now} />
            </span>
          ))}
          {recent.length > PEEK_PAGES && (
            <a className="t-link t-caption" href={href(["knowledge", container.key])}>
              {plural(recent.length - PEEK_PAGES, "more page")} in this container →
            </a>
          )}
        </div>
      </QueryState>
    </Card>
  );
}

/**
 * Who has actually written in this container.
 *
 * THE CREATORS, not the last editors, and the subtitle says so: a page's
 * `author` is stamped by its create and no save moves it (see [pageFacts] in
 * `Pages.tsx` for where that is decided in the engine). Naming them "writers"
 * without that qualification would make a container whose pages one seat
 * started and another has rewritten look like the first seat's tree.
 */
function ContainerWriters({ recent, index }: { recent: PageSummary[]; index: OrgIndex }) {
  const writers = useMemo(() => {
    const seen: string[] = [];
    for (const page of recent) {
      if (page.author && !seen.includes(page.author)) seen.push(page.author);
    }
    return seen;
  }, [recent]);

  return (
    <Card>
      <Card.Header
        icon={<GroupGlyph size="sm" />}
        count={writers.length}
        subtitle="the seats that started these pages"
      >
        <Card.Title>Who writes here</Card.Title>
      </Card.Header>
      {writers.length > 0 ? (
        <div className="row wrap gap-2">
          {writers.slice(0, PEEK_WRITERS).map((handle) => (
            <SeatChip
              key={handle}
              name={index.byHandle.get(handle)?.name ?? handle}
              handle={handle}
            />
          ))}
          {writers.length > PEEK_WRITERS && (
            <span className="t-caption">+{writers.length - PEEK_WRITERS} more</span>
          )}
        </div>
      ) : (
        // TWO ABSENCES, told apart. A container with pages and no author on
        // any of them was written by the ENGINE — the tool-skill catalogue
        // publishes exactly that — and saying "nobody" about it would call a
        // working sync an empty space.
        <span className="muted">
          {recent.length > 0
            ? "Every page here was written by the engine — the tool-skill catalogue and the syncs carry no author."
            : "Nothing has been written here yet."}
        </span>
      )}
    </Card>
  );
}
