/**
 * Doclet: the company's own pages, opening on what changed.
 *
 * # Home
 *
 * The approved Doclet layout leads with the spaces in one row at the top
 * (`SpacesRow`), then TILES SIZED BY WHAT EACH IS WORTH, on one screen: the
 * newest change across the company in the big tile, open to read (the page,
 * what each save said, the lines the newest save changed and the page's
 * opening); every other save, comment and new page down the right (Recently
 * written); and four small tiles, for the pages a person read last (Continue
 * reading), starred (Pinned) and follows (Watching), and the agents' own two
 * places. The search sits in the page bar of every Doclet screen
 * (`DocletSearchBar`); with a phrase in the address this screen is the
 * RANKED ANSWER instead, grouped by space beside a preview of the chosen
 * result.
 *
 * # What a search here reads, Doclet's truth
 *
 * On Doclet a search reads THIS NODE'S OWN COPY of the pages: the replicated
 * rows every data node applies from the pages log, the lexical index this
 * node keeps over them, and the replicated vectors, the same fan-out a seat's
 * `search_knowledge` runs, divided across the live data nodes by bucket. It is
 * as current as this node's place on that log. On Confluence the search IS
 * live, at query time, against the site.
 *
 * # Three modes, and what was served
 *
 * `mode=hybrid|keyword|semantic`, Hybrid, Keyword and Meaning to a reader. The
 * answer says what it actually ranked by (`served_mode`) and why that differs
 * from what was asked (`degraded`), and a partial fan-out names the part it
 * did not cover (`coverage`). Each is a sentence above the hits, because each
 * is a fact about every one of them.
 */

import { useEffect, useMemo, useState, type KeyboardEvent, type ReactNode } from "react";
import {
  ButtonLink,
  Callout,
  Card,
  Document,
  DocumentTitle,
  EmptyState,
  FilterPill,
  Highlight,
  KeyChip,
  List,
  ListItem,
  Menu,
  SegmentedControl,
  Skeleton,
  highlightTerms,
  InlineCode,
  firstChangedLines,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  BookOpenGlyph,
  BotGlyph,
  BrainGlyph,
  ClockGlyph,
  ExternalLinkGlyph,
  EyeGlyph,
  ListGlyph,
  PinGlyph,
  PlusGlyph,
  SearchGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useAssistantContext } from "~/app/assistant/useAssistantContext.ts";
