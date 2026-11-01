/**
 * Doclet's widgets: what was written lately, in the company or in one space,
 * the pages the reader follows, and what the agents use and keep.
 *
 * DOCLET'S OWN QUESTIONS AND ROWS. Each widget asks what the matching tile on
 * the Doclet landing asks (`page_activity`, `pages`, `memory_overview`) and
 * draws its rows with the landing's parts (`Face`, `PageGlyph`, the run
 * phrases), so a change reads the same on Home as in Doclet.
 */

import { useMemo, type ReactNode } from "react";
import { List, ListItem, Skeleton } from "@crewlethq/ui";
import {
  BrainGlyph,
  EyeGlyph,
  FolderGlyph,
  ListGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime, tsKey } from "~/lib/format.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { PAGE_LIST_ORDERS } from "~/contract/pages.ts";
import { uncounted } from "~/routes/doclet/Diaries.tsx";
import { foldRuns, runPhrase } from "~/routes/doclet/landing.ts";
import {
  changeAuthor,
  Face,
  PageGlyph,
  SPACES_POLL_MS,
  changedBy,
  pagePath,
  usePeople,
  useSpaces,
} from "~/routes/doclet/parts.tsx";
import {
  ChoiceNote,
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
  type WidgetProps,
} from "~/routes/home/board/kit.tsx";

/** What the landing counts as writing: a new page, a save, a rename, a comment. */
const WRITTEN_KINDS = "created,saved,renamed,comment";
/** Rows a widget lists before its Open link takes over. */
const ROWS = 5;

/** A list of rows at the landing tile's inset. */
function Rows({ children }: { children: ReactNode }) {
  return (
    <div className="knowledge-tile-rows">
      <List variant="plain">{children}</List>
    </div>
  );
}

/** The newest writing, across the company or in one space (`container`). */
function Written({ container }: { container?: string }) {
  const zone = useOrg()?.timezone;
  const who = usePeople();
  const now = useNow();
  const feed = useQuery(
    "page_activity",
    { kinds: WRITTEN_KINDS, limit: ROWS * 2, ...(container ? { container } : {}) },
    { pollMs: SPACES_POLL_MS },
  );
  const runs = useMemo(
    () => foldRuns(feed.data?.changes ?? [], zone).slice(0, ROWS),
    [feed.data, zone],
  );
  return (
    <WidgetQuery
      error={feed.error}
      refusal={feed.refusal}
      loading={feed.loading && !feed.data}
      gaps={gapsOf([feed.data])}
      empty={
        feed.data && runs.length === 0
          ? {
              title: "Nothing written yet",
              hint: container
                ? "Pages saved in this space are listed here."
                : "Pages saved anywhere in Doclet are listed here.",
            }
          : undefined
      }
    >
      <Rows>
        {runs.map((run) => {
          const change = run[0];
          const by = changedBy(change);
          return (
            <ListItem
              key={change.id}
              href={change.title ? href(pagePath(change.page_id)) : undefined}
              leading={<Face handle={by} who={who} />}
              trailing={
                <time className="knowledge-tile-meta" dateTime={change.at}>
                  {relTime(change.at, now)}
                </time>
              }
            >
              <span className="knowledge-tile-text">
                <span className="truncate">{change.title || "A page that is gone"}</span>
                <span className="knowledge-tile-meta truncate">
                  {changeAuthor(change, who)} {runPhrase(run)}
                </span>
              </span>
            </ListItem>
          );
        })}
      </Rows>
    </WidgetQuery>
  );
}

function RecentlyWritten() {
  return (
    <WidgetCard title="Recently written" from="doclet">
      <Written />
    </WidgetCard>
  );
}

function useSpaceChoices() {
  const { spaces, loading } = useSpaces();
  return {
    loading: loading && spaces.length === 0,
    empty: "No spaces yet. Create one in Doclet.",
    options: spaces.map((s) => ({
      value: s.key,
      label: s.name ? `${s.name} (${s.key})` : s.key,
    })),
  };
}

