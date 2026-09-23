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
import { CutNote, QueryState, Section, SeatChip } from "~/components/common.tsx";
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
import { fmtDateTime, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useMemo } from "react";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { DateCell, NumberCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import type {
  PageActivityAnswer,
  PageChange,
  PageContainer,
  PagesAnswer,
} from "~/protocol/index.ts";
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
 * IT IS A PEEK, LIKE [PEEK_PAGES] BELOW IT, so it is bounded here rather than
 * in the read: the seats come from the org projection this screen already holds,
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
 * The changes a container's rail reads everything it says about writing from.
 *
 * `page_activity` narrowed to the container and to the two kinds that WRITE a
 * page — a create and a save — newest first, at the feed's own ceiling
 * (`pages.MaxPageChanges`), because the rail folds changes into pages and a
 * page saved many times spends several of them.
 *
 * NOT THE PAGE LIST. `pages` is ordered by container and TITLE and cut at its
 * limit, so on a container larger than one read it was an alphabetical slice:
 * the "recent" pages were the newest of the first fifty by title, the writers
 * those of the same slice, and "N more pages" a count of that slice's
 * remainder. The feed IS in order of writing, so the newest page written here
 * is its first row however large the container is.
 */
const WRITING = { kinds: "created,saved", limit: 100 } as const;

/**
 * The container's trashed pages, which the rail takes OUT of the feed above.
 *
 * THE SAME SET THE COUNT COUNTS. `containers` leaves trashed pages out of a
 * container's `pages` — "a reader clicks 12 and finds nine" is the reason in
 * its own source — and a trash keeps the page's head, container and all, so
 * the feed's creates and saves of a trashed page still come back filtered to
 * this container. The feed carries no status to tell them apart; this read is
 * what does.
 *
 * At `pages.MaxLimit`, the most one page listing returns, and the answer's
 * `truncated` is read: past it the rail cannot rule a row out, and says so.
 */
const TRASHED = { status: "trashed", limit: 500 } as const;

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
  unread,
  unsure,
  units,
  now,
}: {
  container: PageContainer;
  /** When the newest create or save of a page still here landed, if any did. */
  newest?: string;
  /** The writes have not been read — a read is still in flight, or failed. */
  unread: boolean;
  /** The trash was cut, so a trashed page's write may stand in `newest`. */
  unsure: boolean;
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
    // `internal/pages` says so where it counts them.
    { label: "Pages", value: <NumberCell value={container.pages} /> },
    {
      label: "Last written",
      // A FEED THAT DID NOT ANSWER IS NOT AN EMPTY CONTAINER: this value is
      // DERIVED from that read alone, so a failed or in-flight one would
      // otherwise render as `DateCell`'s "never" — a container nobody has ever
      // written in, stated about a container nothing has been read about.
      //
      // THE FEED'S FIRST ROW TO A PAGE STILL HERE: it is newest first, so the
      // newest write is on its first page however long the history is — unless
      // every write that page holds is to a page since trashed.
      value: unread ? (
        <EmptyValue label="The container's writes have not been read, so nothing here says when it was last written" />
      ) : (
        <DateCell at={newest} now={now} />
      ),
      note: unsure ? "may be a trashed page's — see below" : undefined,
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
 * # Only the container's own read may blank the panel
 *
 * The container itself comes from `containers`, and what was written in it
 * from the change feed less the trash, so a failed read of either of those is
 * a failed SUBTREE read rather than an empty container — the same rule the
 * tracker's subtask panel keeps. It is drawn where the rows would have been,
 * and only reads that actually answered are allowed to conclude that nothing
 * has been written here.
 */
export function ContainerPeek({ id }: { id: string }) {
  const org = useOrg();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const containers = useQuery("containers", undefined, { enabled: id !== "", pollMs: 60_000 });
  // WHAT HAS BEEN WRITTEN HERE, newest first — see [WRITING] — less what has
  // since been trashed — see [TRASHED].
  const activity = useQuery(
    "page_activity",
    { container: id, ...WRITING },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const trashed = useQuery(
    "pages",
    { container: id, ...TRASHED },
    { enabled: id !== "", pollMs: 20_000 },
  );
  const writes = useMemo(
    () => liveWrites(activity.data, trashed.data),
    [activity.data, trashed.data],
  );
  const feed: WritingFeed = {
    ...writes,
    error: activity.error ?? trashed.error,
    loading: activity.loading || trashed.loading,
  };
  const found = containers.data?.containers.find((c) => c.key === id);
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
                  newest: feed.changes?.[0]?.at,
                  unread: !feed.changes,
                  unsure: feed.unsure,
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

                <ContainerPages container={found} feed={feed} now={now} />
                <ContainerWriters feed={feed} index={index} />
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
 * The writes [ContainerPages] and [ContainerWriters] are drawn from: the
 * feed's creates and saves, less those of a page in the trash.
 */
type WritingFeed = {
  /** NULL until BOTH reads answered: half of them is not the set. */
  changes: PageChange[] | null;
  /** How many writes the feed returned, trashed pages' included. */
  read: number;
  /** The feed filled its page, so there are older writes than these. */
  cut: boolean;
  /** The trash filled its page, so a trashed page's writes may be among these. */
  unsure: boolean;
  error: string | null;
  loading: boolean;
};

/** The feed's writes to pages that are not in the trash — see [TRASHED]. */
export function liveWrites(
  activity: PageActivityAnswer | null,
  trashed: PagesAnswer | null,
): Pick<WritingFeed, "changes" | "read" | "cut" | "unsure"> {
  const gone = new Set((trashed?.pages ?? []).map((page) => page.id));
  return {
    changes:
      activity && trashed ? activity.changes.filter((change) => !gone.has(change.page_id)) : null,
    read: activity?.changes.length ?? 0,
    cut: Boolean(activity?.next_cursor),
    unsure: Boolean(trashed?.truncated),
  };
}

/**
 * The pages a feed of changes touched, newest write first, each once.
 *
 * A PURGED PAGE IS SKIPPED: the feed keeps its entries as the record it ever
 * existed, with no title and no container, and there is no page to link to.
 */
export function writtenPages(changes: readonly PageChange[]): PageChange[] {
  const seen = new Set<string>();
  const out: PageChange[] = [];
  for (const change of changes) {
    if (!change.title || seen.has(change.page_id)) continue;
    seen.add(change.page_id);
    out.push(change);
  }
  return out;
}

/**
 * What has been written in a container lately.
 *
 * ITS OWN `QueryState`, drawn WHERE THE ROWS WOULD HAVE BEEN: these are the
 * only reads that can see what was written here, so a refusal rendered as no
 * panel would say "nothing is filed here" about a container holding four
 * hundred pages. Only reads that answered may conclude nothing was written.
 *
 * CUT AT [PEEK_PAGES] and marked by the footer, which links to the container's
 * browse — where every page is — named by a count rather than by the rows,
 * which are a few of its pages and never a count of them.
 */
function ContainerPages({
  container,
  feed,
  now,
}: {
  container: PageContainer;
  feed: WritingFeed;
  now: number;
}) {
  const pages = useMemo(() => writtenPages(feed.changes ?? []), [feed.changes]);
  const shown = pages.slice(0, PEEK_PAGES);
  // EITHER SIGN OF MORE: the container's count, and rows past the cut. The
  // count is polled on a slower cadence than the writes, so a page created
  // since it was read is a row it does not count yet.
  const total = Math.max(container.pages, pages.length);
  return (
    <Card>
      {/* NO COUNT: the rows are the few most recently written, and a number
          beside that title would read as how many were. The container's own
          count is the header's Pages fact and names the link below. */}
      <Card.Header icon={<DescriptionGlyph size="sm" />}>
        <Card.Title>Recently written</Card.Title>
      </Card.Header>
      <QueryState
        error={feed.error}
        loading={feed.loading}
        empty={
          !feed.changes || pages.length
            ? undefined
            : {
                title: "Nothing written here is still on record",
                hint:
                  container.pages > 0
                    ? "Its pages are on the browse; no write this rail read is to one of them."
                    : "A container is created by the first write into it. Its pages may since have been trashed, or this node's copy has not caught up.",
              }
        }
      >
        <div className="col gap-1">
          {shown.map((change) => (
            <span key={change.page_id} className="row gap-2">
              <PageLink
                page={{ container: change.container || container.key, title: change.title ?? "" }}
              />
              <span className="spacer" />
              <DateCell at={change.at} now={now} />
            </span>
          ))}
        </div>
        {/* THE TRASH WAS CUT, so it cannot rule every row out. */}
        {feed.unsure && (
          <p className="t-caption">
            This container has more pages in its trash than one read returns, so a page or a writer
            here may be one of the trashed ones.
          </p>
        )}
      </QueryState>
      {/* `kind=all`: the browse leaves tool-skill pages out by default, and
          the container's count and the rows above both include them. */}
      {total > shown.length && (
        <Card.Footer variant="meta">
          <a
            className="t-link t-caption"
            href={href(["knowledge", container.key], { kind: "all" })}
          >
            All {plural(total, "page")} in this container →
          </a>
        </Card.Footer>
      )}
    </Card>
  );
}

/**
 * Who has been writing in this container.
 *
 * THE SEATS BEHIND THE CREATES AND SAVES OF PAGES STILL HERE, every one of
 * them: the read is bounded by [WRITING], so the set is too, and a chip a
 * reader cannot reach is no chip at all. When the feed was cut its footer says
 * the set is of the newest writes, and that each page's own change history —
 * which pages back with a cursor — holds the older ones.
 */
function ContainerWriters({ feed, index }: { feed: WritingFeed; index: OrgIndex }) {
  const { changes } = feed;
  const writers = useMemo(() => {
    const seen: string[] = [];
    for (const change of changes ?? []) {
      if (change.actor && !seen.includes(change.actor)) seen.push(change.actor);
    }
    return seen;
  }, [changes]);

  return (
    <Card>
      <Card.Header
        icon={<GroupGlyph size="sm" />}
        count={changes ? writers.length : undefined}
        subtitle="who created or saved a page not in the trash"
      >
        <Card.Title>Who writes here</Card.Title>
      </Card.Header>
      {/* NOTHING IS CONCLUDED FROM READS THAT HAVE NOT ANSWERED — the rule
          [ContainerPages] keeps — so an unread container is not one "nothing
          has been written" in. */}
      {!changes ? (
        <QueryState error={feed.error} loading={feed.loading} />
      ) : writers.length > 0 ? (
        <div className="row wrap gap-2">
          {writers.map((handle) => (
            <SeatChip
              key={handle}
              name={index.byHandle.get(handle)?.name ?? handle}
              handle={handle}
            />
          ))}
        </div>
      ) : (
        // TWO ABSENCES, told apart. Writes with no actor on any of them were
        // the ENGINE's — the tool-skill catalogue publishes exactly that — and
        // saying "nobody" about them would call a working sync an empty space.
        <span className="muted">
          {changes.length > 0
            ? "Every write here was the engine's — the tool-skill catalogue and the syncs carry no author."
            : "Nobody has written a page here that is not in the trash."}
        </span>
      )}
      {changes && (
        <CutNote
          shown={feed.read}
          more={feed.cut}
          one="write"
          slice="newest"
          whole="These are the seats behind those of them to pages not in the trash; each page's own change history holds the older ones."
        />
      )}
    </Card>
  );
}