import { choiceKey, urlAction } from "~/app/assistant/urlAction.ts";
import { SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { useOwnStar } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { DiffView } from "~/components/DiffView.tsx";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateCompact, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { compareLines, notComparedReason } from "~/lib/lineDiff.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { plainText } from "~/lib/markdownText.ts";
import { useRecents } from "~/lib/recents.ts";
import {
  SEARCH_MODES,
  modeLabel,
  resolveSearchMode,
  searchCoverageNote,
  servedNote,
} from "~/lib/search.ts";
import { clockIn } from "~/lib/range.ts";
import { useStarred } from "~/lib/starred.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { DOCLET_SEARCH_BACKEND, useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN, DOCLET, accessNames, needsWords } from "~/lib/access.ts";
import type {
  KnowledgeAnswer,
  KnowledgeHit,
  LogRefusal,
  PageChange,
  PageDetail,
  PageStatus,
  QueryRefusal,
} from "~/protocol/index.ts";
import {
  changeAuthor,
  Face,
  DocletBar,
  NewPageButton,
  PageGlyph,
  changeNote,
  changedBy,
  PageTags,
  SpacesRow,
  pagePath,
  usePeople,
  useSpaces,
  type Who,
} from "./parts.tsx";
import { byDay, dayOf, foldRuns, leadOf, runPhrase, saveRevisions, type Run } from "./landing.ts";
import { openingOf, withoutRepeatedTitle } from "./page/body.ts";

/** How often the landing's tiles are read again. */
const LANDING_POLL_MS = 60_000;

/**
 * How many changes the landing reads: the newest leads the big tile and the
 * rest run down Recently written. Twelve fills that tall tile beside the
 * others at 1440 by 900 under its day headings, a glance rather than a
 * history; Every change is one press away.
 */
const WRITTEN_READ = 12;

/**
 * How many pages a small tile lists: Continue reading, Pinned and Watching.
 * Three, so the whole landing fits one screen at 1440 by 900 beside the
 * newest change. The sidebar's Starred group lists every starred page, and
 * the palette every page opened lately.
 */
const TILE_ROWS = 3;

/**
 * How much of the newest change the big tile draws: the notes of its saves,
 * the lines its newest save changed, and the page's first lines. Each is a
 * preview with the rest one press away (the page, and the save in its
 * History), so the tile stays on the screen beside the others.
 */
const LATEST_NOTES = 3;
const LATEST_DIFF_LINES = 6;
const LATEST_OPENING_LINES = 8;

/** The kinds the landing's changes are about: writing, not housekeeping. */
const WRITTEN_KINDS = "created,saved,renamed,comment";

/** A page id off a starred or recent route, or "" for anything else. */
function pageOf(path: readonly string[]): string {
  return path.length === 3 && path[0] === "doclet" && path[1] === "pages" ? (path[2] ?? "") : "";
}

/** How the results are shown, as the control offers it. */
const RESULT_GROUPINGS = [
  { value: "space", label: "By space" },
  { value: "rank", label: "Ranked" },
];

const encoder = new TextEncoder();

export function Doclet() {
  const [q] = useParam("q", "");
  const phrase = q.trim();
  // --- assistant page actions (WP12) ---
  // THE SEARCH THE BOX, THE MODE PICKER, THE GROUPING AND THE SPACE FILTER
  // WRITE, held to the bound the engine refuses a longer phrase at rather
  // than cut, and to the spaces this company has.
  const { spaces } = useSpaces();
  const nav = useNavigator();
  useAssistantContext({
    actions: urlAction("knowledge.search.set", {
      scope: "What Doclet searches for and how it shows it (view only)",
      keys: {
        q: {
          label: "Search",
          history: "filter",
          check: (v) =>
            encoder.encode(v).length <= SEARCH_QUERY_MAX ? null : "That search is too long.",
        },
        mode: choiceKey("Mode", SEARCH_MODES, "filter"),
        by: choiceKey("Show the results", RESULT_GROUPINGS, "filter", "space"),
        space: choiceKey(
          "Space",
          spaces.map((c) => ({ value: c.key, label: c.name || c.key })),
          "filter",
        ),
      },
      nav,
    }),
  });
  // --- end assistant page actions (WP12) ---
  // NO STAR IN THIS BAR: a page is starred from its own More menu, and the
  // frame's would offer the workspace the sidebar already lists.
  useOwnStar();
  return (
    <>
      <DocletBar>
        <NewPageButton />
      </DocletBar>
      {phrase ? <KnowledgeResults phrase={phrase} /> : <DocletLanding />}
    </>
  );
}

// ---------------------------------------------------------------------------
// The landing
// ---------------------------------------------------------------------------

function DocletLanding() {
  const probe = useSearchQuery("knowledge", { q: "", mode: "semantic" });
  const { spaces, loading } = useSpaces();
  if (probe.data?.available === false) return <Unavailable answer={probe.data} />;
  if (probe.data && probe.data.backend !== DOCLET_SEARCH_BACKEND && probe.data.backend !== "") {
    // A VENDOR WIKI: nothing on this node to browse, by design. Confluence is
    // the one wiki `knowledge.backend` admits beside the engine's own.
    return (
      <EmptyState
        icon={<SearchGlyph size="xl" />}
        title="Search your wiki from the search box"
        description="Pages live in Confluence. Search reaches them live."
      />
    );
  }
  return (
    <div className="knowledge-home">
      <SpacesRow />
      {!loading && spaces.length === 0 ? (
        <EmptyState
          icon={<BookOpenGlyph size="xl" />}
          title="Nothing has been written down yet"
          description="Give a team a space, and its agents file what they learn there."
        />
      ) : (
        <Tiles />
      )}
    </div>
  );
}

/** What both tiles drawn from the one read of the feed know about that read. */
interface FeedState {
  error: QueryErrorCode | null;
  /** Why the read was refused, beside `error`: what `QueryState` names. */
  refusal: QueryRefusal | LogRefusal | null;
  /** No answer has arrived yet. */
  loading: boolean;
}

/**
 * The landing's tiles, each sized by what it is worth: the newest change open
 * to read, Recently written down the right, and four small tiles for the pages
 * a person keeps and the agents' own two places, each one line when empty.
 *
 * ONE READ OF THE FEED feeds the first two, so the change the big tile leads
 * with is never listed again beside it. The grid places each tile by a named
 * area and the markup keeps the reading order, so a keyboard and a screen
 * reader meet the tiles in the order a reader scans them, at every width.
 */
function Tiles() {
  const zone = useOrg()?.timezone;
  const feed = useQuery(
    "page_activity",
    { kinds: WRITTEN_KINDS, limit: WRITTEN_READ },
    { pollMs: LANDING_POLL_MS },
  );
  const runs = useMemo(() => foldRuns(feed.data?.changes ?? [], zone), [feed.data, zone]);
  const lead = leadOf(runs);
  const rest = useMemo(() => runs.filter((_, i) => i !== lead), [runs, lead]);
  const state: FeedState = {
    error: feed.error,
    refusal: feed.refusal,
    loading: feed.loading && !feed.data,
  };
  // NOTHING ELSE WRITTEN IS SAID ONCE, by the newest change's tile: a second
  // tile three rows tall holding "Nothing has been written yet" (or "Nothing
  // else", beside the one change the big tile leads with) said it again in a
  // 440px box. The grid then has no Recently written column
  // (`data-written="none"`, screens.css).
  const nothing = !state.loading && !state.error && rest.length === 0;
  return (
    <div className="knowledge-tiles" data-written={nothing ? "none" : undefined}>
      <Latest run={runs[lead]} state={state} />
      {!nothing && <RecentlyWritten runs={rest} state={state} />}
      <ContinueReading />
      <Pinned />
      <Watching />
      <ForTheAgents />
    </div>
  );
}

/**
 * THE NEWEST CHANGE, OPEN TO READ: the page, who changed it and when, what
 * each save said, the lines the newest save changed and the page's opening.
 *
 * ONE PAGE READ, of this page alone, and the revision before the save for its
 * diff. Every other tile names pages; this is the one that reads what a page
 * says.
 */
function Latest({ run, state }: { run: Run | undefined; state: FeedState }) {
  const id = run?.[0].page_id ?? "";
  const page = useQuery("page", { id }, { enabled: id !== "", pollMs: LANDING_POLL_MS });
  // THIS PAGE'S ANSWER, never the last one: a newer change moves the lead to
  // another page, and the old page's answer stays on screen until the new
  // page's arrives.
  const detail = page.data?.page?.id === id ? page.data : null;
  return (
    // THE SIBLING TILES' INSET: at the large step its words stood 8px in from
    // every other tile's in the same grid.
    <Card as="article" padding="md" className="knowledge-latest" aria-label="Newest change">
      {state.loading ? (
        <Skeleton variant="text" rows={6} label="Reading the newest change" />
      ) : (
        <QueryState
          error={state.error}
          refusal={state.refusal}
          loading={false}
          empty={
            run
              ? undefined
              : {
                  title: "Nothing has been written yet",
                  hint: "New saves, comments and pages appear here.",
                }
          }
        >
          {run && <LatestChange run={run} detail={detail} />}
        </QueryState>
      )}
    </Card>
  );
}

function LatestChange({ run, detail }: { run: Run; detail: PageDetail | null }) {
  const who = usePeople();
  const now = useNow();
  const zone = useOrg()?.timezone;
  const change = run[0];
  const by = changedBy(change);
  return (
    <>
      <PageHead
        id={change.page_id}
        container={detail?.page.container ?? change.container}
        folder={detail?.folder}
        status={detail?.page.status}
        skill={detail?.skill}
        onboarding={detail?.onboarding}
      />
      <DocumentTitle as="h2">{detail?.page.title ?? change.title}</DocumentTitle>
      <p className="knowledge-latest-who">
        <Face handle={by} who={who} />
        <span>
          <b>{changeAuthor(change, who)}</b> {runPhrase(run)}
        </span>
        <span className="spacer" />
        <time dateTime={change.at} title={`${relTime(change.at, now)}, ${fmtDateTime(change.at)}`}>
          {dayOf(change.at, now, zone)} {clockIn(tsKey(change.at), zone)}
        </time>
      </p>
      <LatestNotes run={run} zone={zone} />
      {detail && <SaveDiff change={change} detail={detail} />}
      {detail && <Opening detail={detail} />}
    </>
  );
}

/**
 * What each change of the newest row said, newest first, on the clock its day
 * is cut on: a save's note, a comment's words. A new page and a rename have
 * nothing to add here, since the page's own first lines are drawn below.
 */
function LatestNotes({ run, zone }: { run: Run; zone: string | undefined }) {
  const kind = run[0].kind;
  if (kind !== "saved" && kind !== "comment") return null;
  const shown = run.slice(0, LATEST_NOTES);
  const more = run.length - shown.length;
  return (
    <>
      <ol className="knowledge-latest-notes">
        {shown.map((change) => {
          const note = changeNote(change);
          return (
            <li key={change.id} className="knowledge-latest-note">
              <time dateTime={change.at} title={fmtDateTime(change.at)}>
                {clockIn(tsKey(change.at), zone)}
              </time>
              {note ? (
                <span>{kind === "comment" ? `“${note}”` : note}</span>
              ) : (
                <span className="knowledge-note">No note</span>
              )}
            </li>
          );
        })}
      </ol>
      {more > 0 && <p className="knowledge-note">{plural(more, "earlier save")} that day</p>}
    </>
  );
}

/**
 * The lines the newest save changed, drawn as the page's History draws them
 * and cut to a preview, with the count of the rest and the way to the whole
 * save. Nothing for a save this node no longer holds the revisions of, or one
 * that left the body as it was.
 *
 * A SAVE THE LINE DIFF DID NOT COMPARE is said to be one, with the way to it,
 * as History says it: a preview of a comparison that was not made would list
 * the page's first lines as removed by a save that never touched them.
 */
function SaveDiff({ change, detail }: { change: PageChange; detail: PageDetail }) {
  const id = detail.page.id;
  const revs = useMemo(() => saveRevisions(change, detail.history ?? []), [change, detail.history]);
  // THE BODY THE SAVE MADE is the page's own while no later save has landed,
  // and a read of that revision otherwise.
  const made = useQuery(
    "page_revision",
    { page: id, version: revs?.made.version ?? 0 },
    { enabled: revs !== null && !revs.current },
  );
  const before = useQuery(
    "page_revision",
    { page: id, version: revs?.before.version ?? 0 },
    { enabled: revs !== null },
  );
  // AN ANSWER FOR THESE VERSIONS, never the last pair's: a newer save to the
  // same page moves both, and a read keeps its old answer until the new one
  // lands, so for a moment the two halves of a diff belonged to two saves.
  const after = revs?.current
    ? (detail.page.body ?? "")
    : made.data?.version === revs?.made.version
      ? made.data?.body
      : undefined;
  const prior = before.data?.version === revs?.before.version ? before.data?.body : undefined;
  const compared = useMemo(
    () => (after === undefined || prior === undefined ? null : compareLines(prior, after, 0)),
    [prior, after],
  );
  const preview = useMemo(
    () =>
      compared?.kind === "compared"
        ? firstChangedLines(compared.hunks, LATEST_DIFF_LINES)
        : { sections: [], more: 0 },
    [compared],
  );
  if (!revs || !compared) return null;
  const save = (
    <a className="prose-link" href={href(pagePath(id), { version: String(revs.made.version) })}>
      this save
    </a>
  );
  if (compared.kind !== "compared") {
    return (
      <p className="knowledge-note">
        The save was not compared line by line: {notComparedReason(compared)}. Open {save} to read
        it.
      </p>
    );
  }
  if (preview.sections.length === 0) return null;
  return (
    <>
      <DiffView hunks={preview.sections} aria-label="Latest change" />
      {preview.more > 0 && (
        <p className="knowledge-note">
          {plural(preview.more, "more changed line")} in {save}
        </p>
      )}
    </>
  );
}

/** The page's opening: what it says before it is opened, its title said once. */
function Opening({ detail }: { detail: PageDetail }) {
  const opening = useMemo(
    () => openingOf(detail.page.body ?? "", detail.page.title, LATEST_OPENING_LINES).trim(),
    [detail.page.body, detail.page.title],
  );
  if (!opening) return null;
  return (
    <Document size="sm" measure="none" className="knowledge-latest-opening">
      {renderMarkdown(opening)}
    </Document>
  );
}

/**
 * Every save, comment and new page across the company after the one the big
 * tile leads with, under the COMPANY's days. Each row says its time on the
 * clock its day is cut on, and the note it carries, never the title again.
 */
function RecentlyWritten({ runs, state }: { runs: Run[]; state: FeedState }) {
  const who = usePeople();
  const now = useNow();
  const zone = useOrg()?.timezone;
  const days = useMemo(() => byDay(runs, now, zone), [runs, now, zone]);
  return (
    <Tile
      className="knowledge-written"
      title="Recently written"
      icon={<ListGlyph size="sm" />}
      action={
        <a className="t-link" href={href(["settings", "audit"], { kind: "doclet" })}>
          Every change
        </a>
      }
    >
      {state.loading ? (
        <TileBody>
          <Skeleton variant="text" rows={6} label="Reading what was written" />
        </TileBody>
      ) : state.error ? (
        <TileBody>
          <QueryState error={state.error} refusal={state.refusal} loading={false} />
        </TileBody>
      ) : days.length === 0 ? (
        <TileNote>Nothing has been written yet.</TileNote>
      ) : (
        days.map(({ day, runs: rows }) => (
          <section key={day} aria-label={day}>
            <h3 className="knowledge-day">{day}</h3>
            <TileRows>
              {rows.map((run) => (
                <WrittenRow key={run[0].id} run={run} who={who} now={now} zone={zone} />
              ))}
            </TileRows>
          </section>
        ))
      )}
    </Tile>
  );
}

/**
 * One row of Recently written: the page, who did what to it, the note it
 * carries, and WHEN ON THE COMPANY'S CLOCK, the clock its day heading is cut
 * on. An age read against the heading above it disagreed with it ("1d ago"
 * under "Oct 09"); the age is the time's title.
 */
function WrittenRow({
  run,
  who,
  now,
  zone,
}: {
  run: Run;
  who: Who;
  now: number;
  zone: string | undefined;
}) {
  const change = run[0];
  const by = changedBy(change);
  const note = changeNote(change);
  return (
    <ListItem
      // A PURGED PAGE keeps its changes and no title: the record that it
      // existed, with nowhere to go.
      href={change.title ? href(pagePath(change.page_id)) : undefined}
      leading={<Face handle={by} who={who} />}
      trailing={
        <time
          className="knowledge-tile-meta"
          dateTime={change.at}
          title={`${relTime(change.at, now)}, ${fmtDateTime(change.at)}`}
        >
          {clockIn(tsKey(change.at), zone)}
        </time>
      }
    >
      <span className="knowledge-tile-text">
        <span className="truncate">{change.title || "A page that is gone"}</span>
        <span className="knowledge-tile-meta truncate">
          {changeAuthor(change, who)} {runPhrase(run)}
        </span>
        {note && (
          <span className="knowledge-written-note clamp">
            {change.kind === "comment" ? `“${note}”` : note}
          </span>
        )}
      </span>
    </ListItem>
  );
}

/** The pages this browser opened last, from its recents. */
function ContinueReading() {
  const recents = useRecents();
  const reading = useMemo(
    () => recents.filter((recent) => pageOf(recent.path) !== "").slice(0, TILE_ROWS),
    [recents],
  );
  return (
    <Tile className="knowledge-reading" title="Continue reading" icon={<ClockGlyph size="sm" />}>
      {reading.length === 0 ? (
        <TileNote>The pages you open are listed here.</TileNote>
      ) : (
        <TileRows>
          {reading.map((recent) => (
            <KeptPage
              key={pageOf(recent.path)}
              id={pageOf(recent.path)}
              label={recent.label}
              gone="This page is gone"
            />
          ))}
        </TileRows>
      )}
    </Tile>
  );
}

/** The pages this person starred from a page's More menu, newest star first. */
function Pinned() {
  const stars = useStarred();
  const pinned = useMemo(
    () =>
      [...stars]
        .filter((star) => pageOf(star.path) !== "")
        .sort((a, b) => b.at - a.at)
        .slice(0, TILE_ROWS),
    [stars],
  );
  return (
    <Tile className="knowledge-pinned" title="Pinned" icon={<PinGlyph size="sm" />}>
      {pinned.length === 0 ? (
        <TileNote>Star a page to pin it here.</TileNote>
      ) : (
        <TileRows>
          {pinned.map((star) => (
            <KeptPage
              key={pageOf(star.path)}
              id={pageOf(star.path)}
              label={star.label}
              gone="This page is gone: unstar it from the sidebar."
            />
          ))}
        </TileRows>
      )}
    </Tile>
  );
}

/**
 * The pages this person follows: they are told when these change.
 *
 * ASKED BY THE SEAT THEY ACT AS (`ViewerState.handle`), which is the one name
 * a watch is filed under: a wake reaches a seat, so a credential the directory
 * binds to no seat subscribes nobody (`internal/doclet` `Actor.subscriber`) and
 * has no pages to list here.
 */
function Watching() {
  const viewer = useViewer();
  const who = usePeople();
  const now = useNow();
  const watched = useQuery(
    "pages",
    { watcher: viewer.handle, status: "published,draft", skills: false, limit: TILE_ROWS },
    { enabled: viewer.handle !== "", pollMs: LANDING_POLL_MS },
  );
  const rows = watched.data?.pages ?? [];
  return (
    <Tile className="knowledge-watching" title="Watching" icon={<EyeGlyph size="sm" />}>
      {viewer.unbound ? (
        <TileNote>
          A watch is kept under a seat. This credential has none, so it follows no page.
        </TileNote>
      ) : viewer.handle === "" ? (
        <TileNote>Sign in to see pages you follow.</TileNote>
      ) : watched.error ? (
        <TileBody>
          <QueryState error={watched.error} refusal={watched.refusal} loading={false} />
        </TileBody>
      ) : rows.length === 0 ? (
        !watched.loading && <TileNote>Watch a page to follow it.</TileNote>
      ) : (
        <TileRows>
          {rows.map((page) => (
            <PageRow
              key={page.id}
              id={page.id}
              folder={page.folder}
              title={page.title}
              meta={lastSaved(page.saved_at, page.saved_by ?? "", who, now)}
            />
          ))}
        </TileRows>
      )}
    </Tile>
  );
}

/** The two places a person goes to see what the agents use and keep. */
function ForTheAgents() {
  const now = useNow();
  const skills = useQuery(
    "pages",
    { skills: true, status: "published", limit: 1 },
    { pollMs: LANDING_POLL_MS },
  );
  // THE AGENTS' DIARIES ARE THEIR TRAIL, and Diaries is an admin's screen:
  // the overview is asked only of an admin, and the row says what opens it to
  // anybody else rather than counting the few seats a lead would be answered.
  const viewer = useViewer();
  const memory = useQuery("memory_overview", undefined, {
    pollMs: LANDING_POLL_MS,
    enabled: viewer.admin,
  });
  const seats = memory.data?.seats ?? [];
  // THE NEWEST THING AN AGENT KEPT: the last reflection any seat wrote.
  const newest = seats
    .map((seat) => seat.last_reflection_at)
    .filter(Boolean)
    .sort()
    .pop();
  return (
    <Tile className="knowledge-agents" title="For the agents" icon={<BotGlyph size="sm" />}>
      <TileRows>
        <ListItem
          href={href(["doclet", "skills"])}
          leading={<WandSparklesGlyph size="sm" />}
          trailing={
            typeof skills.data?.total === "number" ? (
              <span className="knowledge-tile-meta">{plural(skills.data.total, "tool skill")}</span>
            ) : undefined
          }
        >
          Agent skills
        </ListItem>
        <ListItem
          href={href(["doclet", "diaries"])}
          leading={<BrainGlyph size="sm" />}
          trailing={
            !viewer.loading && !viewer.admin ? (
              <span className="knowledge-tile-meta">{`Needs ${accessNames([ADMIN])} access`}</span>
            ) : memory.data ? (
              <span className="knowledge-tile-meta">
                {plural(seats.length, "agent")}
                {newest ? ` · newest ${relTime(newest, now)}` : ""}
              </span>
            ) : undefined
          }
        >
          Agent diaries
        </ListItem>
      </TileRows>
    </Tile>
  );
}

/**
 * A small tile: a card named by its own heading, its glyph before the name and
 * its one link at the end of the head. `className` names its area of the
 * landing's grid.
 */
function Tile({
  className,
  title,
  icon,
  action,
  children,
}: {
  className: string;
  title: string;
  icon: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card as="section" padding="none" className={className}>
      <Card.Header divided={false} icon={icon} actions={action}>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      {children}
    </Card>
  );
}

/** A tile's rows, set in so each row's glyph sits under the tile's own. */
function TileRows({ children }: { children: ReactNode }) {
  return (
    <div className="knowledge-tile-rows">
      <List variant="plain">{children}</List>
    </div>
  );
}

/** Anything else a tile holds, at the tile's own inset. */
function TileBody({ children }: { children: ReactNode }) {
  return <div className="knowledge-tile-body">{children}</div>;
}

/** What a tile says when it has nothing to list: one line, so an empty tile costs one. */
function TileNote({ children }: { children: ReactNode }) {
  return (
    <TileBody>
      <p className="knowledge-note">{children}</p>
    </TileBody>
  );
}

/**
 * A page somebody keeps: its title as the page holds it now, and who last
 * saved what it says. READ PER ROW, as a pinned card always was, because a
 * star and a recent carry only the label the page had when it was kept; a
 * renamed page is drawn by its new title and a gone one says so.
 */
function KeptPage({ id, label, gone }: { id: string; label: string; gone: string }) {
  const who = usePeople();
  const now = useNow();
  const page = useQuery("page", { id }, { pollMs: LANDING_POLL_MS });
  const detail = page.data?.page ? page.data : null;
  const last = detail?.history?.[0];
  return (
    <PageRow
      id={id}
      folder={detail?.folder}
      title={detail?.page.title ?? label}
      meta={
        page.error === "not_found"
          ? gone
          : lastSaved(last?.created_at, last?.author ?? "", who, now)
      }
    />
  );
}

/** One page in a small tile: its glyph, its title, and one line about it. */
function PageRow({
  id,
  folder,
  title,
  meta,
}: {
  id: string;
  folder: boolean | undefined;
  title: string;
  meta: string;
}) {
  return (
    <ListItem href={href(pagePath(id))} leading={<PageGlyph folder={folder} />}>
      <span className="knowledge-tile-text">
        <span className="truncate">{title}</span>
        {/* A LINE EITHER WAY, so a row does not grow when its read lands. */}
        <span className="knowledge-tile-meta truncate">{meta || " "}</span>
      </span>
    </ListItem>
  );
}

/**
 * "Last saved 2h ago by Crewlet-SWE": the newest change to what a page says,
 * or "" where nothing says when. A listing's zero instant is not a save.
 */
function lastSaved(at: string | undefined, by: string, who: Who, now: number): string {
  if (tsKey(at) <= 0) return "";
  return `Last saved ${relTime(at, now)} by ${by ? who(by).name : "the engine"}`;
}

/**
 * A page's head on a card that shows it before it is opened: its glyph, its
 * space, the words it wears, and the way in. One drawing for the newest
 * change and a search's preview, so the two show a page alike.
 */
function PageHead({
  id,
  container,
  folder,
  status,
  skill,
  onboarding,
}: {
  id: string;
  container: string | undefined;
  folder: boolean | undefined;
  status: PageStatus | undefined;
  skill: boolean | undefined;
  onboarding: boolean | undefined;
}) {
  return (
    <div className="knowledge-preview-head">
      <PageGlyph folder={folder} />
      {container && <KeyChip>{container}</KeyChip>}
      <PageTags status={status} skill={skill} onboarding={onboarding} />
      <span className="spacer" />
      <ButtonLink
        size="small"
        variant="secondary"
        leadingIcon={<ArrowUpRightGlyph size="sm" />}
        href={href(pagePath(id))}
      >
        Open page
      </ButtonLink>
    </div>
  );
}

/**
 * The search DID NOT RUN, and why: chosen off `reason`, never off the prose
 * `note` or an empty `backend` (which means "no backend" and "no company"
 * alike, and the remedies differ).
 */
export function Unavailable({ answer }: { answer: KnowledgeAnswer }) {
  const building = answer.reason === "building";
  if (answer.reason === "no_access") {
    // THE READER'S ACCESS, not the company's search: said in the words every
    // lock uses, with nothing to configure.
    return (
      <Callout variant="neutral" icon={<BookOpenGlyph size="md" />}>
        <span>{needsWords("Search", [DOCLET])}</span>
      </Callout>
    );
  }
  return (
    <Callout
      variant={building ? "warning" : "neutral"}
      icon={building ? <ClockGlyph size="md" /> : <BookOpenGlyph size="md" />}
    >
      {/* NOT A MISCONFIGURATION: this node joined recently and is still
          indexing what it holds. Nothing is lost and nothing needs fixing. */}
      {building ? (
        <span>Still indexing. Try again shortly.</span>
      ) : (
        <span>
          Search can&rsquo;t run: {answer.note || "the engine gave no reason"}.
          {answer.reason === "no_backend" && (
            <>
              {" "}
              Set <InlineCode>knowledge.backend</InlineCode> to <InlineCode>doclet</InlineCode> or{" "}
              <InlineCode>confluence</InlineCode>.
            </>
          )}
          {answer.reason === "no_scope" && (
            <>
              {" "}
              Add spaces to <InlineCode>knowledge.scope</InlineCode>.
            </>
          )}
        </span>
      )}
    </Callout>
  );
}

// ---------------------------------------------------------------------------
// The results
// ---------------------------------------------------------------------------

/**
 * A ranked answer, grouped by space beside a preview of the chosen result.
 *
 * THE RANK IS THE ENGINE'S, and grouping by space never reorders it inside a
 * space: a group is the hits of one space in the engine's order, and the
 * groups are ordered by their best hit. "Ranked" draws the one list as it came.
 */
function KnowledgeResults({ phrase }: { phrase: string }) {
  const [modeRaw] = useParam("mode", "");
  const [grouping, setGrouping] = useParam("by", "space");
  const [space, setSpace] = useParam("space", "");
  const probe = useSearchQuery("knowledge", { q: "", mode: "semantic" });
  const mode = resolveSearchMode(modeRaw || null, probe.data);
  // AN ADDRESS WITH NO MODE WAITS FOR THE PROBE rather than running hybrid
  // first: on a company with no embeddings provider that first answer is
  // "asked for Hybrid, served Keyword", a degradation of a choice nobody made.
  const settled = modeRaw !== "" || probe.data !== null || probe.error !== null;
  const search = useSearchQuery(
    "knowledge",
    { q: phrase, mode },
    { enabled: phrase !== "" && settled },
  );
  const { spaces } = useSpaces();
  const names = useMemo(() => new Map(spaces.map((s) => [s.key, s.name || s.key])), [spaces]);
  const data = search.data;
  const all = useMemo(() => data?.hits ?? [], [data]);
  // THE SPACE FILTER NARROWS WHAT IS SHOWN of the ranked answer; the engine's
  // search takes no space, so the ranks stay the ones it gave.
  const hits = useMemo(
    () =>
      all
        .map((hit, i) => ({ hit, rank: i + 1 }))
        .filter((h) => !space || h.hit.container === space),
    [all, space],
  );
  const groups = useMemo(() => {
    if (grouping !== "space") return [{ key: "", hits }];
    const out = new Map<string, typeof hits>();
    for (const h of hits) {
      const key = h.hit.container;
      out.set(key, [...(out.get(key) ?? []), h]);
    }
    return [...out.entries()].map(([key, rows]) => ({ key, hits: rows }));
  }, [hits, grouping]);
  const order = useMemo(() => groups.flatMap((g) => g.hits), [groups]);
  const [chosen, setChosen] = useState(0);
  useEffect(() => setChosen(0), [phrase, mode, space, grouping]);
  const selected = order[Math.min(chosen, Math.max(0, order.length - 1))]?.hit ?? null;
  const terms = useMemo(() => highlightTerms(phrase), [phrase]);

  if (search.tooLong) return <Callout variant="warning">{search.tooLong}</Callout>;
  if (data?.available === false) return <Unavailable answer={data} />;
  if ((search.loading || !settled) && !data) {
    return <Skeleton variant="text" rows={6} label="Searching" />;
  }
  const served = servedNote(data);
  const partial = searchCoverageNote(data);
  const ran = Boolean(data?.served_mode);
  const doclet = data?.backend === DOCLET_SEARCH_BACKEND;

  // j AND k MOVE THE CHOICE, Enter opens it: a ranked list is read by
  // comparing the top few, and the preview is where each one is read.
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (
      event.target instanceof HTMLInputElement ||
      event.metaKey ||
      event.ctrlKey ||
      event.altKey
    ) {
      return;
    }
    if (event.key === "j" || event.key === "k") {
      event.preventDefault();
      setChosen((at) => Math.max(0, Math.min(order.length - 1, at + (event.key === "j" ? 1 : -1))));
    }
  };

  return (
    <div className="knowledge-results">
      {/* WHAT WAS SERVED, when it is not what was asked; and WHAT WAS NOT
          COVERED, when the search is partial. */}
      {served && <Callout variant="info">{served}</Callout>}
      {partial && (
        <Callout variant="warning">
          {partial.sentence}
          {partial.nodes.length > 0 && (
            <ul className="coverage-nodes">
              {partial.nodes.map((n) => (
                <li key={n.id}>
                  <InlineCode>{n.id}</InlineCode>
                  {n.error ? `: ${n.error}` : ""}
                </li>
              ))}
            </ul>
          )}
        </Callout>
      )}
      <QueryState
        error={search.error}
        refusal={search.refusal}
        detail={search.detail}
        loading={search.loading && !data}
      >
        {/* The list and its preview share the keys: j and k move the choice
            through the list, and the preview follows it. */}
        <div className="knowledge-results-split" onKeyDown={onKeyDown}>
          <section className="knowledge-results-list" aria-label="Results">
            <div className="knowledge-results-head">
              <b>{plural(hits.length, "result")}</b>
              <span className="knowledge-results-for">
                for &ldquo;{phrase}&rdquo;
                {ran ? `, ranked by ${modeLabel(data?.served_mode ?? "")}` : ""}
              </span>
              <span className="spacer" />
              <SegmentedControl
                semantics="radio"
                size="sm"
                label="Show the results"
                options={RESULT_GROUPINGS}
                value={grouping === "rank" ? "rank" : "space"}
                onValueChange={(value) => setGrouping(value === "space" ? "space" : value)}
              />
            </div>
            <div className="knowledge-results-filters">
              {space ? (
                // THE KIT'S FILTER PILL, the one drawing every filter in force
                // takes, so this one reads as the Worklet's do.
                <FilterPill
                  field="Space"
                  operator="is"
                  value={names.get(space) ?? space}
                  onRemove={() => setSpace("")}
                />
              ) : (
                <Menu
                  label="Filter the results by space"
                  icon={<PlusGlyph size="sm" />}
                  trigger="Filter"
                  triggerVariant="ghost"
                  items={spaces.map((s) => ({
                    key: s.key,
                    label: s.name || s.key,
                    hint: <KeyChip>{s.key}</KeyChip>,
                    onSelect: () => setSpace(s.key),
                  }))}
                />
              )}
              {space && <span className="knowledge-note">of the ranked answer</span>}
            </div>
            {hits.length === 0 ? (
              <EmptyState
                size="compact"
                icon={<SearchGlyph size="xl" />}
                title={`Nothing matched “${phrase}”`}
                description={
                  space
                    ? "Remove the space filter to see the rest."
                    : doclet
                      ? "Try another search mode."
                      : "This is the wiki's own answer, taken just now."
                }
              />
            ) : (
              groups.map((group) => (
                <div key={group.key || "all"} className="knowledge-results-group">
                  {group.key && (
                    <h3 className="knowledge-results-space">
                      <b>{names.get(group.key) ?? group.key}</b>
                      <KeyChip>{group.key}</KeyChip>
                      <span>{plural(group.hits.length, "result")}</span>
                    </h3>
                  )}
                  <List variant="divided">
                    {group.hits.map(({ hit, rank }) => (
                      <ResultRow
                        key={hit.id}
                        hit={hit}
                        rank={rank}
                        terms={terms}
                        space={names.get(hit.container) ?? hit.container}
                        selected={selected?.id === hit.id}
                        onChoose={() => setChosen(order.findIndex((h) => h.hit.id === hit.id))}
                      />
                    ))}
                  </List>
                </div>
              ))
            )}
            <p className="knowledge-results-foot">
              <kbd>j</kbd> <kbd>k</kbd> move, <kbd>Enter</kbd> opens.
            </p>
          </section>
          <section className="knowledge-results-preview" aria-label="Preview">
            {selected ? (
              <ResultPreview hit={selected} terms={terms} />
            ) : (
              <p className="knowledge-note">Choose a result to read it here.</p>
            )}
          </section>
        </div>
      </QueryState>
    </div>
  );
}

