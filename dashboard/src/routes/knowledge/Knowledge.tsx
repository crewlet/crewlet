/**
 * Knowledge — the company's own pages, searched and browsed.
 *
 * # What this screen is
 *
 * The pane beside the Knowledge tree (`KnowledgeTree.tsx`, which holds the
 * search box and its mode): with a phrase in the address it is the RANKED
 * ANSWER, and with none it is the spaces at a glance.
 *
 * # What a search here reads — the native truth
 *
 * On the engine's own knowledge base a search reads THIS NODE'S OWN COPY of
 * the pages: the replicated rows every node applies from the pages log, the
 * lexical index this node keeps over them, and the replicated vectors — the
 * same fan-out a seat's `search_knowledge` runs, divided across the live
 * nodes by bucket. It is as current as this node's place on that log, not a
 * live read of somewhere else. (It said "no local copy … no staleness window"
 * here, which was true of the Confluence backend it was written for and false
 * of the one most companies run.) On Confluence the search IS live, at query
 * time, against the site.
 *
 * # Three modes, and what was served
 *
 * `mode=hybrid|keyword|semantic` — Hybrid, Keyword and Meaning to a reader.
 * The answer says what it actually ranked by (`served_mode`) and why that
 * differs from what was asked (`degraded`), and a partial fan-out names the
 * part it did not cover (`coverage`). Each is a sentence above the hits,
 * because each is a fact about every one of them.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, Section, SeatChip } from "~/components/common.tsx";
import { Callout, Card, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  ClockGlyph,
  ExternalLinkGlyph,
  FileTextGlyph,
  FolderGlyph,
  SearchGlyph,
  TargetGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg, seatLookup, type OrgIndex } from "~/lib/seats.ts";
import { plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { modeLabel, resolveSearchMode, searchCoverageNote, servedNote } from "~/lib/search.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { DateCell, NumberCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import type {
  KnowledgeAnswer,
  LogRefusal,
  PageContainer,
  PageSummary,
  QueryRefusal,
} from "~/protocol/index.ts";
// THE BROWSE'S OWN SPELLING of a page's address and of a link that peeks,
// rather than a second one here: a hit, a grid row and a container's page list
// must resolve to the same `peek=` token, or the stepper walks past the page
// the reader just opened and one of the three forgets the middle button.
import { PageLink } from "./Pages.tsx";
import { PAGES_WINDOW } from "./usePagedPages.ts";
import { ownerSentence, useSpaceOwners, type SpaceOwners } from "./spaceOwners.ts";
import { plainText } from "~/lib/markdown.ts";

export function Knowledge() {
  const [q] = useParam("q", "");
  const [modeRaw] = useParam("mode", "");
  const phrase = q.trim();
  // THE PROBE: which backend, whether it can search at all — the home's one
  // question about the search — and which modes it serves, which is what an
  // address naming no mode runs in (`defaultSearchMode`, the same rule the
  // tree's control draws). It runs nothing, so asking it beside a search
  // costs a round trip and no ranking. See `KnowledgeTree`.
  const probe = useQuery("knowledge", { q: "", mode: "semantic" });
  const mode = resolveSearchMode(modeRaw || null, probe.data);
  // AN ADDRESS WITH NO MODE WAITS FOR THE PROBE rather than running hybrid
  // first: on a company with no embeddings provider that first answer is
  // "asked for Hybrid, served Keyword" — a degradation of a choice nobody
  // made, drawn and then replaced. A probe that failed settles it too, at the
  // engine's default.
  const settled = modeRaw !== "" || probe.data !== null || probe.error !== null;
  // Searching is a real request, so it runs on submit — the phrase is in the
  // address — rather than on every keystroke.
  const asked = useQuery("knowledge", { q: phrase, mode }, { enabled: phrase !== "" && settled });
  const search = { ...asked, loading: asked.loading || (phrase !== "" && !settled) };
  const answer = phrase ? search.data : probe.data;

  return (
    <>
      <PageActions>
        {answer?.backend ? (
          <Tag appearance="outline" title="The knowledge backend this company runs">
            {answer.backend}
          </Tag>
        ) : undefined}
      </PageActions>
      {answer?.available === false && <Unavailable answer={answer} />}
      {phrase ? (
        <Results phrase={phrase} search={search} />
      ) : (
        <KnowledgeHome backend={probe.data?.backend ?? ""} />
      )}
    </>
  );
}

/**
 * The search DID NOT RUN, and why — chosen off `reason`, never off the prose
 * `note` or an empty `backend` (which means "no backend" and "no company"
 * alike, and the remedies differ).
 */
