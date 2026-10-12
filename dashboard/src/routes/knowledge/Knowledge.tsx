/**
 * Knowledge: the company's own pages, opening on what changed.
 *
 * # Home
 *
 * The approved Knowledge layout leads with the spaces in one row at the top
 * (`SpacesRow`), then what is worth returning to: the pages a person starred
 * (Pinned), the ones they read last (Continue reading), the ones they follow
 * (Watching), and every save, comment and new page across the company
 * (Recently written). The search sits in the page bar of every Knowledge
 * screen (`KnowledgeSearchBar`); with a phrase in the address this screen is
 * the RANKED ANSWER instead, grouped by space beside a preview of the chosen
 * result.
 *
 * # What a search here reads, the native truth
 *
 * On the engine's own knowledge base a search reads THIS NODE'S OWN COPY of
 * the pages: the replicated rows every node applies from the pages log, the
 * lexical index this node keeps over them, and the replicated vectors, the
 * same fan-out a seat's `search_knowledge` runs. It is as current as this
 * node's place on that log. On Confluence the search IS live, at query time,
 * against the site.
 *
 * # Three modes, and what was served
 *
 * `mode=hybrid|keyword|semantic`, Hybrid, Keyword and Meaning to a reader. The
 * answer says what it actually ranked by (`served_mode`) and why that differs
 * from what was asked (`degraded`), and a partial fan-out names the part it
 * did not cover (`coverage`). Each is a sentence above the hits, because each
 * is a fact about every one of them.
 */

import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import {
  Button,
  Callout,
  Card,
  Document,
  DocumentTitle,
  EmptyState,
  Highlight,
  KeyChip,
  List,
  ListItem,
  Menu,
  SegmentedControl,
  Skeleton,
  highlightTerms,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  BookOpenGlyph,
  BrainGlyph,
  ClockGlyph,
  ExternalLinkGlyph,
  PlusGlyph,
  SearchGlyph,
  WandSparklesGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useOwnStar } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateCompact, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { plainText, renderMarkdown } from "~/lib/markdown.ts";
import { useRecents } from "~/lib/recents.ts";
import { modeLabel, resolveSearchMode, searchCoverageNote, servedNote } from "~/lib/search.ts";
import { clockIn, companyMidnight } from "~/lib/range.ts";
import { useStarred } from "~/lib/starred.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { KnowledgeAnswer, KnowledgeHit, PageChange } from "~/protocol/index.ts";
import {
  Face,
  KnowledgeBar,
  NewPageButton,
  PageGlyph,
  changeNote,
  changeWords,
  PageTags,
  SpacesRow,
  drawnBy,
  pagePath,
  usePeople,
  useSpaces,
  type Who,
} from "./parts.tsx";
import { withoutRepeatedTitle } from "./page/body.ts";

/** How often the landing's lists are read again. */
const LANDING_POLL_MS = 60_000;

/** How many rows a landing list draws: a glance, not a history. */
const PINNED_MAX = 6;
const READING_MAX = 4;
const WATCHING_MAX = 5;
const WRITTEN_MAX = 8;

/** A page id off a starred or recent route, or "" for anything else. */
function pageOf(path: readonly string[]): string {
  return path.length === 3 && path[0] === "knowledge" && path[1] === "pages" ? (path[2] ?? "") : "";
}

export function Knowledge() {
  const [q] = useParam("q", "");
  const phrase = q.trim();
  // NO STAR IN THIS BAR: a page is starred from its own More menu, and the
  // frame's would offer the workspace the sidebar already lists.
  useOwnStar();
  return (
    <>
      <KnowledgeBar>
        <NewPageButton />
      </KnowledgeBar>
      {phrase ? <KnowledgeResults phrase={phrase} /> : <KnowledgeLanding />}
    </>
  );
}

// ---------------------------------------------------------------------------
// The landing
// ---------------------------------------------------------------------------