/** One ranked hit: its rank, its title and snippet with the search marked, and where it lives. */
function ResultRow({
  hit,
  rank,
  terms,
  space,
  selected,
  onChoose,
}: {
  hit: KnowledgeHit;
  rank: number;
  terms: readonly string[];
  space: string;
  selected: boolean;
  onChoose: () => void;
}) {
  const now = useNow();
  const body = (
    <span className="knowledge-hit">
      <span className="knowledge-hit-head">
        <span className="knowledge-hit-rank" aria-label={`Rank ${rank}`}>
          {rank}
        </span>
        <Highlight className="knowledge-hit-title" text={hit.title} terms={terms} />
        {hit.updated_at && (
          <time
            className="knowledge-hit-when"
            dateTime={hit.updated_at}
            title={fmtDateTime(hit.updated_at)}
          >
            {fmtDateCompact(hit.updated_at, now)}
          </time>
        )}
      </span>
      <span className="knowledge-hit-path">{space}</span>
      {hit.snippet && (
        <Highlight className="knowledge-hit-snippet" text={plainText(hit.snippet)} terms={terms} />
      )}
    </span>
  );
  // A VENDOR HIT carries the wiki's own link, and opens it; a Doclet hit is
  // chosen here and read in the preview, its address one press away.
  if (hit.url) {
    return (
      <ListItem
        asChild
        trailing={<ExternalLinkGlyph size="sm" aria-label="Opens the wiki in a new tab" />}
      >
        <a className="knowledge-hit-link" href={hit.url} target="_blank" rel="noreferrer">
          {body}
        </a>
      </ListItem>
    );
  }
  return (
    <ListItem selected={selected} onClick={onChoose}>
      {body}
    </ListItem>
  );
}