function Unavailable({ answer }: { answer: KnowledgeAnswer }) {
  const building = answer.reason === "building";
  return (
    <Callout
      variant={building ? "warning" : "neutral"}
      icon={building ? <ClockGlyph size="md" /> : <BookOpenGlyph size="md" />}
    >
      <span>
        Search cannot run here: {answer.note || "the engine gave no reason"}.
        {answer.reason === "no_backend" && (
          <>
            {" "}
            Set <code className="inline">knowledge.backend</code> to{" "}
            <code className="inline">native</code> to use the engine&rsquo;s own knowledge base, or
            to <code className="inline">confluence</code> alongside an{" "}
            <code className="inline">integrations.confluence</code> block.
          </>
        )}
        {answer.reason === "no_scope" && (
          <>
            {" "}
            The backend itself is fine — add the spaces to search to{" "}
            <code className="inline">knowledge.scope</code>.
          </>
        )}
        {/* NOT A MISCONFIGURATION: this node joined recently and is still
            indexing what it holds. Nothing is lost and nothing needs fixing. */}
        {building && (
          <>
            {" "}
            Nothing is wrong — this node joined recently and is still indexing. Pages that exist are
            not findable from here yet; try again in a moment.
          </>
        )}
      </span>
    </Callout>
  );
}

function Results({
  phrase,
  search,
}: {
  phrase: string;
  search: {
    data: KnowledgeAnswer | null;
    loading: boolean;
    error: string | null;
    refusal: QueryRefusal | LogRefusal | null;
  };
}) {
  const { open: openPeek } = usePeekControls();
  const data = search.data;
  const hits = useMemo(() => data?.hits ?? [], [data]);
  // WHAT `[` AND `]` WALK: the hits in the engine's ranked order — the order
  // they are drawn in. NATIVE HITS ONLY, with no gap in the middle: one
  // backend serves a company, so a result set is all native (pages this
  // engine holds) or all vendor (URLs in somebody else's wiki, with nothing
  // here to peek at).
  usePeekNeighbours(
    useMemo(
      () =>
        hits
          .filter((hit) => !hit.url && hit.container)
          .map((hit) => ({ kind: "page" as const, id: hit.id })),
      [hits],
    ),
  );
  if (search.loading && !data) return <Skeleton variant="text" rows={5} label="Searching" />;
  if (data?.available === false) return null;

  const served = servedNote(data);
  const partial = searchCoverageNote(data);
  const ran = Boolean(data?.served_mode);
  const native = data?.backend === "native";

  return (
    <>
      {/* WHAT WAS SERVED, when it is not what was asked; and WHAT WAS NOT
          COVERED, when the search is partial. Above the hits, because each is
          a fact about every one of them. */}
      {served && <Callout variant="info">{served}</Callout>}
      {partial && (
        <Callout variant="warning" className="coverage-note">
          {partial.sentence}
          {partial.nodes.length > 0 && (
            <ul className="coverage-nodes">
              {partial.nodes.map((n) => (
                <li key={n.id}>
                  <code className="inline">{n.id}</code>
                  {n.error ? ` — ${n.error}` : ""}
                </li>
              ))}
            </ul>
          )}
        </Callout>
      )}
      <QueryState
        error={search.error}
        refusal={search.refusal}
        loading={search.loading}
        empty={
          hits.length || !ran
            ? undefined
            : {
                title: `Nothing matched “${phrase}”`,
                hint: native
                  ? `Ranked by ${modeLabel(data?.served_mode ?? "")} over this node’s copy of the pages. Another mode may find what these words did not.`
                  : "This is the wiki’s own answer, taken just now.",
              }
        }
      >
        {hits.length > 0 && (
          <Card padding="none">
            <Card.Header
              icon={<SearchGlyph size="sm" />}
              count={hits.length}
              // WHAT RANKED THEM, always — not only when it differs from what
              // was asked: a reader comparing two searches has to know each
              // one's ranking without remembering the segment's state.
              subtitle={ran ? `ranked by ${modeLabel(data?.served_mode ?? "")}` : undefined}
            >
              <Card.Title>Results</Card.Title>
            </Card.Header>
            <ol className="list k-list">
              {hits.map((hit) => (
                <li key={hit.id} className="hit">
                  {/* THE DESTINATION IS CHOSEN HERE, where the routes are
                      known: a native hit has no URL (the seam must not know
                      this dashboard's routes) and opens the page, a vendor
                      hit carries the wiki's own link. A native hit with no
                      container has no page to open and is text. */}
                  {hit.url ? (
                    <a className="hit-title" href={hit.url} target="_blank" rel="noreferrer">
                      {hit.title}{" "}
                      <ExternalLinkGlyph
                        size="xs"
                        style={{ display: "inline" }}
                        aria-hidden="true"
                      />
                      <span className="sr-only"> (opens the wiki in a new tab)</span>
                    </a>
                  ) : hit.container ? (
                    // A PLAIN CLICK PEEKS: a ranked list is read by comparing
                    // the top few, and the first paragraph of each is what the
                    // snippet is too short to be. The href is the page's own
                    // route, so ⌘-click and the middle button open it.
                    <a
                      className="hit-title"
                      href={peekHref({ kind: "page", id: hit.id })}
                      onClick={rowPeekHandler(() => openPeek({ kind: "page", id: hit.id }))}
                    >
                      {hit.title}
                    </a>
                  ) : (
                    <span className="hit-title">{hit.title}</span>
                  )}
                  {hit.container && (
                    <div className="row gap-1">
                      <Tag appearance="outline" monospace>
                        {hit.container}
                      </Tag>
                    </div>
                  )}
                  {/* A CUT OF A MARKDOWN PAGE, drawn as the prose it renders
                      to rather than with its marks in it. */}
                  {hit.snippet && <p className="hit-snippet">{plainText(hit.snippet)}</p>}
                </li>
              ))}
            </ol>
            <Card.Footer variant="meta">
              {native
                ? "Searched on this node’s own copy of the pages — the same search an agent’s search_knowledge runs — and as current as this node’s place on the pages log."
                : "Searched live on the wiki at query time, as the engine’s own account."}
            </Card.Footer>
          </Card>
        )}
      </QueryState>
    </>
  );
}