function KnowledgeLanding() {
  const probe = useSearchQuery("knowledge", { q: "", mode: "semantic" });
  const { spaces, loading } = useSpaces();
  if (probe.data?.available === false) return <Unavailable answer={probe.data} />;
  if (probe.data && probe.data.backend !== "native" && probe.data.backend !== "") {
    // A VENDOR WIKI: nothing on this node to browse, by design.
    return (
      <EmptyState
        icon={<SearchGlyph size="xl" />}
        title="Search your wiki from the search box"
        description={`This company's pages live in ${probe.data.backend}, so there is nothing on this engine to browse. The search reaches the wiki live, as the engine's own account.`}
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
          description="A space is created the first time somebody writes into it. Give a unit a space, and its agents have somewhere to file what they learn."
        />
      ) : (
        <div className="knowledge-home-grid">
          <div className="knowledge-home-column">
            <Pinned />
            <RecentlyWritten />
          </div>
          <div className="knowledge-home-column">
            <ContinueReading />
            <Watching />
            <ForTheAgents />
          </div>
        </div>
      )}
    </div>
  );
}

/** A landing section: its name, a quiet line saying what it is, and its rows. */
function LandingSection({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: string | undefined;
  children: React.ReactNode;
}) {
  const id = `knowledge-${title.toLowerCase().replace(/\W+/g, "-")}`;
  return (
    <section className="knowledge-section" aria-labelledby={id}>
      <h2 className="knowledge-section-title" id={id}>
        {title}
        {hint && <span className="knowledge-section-hint">{hint}</span>}
      </h2>
      {children}
    </section>
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
        .slice(0, PINNED_MAX),
    [stars],
  );
  return (
    <LandingSection title="Pinned" hint="your starred pages">
      {pinned.length === 0 ? (
        <p className="knowledge-note">Star a page from its More menu to pin it here.</p>
      ) : (
        <ul className="knowledge-pins" role="list">
          {pinned.map((star) => (
            <li key={pageOf(star.path)}>
              <PinnedCard id={pageOf(star.path)} label={star.label} />
            </li>
          ))}
        </ul>
      )}
    </LandingSection>
  );
}

/** One pinned page: its space, its title, and who last saved it, when. */
function PinnedCard({ id, label }: { id: string; label: string }) {
  const who = usePeople();
  const now = useNow();
  const page = useQuery("page", { id }, { pollMs: LANDING_POLL_MS });
  const detail = page.data;
  const read = detail?.page;
  const last = detail?.history?.[0];
  const by = last ? drawnBy(last.author, last.author_seat) : "";
  return (
    <Card href={href(pagePath(id))} padding="sm" className="knowledge-pin">
      <span className="knowledge-pin-head">
        <PageGlyph folder={detail?.folder} />
        {read && <KeyChip>{read.container}</KeyChip>}
        {read && <PageTags status={read.status} onboarding={detail?.onboarding} />}
      </span>
      <span className="knowledge-pin-title">{read?.title ?? label}</span>
      {page.error === "not_found" ? (
        <span className="knowledge-row-meta">This page is gone: unstar it from the sidebar.</span>
      ) : (
        last && (
          <span className="knowledge-row-meta">
            {by && <Face handle={by} who={who} />}
            <span className="truncate">
              {by ? who(by).name : "The engine"} ·{" "}
              <time dateTime={last.created_at} title={fmtDateTime(last.created_at)}>
                {relTime(last.created_at, now)}
              </time>
            </span>
          </span>
        )
      )}
    </Card>
  );
}

/** The pages this person opened last, from this browser's recents. */
function ContinueReading() {
  const recents = useRecents();
  const reading = useMemo(
    () => recents.filter((recent) => pageOf(recent.path) !== "").slice(0, READING_MAX),
    [recents],
  );
  return (
    <LandingSection title="Continue reading">
      {reading.length === 0 ? (
        <p className="knowledge-note">The pages you open are listed here.</p>
      ) : (
        <Card padding="none">
          <List variant="divided">
            {reading.map((recent) => (
              <ListItem key={pageOf(recent.path)} href={href(recent.path)} leading={<PageGlyph />}>
                <span className="truncate">{recent.label}</span>
              </ListItem>
            ))}
          </List>
        </Card>
      )}
    </LandingSection>
  );
}