/** The chosen result: what it is, and the page's opening with the search marked. */
function ResultPreview({ hit, terms }: { hit: KnowledgeHit; terms: readonly string[] }) {
  const who = usePeople();
  const now = useNow();
  const page = useQuery("page", { id: hit.id }, { enabled: !hit.url });
  const reads = useQuery("page_reads", { page: hit.id }, { enabled: !hit.url });
  const detail = page.data;
  if (hit.url) {
    return (
      <Callout variant="neutral" icon={<ExternalLinkGlyph size="md" />}>
        <a className="prose-link" href={hit.url} target="_blank" rel="noreferrer">
          Open this result in the wiki
        </a>
        .
      </Callout>
    );
  }
  if (page.loading && !detail) return <Skeleton variant="text" rows={8} label="Reading the page" />;
  if (!detail?.page)
    return <QueryState error={page.error} refusal={page.refusal} loading={false} />;
  const last = detail.history?.[0];
  const author = detail.page.author ?? "";
  const saver = last ? (last.author ?? "") : author;
  const body = withoutRepeatedTitle(detail.page.body ?? "", detail.page.title);
  return (
    <article className="knowledge-preview" aria-label={detail.page.title}>
      <PageHead
        id={detail.page.id}
        container={detail.page.container}
        folder={detail.folder}
        status={detail.page.status}
        skill={detail.skill}
        onboarding={detail.onboarding}
      />
      <DocumentTitle as="h2">{detail.page.title}</DocumentTitle>
      <dl className="knowledge-facts">
        <div>
          <dt>Author</dt>
          <dd>
            {author ? (
              <>
                <Face handle={author} who={who} />
                {who(author).name}
              </>
            ) : (
              "The engine"
            )}
          </dd>
        </div>
        <div>
          <dt>Last saved</dt>
          <dd>
            {last ? (
              <span title={`Revision ${last.version}`}>
                {relTime(last.created_at, now)} by {saver ? who(saver).name : "the engine"}
              </span>
            ) : (
              "Not saved over since it was written"
            )}
          </dd>
        </div>
        <div>
          <dt>Read by agents</dt>
          <dd>
            {reads.data?.readers
              ? reads.data.readers.length
                ? plural(reads.data.readers.length, "agent")
                : `None in ${plural(reads.data.days, "day")}`
              : reads.error === "unknown_query"
                ? "Not recorded on this node"
                : "Reading…"}
          </dd>
        </div>
        <div>
          <dt>Linked from</dt>
          <dd>
            {detail.linked_from
              ? detail.linked_from.tasks_total + detail.linked_from.pages_total
                ? `${plural(detail.linked_from.tasks_total, "task")}, ${plural(detail.linked_from.pages_total, "page")}`
                : "Nothing links here"
              : "Not known on this node"}
          </dd>
        </div>
      </dl>
      <Card padding="md">
        <Card.Header
          icon={<SearchGlyph size="sm" />}
          subtitle="the opening, with your search marked"
        >
          <Card.Title>The page</Card.Title>
        </Card.Header>
        {body.trim() ? (
          <Document size="sm" measure="none">
            {renderMarkdown(body, { highlight: terms })}
          </Document>
        ) : (
          <p className="knowledge-note">
            {detail.folder
              ? "A folder: it groups the pages filed under it."
              : "This page has no body."}
          </p>
        )}
      </Card>
    </article>
  );
}