/**
 * The spaces at a glance, before anybody searches: what each is for and how
 * much is in it, one card per space.
 */
function KnowledgeHome({ backend }: { backend: string }) {
  const containers = useQuery("containers", undefined, { pollMs: 60_000 });
  const spaces = useMemo(
    () =>
      [...(containers.data?.containers ?? [])].sort((a, b) =>
        (a.name || a.key).localeCompare(b.name || b.key),
      ),
    [containers.data],
  );
  if (containers.error === "unknown_query") {
    // A VENDOR WIKI: nothing on this node to browse, by design.
    return (
      <EmptyState
        icon={<SearchGlyph size="xl" />}
        title="Search your wiki from the search box"
        description={`This company’s pages live in ${backend || "its wiki"}, so there is nothing on this engine to browse — the search reaches the wiki live, as the engine’s own account.`}
      />
    );
  }
  return (
    <>
      <PageNote>
        The company&rsquo;s own pages. Search ranks them by their words, their meaning or both — the
        same search an agent runs — and the tree browses them by space.
      </PageNote>
      <QueryState
        error={containers.error}
        refusal={containers.refusal}
        loading={containers.loading}
        empty={
          spaces.length
            ? undefined
            : {
                title: "Nothing has been written down yet",
                hint: "A space is created the first time somebody writes into it. Give a unit a `space` and its agents will have somewhere to file what they learn.",
              }
        }
      >
        <Section title="Spaces" hint="where each team files what it writes down">
          <ul className="grid grid-auto k-list" role="list">
            {spaces.map((space) => (
              <li key={space.key}>
                <a className="k-space" href={href(["knowledge", space.key])}>
                  <span className="row gap-2">
                    <FolderGlyph size="sm" aria-hidden="true" />
                    <strong className="truncate">{space.name || space.key}</strong>
                    <span className="spacer" />
                    <span className="ktree-key mono">{space.key}</span>
                  </span>
                  <span className="t-caption clamp">
                    {space.purpose || "No purpose is written for this space."}
                  </span>
                  <span className="t-caption">{plural(space.pages, "page")}</span>
                </a>
              </li>
            ))}
          </ul>
        </Section>
      </QueryState>
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
  owners,
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
   * Who files into this container, in the four states `spaceOwners.ts` names:
   * `space` is guarded, so a reader without the company document does not know
   * who files here, and an empty list would say nobody does.
   */
  owners: SpaceOwners;
  now: number;
}): Fact[] {
  const units = owners.state === "read" ? owners.units : null;
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
      // THE STATE'S OWN SENTENCE while there is no list: "needs a grant" said
      // to an operator whose read is merely in flight is false.
      value: !units ? (
        <EmptyValue label={ownerSentence(owners)} />
      ) : units.length > 0 ? (
        units.map((u) => u.name).join(", ")
      ) : (
        <EmptyValue label="No unit names this container in its space:" />
      ),
      // A LINK ONLY WHERE THERE IS ONE PLACE TO GO. Two units filing into one
      // container is legal and happens — a shared space — and a fact line that
      // linked the first of them would be a link that is right half the time.
      path: units?.length === 1 && lead?.id ? ["agents", "teams", lead.id] : undefined,
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
  //
  // ONE WINDOW OF `PAGES_WINDOW`, not the engine's default fifty: every
  // statement below — what moved last, who writes here — is derived from the
  // rows this read returned, and a default window made them statements about
  // the first fifty pages BY TITLE of a container of four hundred. What the
  // rail COUNTS comes from the answer's `total`, never from the rows.
  const list = useQuery(
    "pages",
    { container: id, status: "published,draft", limit: PAGES_WINDOW },
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
  // HOW MANY PAGES THE LISTING MATCHES IN ALL — the answer's own `total`,
  // which is the same set (trashed excluded) as the container's count — or
  // null before there is an answer.
  const total = list.data ? (list.data.total ?? recent.length) : null;
  // WHETHER THAT READ SAW THE WHOLE CONTAINER: the engine says so by counting
  // more than it returned, or by handing back a cursor to the next window.
  // False until there IS an answer, since nothing has been read to qualify.
  const capped = Boolean(list.data && (list.data.after || (total ?? 0) > recent.length));
  // WHO FILES HERE IS GUARDED. A unit's `space:` is the knowledge container
  // it owns, and `internal/api/orgprojection_test.go` classifies it as guarded
  // ("a knowledge container key: where this unit's pages are written"), so the
  // anonymous org projection carries none of it and this is read from the
  // company document — in the four states `spaceOwners.ts` names, the tree's
  // own, because "no unit files here" is a fact about the company and an
  // unread document is not evidence for it.
  const owners = useSpaceOwners({ enabled: id !== "" })(id);

  return (
    <>
      {containers.loading && !containers.data && (
        <Skeleton variant="text" rows={6} label="Loading the container" />
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
                  owners,
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

                <ContainerPages
                  container={found}
                  recent={recent}
                  total={total}
                  capped={capped}
                  list={list}
                  now={now}
                />
                <ContainerWriters recent={recent} total={total} capped={capped} index={index} />
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
  total,
  capped,
  list,
  now,
}: {
  container: PageContainer;
  recent: PageSummary[];
  /** The listing's own total, or null before it answered. */
  total: number | null;
  /** The read returned fewer pages than the container holds. */
  capped: boolean;
  list: { error: string | null; refusal: QueryRefusal | LogRefusal | null; loading: boolean };
  now: number;
}) {
  const shown = Math.min(recent.length, PEEK_PAGES);
  // THE REST OF THE CONTAINER, counted off the TOTAL: the rows are one window,
  // and "42 more pages" under a container of four hundred was the window
  // subtracted from itself.
  const rest = (total ?? recent.length) - shown;
  return (
    <Card>
      <Card.Header
        icon={<FileTextGlyph size="sm" />}
        count={total ?? undefined}
        // WHAT "RECENT" COVERS when the read is a window: the engine orders a
        // listing by title, so the newest of a capped read is the newest of
        // the pages it returned rather than of the container.
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
          {rest > 0 && (
            <a className="t-link t-caption" href={href(["knowledge", container.key])}>
              {plural(rest, "more page")} in this container →
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
function ContainerWriters({
  recent,
  total,
  capped,
  index,
}: {
  recent: PageSummary[];
  total: number | null;
  capped: boolean;
  index: OrgIndex;
}) {
  // THE NAME AND THE KIND, from one lookup: a writer's chip draws the dashed
  // ring off the kind, so resolving only the name makes every human writer
  // look like an agent.
  const who = useMemo(() => seatLookup(index), [index]);
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
        icon={<UsersGlyph size="sm" />}
        count={writers.length}
        // WHAT THE LIST COVERS, when it is not the whole container: the
        // creators of the pages this read returned, which past one window is
        // a slice by title rather than every writer here.
        subtitle={
          capped && total !== null
            ? `creators of the first ${recent.length} by title`
            : "creators, not editors"
        }
      >
        <Card.Title>Who writes here</Card.Title>
      </Card.Header>
      {writers.length > 0 ? (
        <div className="row wrap gap-2">
          {writers.slice(0, PEEK_WRITERS).map((handle) => (
            <SeatChip key={handle} handle={handle} {...who(handle)} />
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