/** The pages this person follows: they are told when these change. */
function Watching() {
  const viewer = useViewer();
  const now = useNow();
  const watched = useQuery(
    "pages",
    { watcher: viewer.handle, status: "published,draft", skills: false, limit: WATCHING_MAX },
    { enabled: viewer.handle !== "", pollMs: LANDING_POLL_MS },
  );
  const rows = watched.data?.pages ?? [];
  return (
    <LandingSection title="Watching" hint="you are told when these change">
      {viewer.handle === "" ? (
        <p className="knowledge-note">
          Pages you follow are listed here once your token is bound to your seat.
        </p>
      ) : rows.length === 0 && !watched.loading ? (
        <p className="knowledge-note">Watch a page with the eye in its bar to follow it.</p>
      ) : (
        <Card padding="none">
          <List variant="divided">
            {rows.map((page) => (
              <ListItem
                key={page.id}
                href={href(pagePath(page.id))}
                leading={<PageGlyph folder={page.folder} />}
                trailing={
                  <span className="knowledge-row-trail">
                    <KeyChip>{page.container}</KeyChip>
                    <time dateTime={page.updated_at} title={fmtDateTime(page.updated_at)}>
                      {fmtDateCompact(page.updated_at, now)}
                    </time>
                  </span>
                }
              >
                <span className="truncate">{page.title}</span>
              </ListItem>
            ))}
          </List>
        </Card>
      )}
    </LandingSection>
  );
}

/** The kinds a "recently written" row is about: writing, not housekeeping. */
const WRITTEN_KINDS = "created,saved,renamed,comment";

/**
 * The COMPANY's day a change fell on, as a heading over its rows: the day the
 * engine counts a day in, so a change at 23:00 company time is under the same
 * day on this screen as on every other.
 */
function dayOf(at: string, now: number, zone: string | undefined): string {
  const when = Date.parse(at);
  const today = companyMidnight(now, zone);
  if (when >= today) return "Today";
  if (when >= companyMidnight(today - 1, zone)) return "Yesterday";
  // NOON OF THE COMPANY'S DAY, drawn as a date: the day's own midnight can
  // read as the day before in the reader's zone.
  const noon = companyMidnight(when, zone) + 12 * 3_600_000;
  return fmtDateCompact(new Date(noon).toISOString(), now);
}

/** Every save, comment and new page across the company, newest first. */
function RecentlyWritten() {
  const who = usePeople();
  const now = useNow();
  const zone = useOrg()?.timezone;
  const feed = useQuery(
    "page_activity",
    { kinds: WRITTEN_KINDS, limit: WRITTEN_MAX },
    { pollMs: LANDING_POLL_MS },
  );
  const changes = feed.data?.changes ?? [];
  const days = useMemo(() => {
    const out: { day: string; rows: PageChange[] }[] = [];
    for (const change of changes) {
      const day = dayOf(change.at, now, zone);
      const last = out[out.length - 1];
      if (last && last.day === day) last.rows.push(change);
      else out.push({ day, rows: [change] });
    }
    return out;
  }, [changes, now, zone]);
  return (
    <LandingSection title="Recently written" hint="every save, comment and new page">
      <QueryState
        error={feed.error}
        loading={feed.loading && !feed.data}
        empty={
          changes.length
            ? undefined
            : {
                title: "Nothing has been written yet",
                hint: "Every save, comment and new page is listed here as it lands.",
              }
        }
      >
        <Card padding="none">
          {days.map(({ day, rows }) => (
            <section key={day} aria-label={day}>
              <h3 className="knowledge-day">{day}</h3>
              <ol className="knowledge-written-rows">
                {rows.map((change) => (
                  <WrittenRow key={change.id} change={change} who={who} now={now} zone={zone} />
                ))}
              </ol>
            </section>
          ))}
          <Card.Footer variant="meta">
            From each space&rsquo;s change log.{" "}
            <a className="prose-link" href={href(["settings", "audit"])}>
              Every change
            </a>
          </Card.Footer>
        </Card>
      </QueryState>
    </LandingSection>
  );
}