function SpaceActivity({ params }: WidgetProps) {
  const { spaces, read, error, refusal } = useSpaces();
  const space = spaces.find((s) => s.key === params.space);
  if (!space) {
    // A REFUSED LIST OF SPACES SAYS SO, as Saved view's does: waiting on an
    // answer that was refused drew a skeleton for good.
    return (
      <WidgetCard title="Space activity" from="doclet">
        {error ? (
          <WidgetQuery error={error} refusal={refusal} loading={false} gaps={[]} />
        ) : read ? (
          <ChoiceNote thing="space" />
        ) : (
          <div className="home-widget-pad">
            <Skeleton variant="text" rows={3} />
          </div>
        )}
      </WidgetCard>
    );
  }
  return (
    <WidgetCard
      title={space.name || space.key}
      from="doclet"
      actions={
        <a className="t-link home-widget-open" href={href(["doclet", space.key])}>
          Open space
        </a>
      }
    >
      <Written container={space.key} />
    </WidgetCard>
  );
}

function Watching() {
  const viewer = useViewer();
  const who = usePeople();
  const now = useNow();
  const watched = useQuery(
    "pages",
    { watcher: viewer.handle, status: "published,draft", skills: false, limit: ROWS },
    { enabled: viewer.handle !== "", pollMs: SPACES_POLL_MS },
  );
  const rows = watched.data?.pages ?? [];
  return (
    <WidgetCard title="Pages you watch" from="doclet">
      {viewer.unbound ? (
        <WidgetNote>This sign-in has no seat, so it watches no pages.</WidgetNote>
      ) : (
        <WidgetQuery
          error={watched.error}
          refusal={watched.refusal}
          loading={watched.loading && !watched.data}
          gaps={gapsOf([watched.data])}
          empty={
            watched.data && rows.length === 0
              ? { title: "No pages watched", hint: "Watch a page in Doclet to follow it." }
              : undefined
          }
        >
          <Rows>
            {rows.map((page) => (
              <ListItem
                key={page.id}
                href={href(pagePath(page.id))}
                leading={<PageGlyph folder={page.folder} />}
              >
                <span className="knowledge-tile-text">
                  <span className="truncate">{page.title}</span>
                  <span className="knowledge-tile-meta truncate">
                    {tsKey(page.saved_at) > 0
                      ? `Last saved ${relTime(page.saved_at, now)} by ${
                          page.saved_by ? who(page.saved_by).name : "the engine"
                        }`
                      : " "}
                  </span>
                </span>
              </ListItem>
            ))}
          </Rows>
        </WidgetQuery>
      )}
    </WidgetCard>
  );
}

/**
 * The newest published tool skills, as Agent skills lists them newest first:
 * the ENGINE's order, read from the skill rows' own index, a widget's window
 * of it. This asked for fifty in the knowledge base's own order and sorted
 * them here, so past fifty skills "newest" was the newest of the
 * alphabetically first fifty.
 */
const NEWEST_SKILLS = {
  skills: true,
  status: "published",
  order: "newest" satisfies (typeof PAGE_LIST_ORDERS)[number],
  limit: ROWS,
} as const;

function Skills() {
  const now = useNow();
  const listing = useQuery("pages", NEWEST_SKILLS, { pollMs: SPACES_POLL_MS });
  const total = listing.data?.total;
  const newest = listing.data?.pages ?? [];
  return (
    <WidgetCard title={total ? `Agent skills · ${total}` : "Agent skills"} from="doclet-skills">
      <WidgetQuery
        error={listing.error}
        refusal={listing.refusal}
        loading={listing.loading && !listing.data}
        gaps={gapsOf([listing.data])}
        empty={
          listing.data && newest.length === 0
            ? {
                title: "No skills yet",
                hint: "Skills appear once an agent's tools publish them.",
              }
            : undefined
        }
      >
        <Rows>
          {newest.map((page) => (
            <ListItem
              key={page.id}
              href={href(pagePath(page.id))}
              leading={<WandSparklesGlyph size="sm" />}
              trailing={
                <span className="knowledge-tile-meta">{relTime(page.updated_at, now)}</span>
              }
            >
              <span className="truncate">{page.title}</span>
            </ListItem>
          ))}
        </Rows>
      </WidgetQuery>
    </WidgetCard>
  );
}

function Diaries() {
  const now = useNow();
  const who = usePeople();
  const overview = useQuery("memory_overview", undefined, { pollMs: SPACES_POLL_MS });
  const seats = useMemo(
    () =>
      [...(overview.data?.seats ?? [])]
        .filter((s) => !uncounted(s) && s.last_reflection_at)
        .sort((a, b) => tsKey(b.last_reflection_at) - tsKey(a.last_reflection_at))
        .slice(0, ROWS),
    [overview.data],
  );
  const total = overview.data?.seats.length ?? 0;
  // A SEAT WHOSE HOLDER DID NOT ANSWER HAS NO COUNT, which is not a seat with
  // nothing written: it is said beside the list, as the Diaries page lists
  // it without a count, rather than dropped from it in silence.
  const unread = (overview.data?.seats ?? []).filter((s) => uncounted(s)).length;
  const gaps = [
    ...gapsOf([overview.data]),
    ...(unread === 1
      ? ["1 agent’s diary couldn’t be read, so it isn’t listed."]
      : unread > 1
        ? [`${unread} agents’ diaries couldn’t be read, so they aren’t listed.`]
        : []),
  ];
  return (
    <WidgetCard title="Agent diaries" from="doclet-diaries">
      <WidgetQuery
        error={overview.error}
        refusal={overview.refusal}
        loading={overview.loading && !overview.data}
        gaps={gaps}
        empty={
          overview.data && seats.length === 0
            ? {
                title: "Nothing written yet",
                hint: `${plural(total, "agent")} in the chart, none with a diary entry.`,
              }
            : undefined
        }
      >
        <Rows>
          {seats.map((s) => (
            <ListItem
              key={s.agent_id}
              href={href(["doclet", "diaries", s.handle])}
              leading={<Face handle={s.handle} who={who} />}
              trailing={
                <span className="knowledge-tile-meta">{relTime(s.last_reflection_at, now)}</span>
              }
            >
              <span className="knowledge-tile-text">
                <span className="truncate">{who(s.handle).name}</span>
                <span className="knowledge-tile-meta truncate">
                  {s.latest_reflection?.content ?? plural(s.diary_total, "entry", "entries")}
                </span>
              </span>
            </ListItem>
          ))}
        </Rows>
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "doclet.recent",
    from: "doclet",
    label: "Recently written",
    description: "The latest pages, saves and comments",
    icon: ListGlyph,
    hue: "cyan",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: RecentlyWritten,
  },
  {
    key: "doclet.space",
    from: "doclet",
    label: "Space activity",
    description: "The latest writing in one space",
    icon: FolderGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    params: [
      {
        key: "space",
        label: "Space",
        helper: "Its latest saves and comments are listed.",
        useChoices: useSpaceChoices,
      },
    ],
    Component: SpaceActivity,
  },
  {
    key: "doclet.watching",
    from: "doclet",
    label: "Pages you watch",
    description: "Pages you follow and when they changed",
    icon: EyeGlyph,
    hue: "blue",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Watching,
  },
  {
    key: "doclet.skills",
    from: "doclet-skills",
    label: "Agent skills",
    description: "The skills agents can use",
    icon: WandSparklesGlyph,
    hue: "purple",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Skills,
  },
  {
    key: "doclet.diaries",
    from: "doclet-diaries",
    label: "Agent diaries",
    description: "What each agent chose to remember",
    icon: BrainGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Diaries,
  },
]);