/**
 * One change: who did what to which page, its note, and WHEN ON THE COMPANY'S
 * CLOCK, the clock its day heading is cut on. An age read against the heading
 * above it disagreed with it ("1d ago" under "Oct 09"); the age is the
 * time's title.
 */
function WrittenRow({
  change,
  who,
  now,
  zone,
}: {
  change: PageChange;
  who: Who;
  now: number;
  zone: string | undefined;
}) {
  const by = drawnBy(change.actor, change.actor_seat);
  const note = changeNote(change);
  return (
    <li className="knowledge-written-row">
      <Face handle={by} who={who} size="sm" />
      <p className="knowledge-written-text">
        <b>{by ? who(by).name : "The engine"}</b> {changeWords(change.kind).verb}{" "}
        {change.title ? (
          <a className="prose-link" href={href(pagePath(change.page_id))}>
            {change.title}
          </a>
        ) : (
          <span>a page that is gone</span>
        )}{" "}
        {change.container && <KeyChip>{change.container}</KeyChip>}
        {note && <span className="knowledge-written-note">{note}</span>}
      </p>
      <time dateTime={change.at} title={`${relTime(change.at, now)}, ${fmtDateTime(change.at)}`}>
        {clockIn(tsKey(change.at), zone)}
      </time>
    </li>
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
  const memory = useQuery("memory_overview", undefined, { pollMs: LANDING_POLL_MS });
  const seats = memory.data?.seats ?? [];
  // THE NEWEST THING AN AGENT KEPT: the last reflection any seat wrote.
  const newest = seats
    .map((seat) => seat.last_reflection_at)
    .filter(Boolean)
    .sort()
    .pop();
  return (
    <LandingSection title="For the agents">
      <Card padding="none">
        <List variant="divided">
          <ListItem
            href={href(["knowledge", "skills"])}
            leading={<WandSparklesGlyph size="sm" />}
            trailing={
              typeof skills.data?.total === "number" ? (
                <span>{plural(skills.data.total, "tool skill")}</span>
              ) : undefined
            }
          >
            Agent skills
          </ListItem>
          <ListItem
            href={href(["knowledge", "diaries"])}
            leading={<BrainGlyph size="sm" />}
            trailing={
              memory.data ? (
                <span>
                  {plural(seats.length, "agent")}
                  {newest ? ` · newest ${relTime(newest, now)}` : ""}
                </span>
              ) : undefined
            }
          >
            Agent diaries
          </ListItem>
        </List>
      </Card>
    </LandingSection>
  );
}

/**
 * The search DID NOT RUN, and why: chosen off `reason`, never off the prose
 * `note` or an empty `backend` (which means "no backend" and "no company"
 * alike, and the remedies differ).
 */
export function Unavailable({ answer }: { answer: KnowledgeAnswer }) {
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
            The backend itself is fine. Add the spaces to search to{" "}
            <code className="inline">knowledge.scope</code>.
          </>
        )}
        {/* NOT A MISCONFIGURATION: this node joined recently and is still
            indexing what it holds. Nothing is lost and nothing needs fixing. */}
        {building && (
          <>
            {" "}
            Nothing is wrong: this node joined recently and is still indexing. Pages that exist are
            not findable from here yet; try again in a moment.
          </>
        )}
      </span>
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
  const nav = useNavigator();

  if (search.tooLong) return <Callout variant="warning">{search.tooLong}</Callout>;
  if (data?.available === false) return <Unavailable answer={data} />;
  if ((search.loading || !settled) && !data) {
    return <Skeleton variant="text" rows={6} label="Searching" />;
  }
  const served = servedNote(data);
  const partial = searchCoverageNote(data);
  const ran = Boolean(data?.served_mode);
  const native = data?.backend === "native";

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
                  <code className="inline">{n.id}</code>
                  {n.error ? `: ${n.error}` : ""}
                </li>
              ))}
            </ul>
          )}
        </Callout>
      )}
      <QueryState error={search.error} detail={search.detail} loading={search.loading && !data}>
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
                options={[
                  { value: "space", label: "By space" },
                  { value: "rank", label: "Ranked" },
                ]}
                value={grouping === "rank" ? "rank" : "space"}
                onValueChange={(value) => setGrouping(value === "space" ? "space" : value)}
              />
            </div>
            <div className="knowledge-results-filters">
              {space ? (
                <Button
                  size="small"
                  variant="secondary"
                  trailingIcon={<XGlyph size="sm" />}
                  onClick={() => setSpace("")}
                  aria-label={`Remove the filter: space ${names.get(space) ?? space}`}
                >
                  Space: {names.get(space) ?? space}
                </Button>
              ) : (
                <Menu
                  label="Filter the results by space"
                  trigger={
                    <>
                      <PlusGlyph size="sm" />
                      Filter
                    </>
                  }
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
                    ? "Nothing in this space was among the results. Remove the filter to see the rest."
                    : native
                      ? `Ranked by ${modeLabel(data?.served_mode ?? "")} over this node's copy of the pages. Another mode may find what these words did not.`
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
              <kbd>j</kbd> <kbd>k</kbd> move, <kbd>Enter</kbd> opens.{" "}
              {native
                ? "Searched on this node's own copy of the pages, the same search an agent runs."
                : "Searched live on the wiki at query time, as the engine's own account."}
            </p>
          </section>
          <section className="knowledge-results-preview" aria-label="Preview">
            {selected ? (
              <ResultPreview
                hit={selected}
                terms={terms}
                onOpen={() => nav.to(pagePath(selected.id))}
              />
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
  // A VENDOR HIT carries the wiki's own link, and opens it; a native hit is
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
function ResultPreview({
  hit,
  terms,
  onOpen,
}: {
  hit: KnowledgeHit;
  terms: readonly string[];
  onOpen: () => void;
}) {
  const who = usePeople();
  const now = useNow();
  const page = useQuery("page", { id: hit.id }, { enabled: !hit.url });
  const reads = useQuery("page_reads", { page: hit.id }, { enabled: !hit.url });
  const detail = page.data;
  if (hit.url) {
    return (
      <Callout variant="neutral" icon={<ExternalLinkGlyph size="md" />}>
        This result lives in the wiki.{" "}
        <a className="prose-link" href={hit.url} target="_blank" rel="noreferrer">
          Open it there
        </a>
        .
      </Callout>
    );
  }
  if (page.loading && !detail) return <Skeleton variant="text" rows={8} label="Reading the page" />;
  if (!detail?.page) return <QueryState error={page.error} loading={false} />;
  const last = detail.history?.[0];
  const author = drawnBy(detail.page.author, detail.author_seat);
  const saver = last ? drawnBy(last.author, last.author_seat) : author;
  const body = withoutRepeatedTitle(detail.page.body ?? "", detail.page.title);
  return (
    <article className="knowledge-preview" aria-label={detail.page.title}>
      <div className="knowledge-preview-head">
        <PageGlyph folder={detail.folder} />
        <KeyChip>{detail.page.container}</KeyChip>
        <PageTags status={detail.page.status} skill={detail.skill} onboarding={detail.onboarding} />
        <span className="spacer" />
        <Button
          size="small"
          variant="secondary"
          leadingIcon={<ArrowUpRightGlyph size="sm" />}
          onClick={onOpen}
        >
          Open page
        </Button>
      </div>
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
