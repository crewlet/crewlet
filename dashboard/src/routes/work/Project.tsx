/**
 * One project: its own facts, and three lenses over its work.
 *
 * # A project is an object, so it has a header rather than a dashboard
 *
 * The screen used to open with the same four-number strip and stacked census
 * card the company-wide list did, above the same board — so the first screenful
 * of a project was an overview nobody asked for, and the work started below the
 * fold. What a container can say that its rows cannot is who leads it, which
 * unit owns it, and how far along it is: five facts and one meter, in the
 * header every other object in this product wears.
 *
 * # Three lenses, and each is a different question
 *
 * ITEMS is the work, which is why it is the default and why it is the same
 * component the company-wide list is. OVERVIEW is what the CONTAINER is — its
 * vocabulary, its tags, its census, the findings on its own record — which a
 * board can say none of. HISTORY is what has happened, ordered by the log
 * rather than by anything the rows sort on.
 *
 * A LENS IS A SECTION, so each pushes history: a reader who walked Items →
 * Overview → History and pressed Back three times walks out through them.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes.
 */

import { useMemo } from "react";
import { href } from "~/app/router.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { usePageCoverage, usePageLabels } from "~/app/Shell.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
// THE HEADER'S FACT IS NOT THE TRACKER'S. `components/work.tsx` exports a
// `Fact` that DRAWS one in an overview strip; this one is the VALUE an
// [ObjectHeader] renders. Both are right and neither may be renamed for the
// other's benefit, so the type is aliased where it is imported.
import { ObjectHeader, type Fact as HeaderFact } from "~/app/frame/ObjectHeader.tsx";
import { NumberCell } from "~/app/frame/cells.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Coverage, type RowChrome } from "~/components/work.tsx";
import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  Legend,
  Skeleton,
  StackedBar,
  Tabs,
  Tag,
  DATA_COLOR_OTHER,
} from "@crewlethq/ui";
import { DashboardGlyph, TimelineGlyph, TuneGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { pageCount, pageNote, statusLabel, STATUSES, typeName } from "~/lib/work.ts";
import { describeChange } from "~/lib/work.ts";
import { ItemsView } from "./ItemsView.tsx";
import { HistoryView } from "./History.tsx";
import { FEED_PAGE } from "./feed.tsx";
import { statusDot } from "./shapes/group.tsx";
import type { WorkGroup, WorkProjectDetail, WorkTaskCounts } from "~/protocol/index.ts";

/** The lenses, in the order the strip draws them; the first is the default. */
const LENSES = ["items", "overview", "history"] as const;

export function Project({ projectKey }: { projectKey: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [lens, setLens] = useTab("lens", LENSES);

  const state = useQuery("work_project", { key: projectKey }, { pollMs: 60_000 });
  const detail = state.data;
  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: detail?.types,
    statuses: detail?.statuses,
  };

  // THE NAME THE HEADER DRAWS IS THE NAME THE TRAIL GETS, and it is one
  // string: a screen that resolved it twice would offer the palette a word the
  // page never showed.
  usePageLabels(detail?.name ? { [projectKey]: detail.name } : {});
  usePageCoverage(detail);

  if (state.error === "not_found") return <NoSuchProject projectKey={projectKey} />;

  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["work"])}>
          All work →
        </a>
      </PageActions>

      {state.loading && !detail && <Skeleton variant="text" rows={4} label="Loading the project" />}
      <QueryState error={state.error} loading={state.loading}>
        {detail && (
          <div className="work-main">
            <ProjectBanners detail={detail} />
            <ObjectHeader
              kind="Project"
              icon="view_column"
              identifier={detail.key}
              title={detail.name}
              status={detail.archived ? <Tag appearance="outline">archived</Tag> : undefined}
              facts={projectFacts(detail, chrome)}
            />
            {/* THE CENSUS AS A SHAPE, under the header rather than in a card of
                its own above the work. The three numbers in the fact line say
                how much; the bar says the proportion, which is the fact a
                reader actually wants — "mostly finished" against "mostly
                ahead". It is a SIBLING rather than a sixth fact because a bar
                inside `.fact-value` is a bar in a truncated inline box, which
                is no bar at all.

                A BAR OF NOTHING IS NOT A CENSUS: three zero segments draw an
                empty track that reads as a chart which failed to load rather
                than as a project nobody has filed anything in. */}
            {total(detail.task_counts) > 0 && (
              <div className="work-census">
                <ProjectCensus counts={detail.task_counts} />
              </div>
            )}
            {detail.purpose && <p className="t-body measure">{detail.purpose}</p>}

            <Tabs
              ariaLabel="Lens"
              value={lens}
              onValueChange={(value) => setLens(value as (typeof LENSES)[number])}
              items={[
                { value: "items", label: "Items" },
                { value: "overview", label: "Overview" },
                { value: "history", label: "History" },
              ]}
            />

            {lens === "items" && <ItemsView project={projectKey} />}
            {lens === "overview" && <ProjectOverview detail={detail} chrome={chrome} />}
            {lens === "history" && <HistoryView container={`project:${projectKey}`} embedded />}
          </div>
        )}
      </QueryState>
    </>
  );
}

/** How much work a project holds at all, which decides whether a bar means anything. */
function total(counts: WorkTaskCounts): number {
  return counts.open + counts.done + counts.closed;
}

/**
 * What the CONTAINER is, as against what is in it.
 *
 * Its vocabulary — the statuses, the types and the fields it declares — its
 * labels, and how its open work is distributed. Every one of these is a fact a
 * board cannot state from its rows, and each was either buried in a rail or
 * reachable nowhere at all: a company that renamed four statuses and declared
 * six fields had no screen that said so.
 */
function ProjectOverview({ detail, chrome }: { detail: WorkProjectDetail; chrome: RowChrome }) {
  // `group_limit: 1` because only the COUNTS are drawn here and the engine runs
  // one paged statement per column — a column's rows are exactly what this
  // panel does not show, and zero is not a bound the grammar accepts.
  const census = useQuery(
    "work_items",
    { container: `project:${detail.key}`, group_by: "status", group_limit: 1 },
    { pollMs: 60_000 },
  );

  return (
    <div className="col gap-4">
      <Card>
        <Card.Header icon={<DashboardGlyph size="sm" />}>
          <Card.Title>Where the open work stands</Card.Title>
        </Card.Header>
        {/* A FAILED BREAKDOWN IS NOT AN EMPTY ONE. This is the only read that
            can say how the open work is distributed, so a refusal rendered as
            no rows would read as a project whose every task is in one status. */}
        <QueryState error={census.error} loading={census.loading}>
          {census.data && (
            <div className="col">
              {statusCounts(census.data.groups ?? []).map((line) => (
                <div className="work-col-head" key={line.status}>
                  <i className={statusDot(line.status)} aria-hidden="true" />
                  <span className="truncate">{statusLabel(line.status, detail.statuses)}</span>
                  <span className="count-chip">{line.count}</span>
                </div>
              ))}
              <span className="t-caption">
                Open work only — the {detail.task_counts.done} done and {detail.task_counts.closed}{" "}
                closed are in the census above.
              </span>
            </div>
          )}
        </QueryState>
      </Card>

      <Card>
        <Card.Header icon={<TuneGlyph size="sm" />}>
          <Card.Title>What this project calls things</Card.Title>
        </Card.Header>
        <div className="col gap-3">
          <div className="col gap-1">
            <span className="t-label">Statuses</span>
            <div className="row gap-2 wrap">
              {(detail.statuses ?? []).map((s) => (
                <Tag key={s.status} appearance="outline" title={s.description || undefined}>
                  {s.label}
                </Tag>
              ))}
            </div>
          </div>
          <div className="col gap-1">
            <span className="t-label">Types</span>
            <div className="row gap-2 wrap">
              {(detail.types ?? [])
                .filter((t) => !t.archived)
                .map((t) => (
                  <Tag key={t.slug} appearance="outline">
                    {typeName(t.slug, detail.types)}
                  </Tag>
                ))}
            </div>
          </div>
          <div className="col gap-1">
            <span className="t-label">Labels</span>
            <div className="row gap-2 wrap">
              {(detail.tags ?? []).filter((t) => !t.archived).length === 0 ? (
                <EmptyValue label="This project declares no labels" />
              ) : (
                (detail.tags ?? [])
                  .filter((t) => !t.archived)
                  .map((t) => (
                    <Tag key={t.slug} appearance="outline">
                      {t.label}
                    </Tag>
                  ))
              )}
            </div>
          </div>
          <div className="col gap-1">
            <span className="t-label">Fields</span>
            <div className="row gap-2 wrap">
              {(detail.fields ?? []).flatMap((g) => g.fields).filter((f) => !f.archived).length ===
              0 ? (
                <EmptyValue label="This project declares no fields of its own" />
              ) : (
                (detail.fields ?? [])
                  .flatMap((g) => g.fields)
                  .filter((f) => !f.archived)
                  .map((f) => (
                    <Tag key={f.id} appearance="outline" title={f.description || undefined}>
                      {f.name}
                    </Tag>
                  ))
              )}
            </div>
          </div>
          {/* THE STATE A FIELD IN THE MIDDLE OF A MOVE IS IN, which is a
              finding rather than a decoration: a workspace field this project
              redeclares is one whose values are read from two places until the
              move finishes. */}
          {detail.shadowed?.length ? (
            <Callout variant="info">
              This project redeclares {detail.shadowed.length} workspace field
              {detail.shadowed.length === 1 ? "" : "s"}, so its own declaration is what its tasks
              are read against.
            </Callout>
          ) : null}
        </div>
      </Card>
      <ProjectFeed detail={detail} chrome={chrome} />
    </div>
  );
}

/** The last few changes, as a companion to the container's own facts. */
function ProjectFeed({ detail, chrome }: { detail: WorkProjectDetail; chrome: RowChrome }) {
  const now = useNow();
  const feed = useQuery(
    "work_activity",
    { container: `project:${detail.key}`, limit: FEED_PAGE.rail },
    { pollMs: 60_000 },
  );
  const records = feed.data?.records ?? [];
  return (
    <Card>
      <Card.Header
        icon={<TimelineGlyph size="sm" />}
        count={feed.data ? pageCount(records.length, !!feed.data.next_cursor) : undefined}
        subtitle={pageNote(records.length, !!feed.data?.next_cursor, "change") || undefined}
      >
        <Card.Title>Latest changes</Card.Title>
      </Card.Header>
      <QueryState error={feed.error} loading={feed.loading}>
        {feed.data &&
          (records.length > 0 ? (
            <div className="col gap-2">
              {records.map((record) => (
                <div className="col gap-1" key={record.id}>
                  <div className="row gap-2">
                    {record.subject_key ? (
                      <a className="mono t-link" href={href(["work", record.subject_key])}>
                        {record.subject_key}
                      </a>
                    ) : (
                      <EmptyValue label="No work item" />
                    )}
                    <span className="spacer" />
                    <span className="t-caption" title={fmtDateTime(record.at)}>
                      {relTime(record.at, now)}
                    </span>
                  </div>
                  <span className="t-caption truncate">
                    {describeChange(record, chrome)} · {record.actor || "the engine"}
                  </span>
                </div>
              ))}
              <a className="t-link" href={href(["work", "history"], { project: detail.key })}>
                Every change →
              </a>
            </div>
          ) : (
            <span className="muted">Nothing has changed in this project yet.</span>
          ))}
      </QueryState>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// The rail
// ---------------------------------------------------------------------------

/**
 * One project, beside the list it was opened from.
 *
 * # What it answers that the row could not
 *
 * The directory ranks projects and says how much work each holds — a meter,
 * three numbers and a name. That is enough to CHOOSE a project and nothing
 * like enough to RECOGNISE one: who leads it, which unit owns it and whether
 * anything has happened this week are facts about the CONTAINER. So the header
 * is the same facts the project's own page wears — from [projectFacts], so the
 * two cannot drift — and under it the two questions a reader opens a project
 * for: where the work stands, and what changed.
 */
export function ProjectPeek({ projectKey }: { projectKey: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  // NAMED FOR WHAT IT IS, not for the slot it arrives in. The rail hands every
  // peek an opaque `id` because the frame knows nothing about any kind, and a
  // project's is its KEY — the string a unit declares and a person types.
  const state = useQuery(
    "work_project",
    { key: projectKey },
    { enabled: projectKey !== "", pollMs: 60_000 },
  );
  const detail = state.data;
  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: detail?.types,
    statuses: detail?.statuses,
  };

  // NOT THE GENERIC "there is no such record" that `QueryState` draws for a
  // `not_found`. A project key reaches this from a pasted URL and from a
  // bookmark as often as from a row, and WHICH key resolved to nothing is
  // precisely the half the generic banner drops.
  if (state.error === "not_found") return <NoSuchProject projectKey={projectKey} />;

  return (
    <>
      {state.loading && !state.data && (
        <Skeleton variant="text" rows={6} label="Loading the project" />
      )}
      <QueryState error={state.error} loading={state.loading}>
        {detail && (
          <>
            <ObjectHeader
              size="peek"
              kind="Project"
              icon="view_column"
              identifier={detail.key}
              title={detail.name}
              status={detail.archived ? <Tag appearance="outline">archived</Tag> : undefined}
              facts={projectFacts(detail, chrome)}
            />
            <div className="col gap-3">
              <Coverage answer={detail} />
              <ProjectBanners detail={detail} />
              {/* WHY THIS PROJECT EXISTS, drawn whenever somebody wrote it:
                  "is this the one I meant" is the question the rail answers,
                  and a purpose is the sentence that answers it. */}
              {detail.purpose && <p className="t-body measure">{detail.purpose}</p>}
              {total(detail.task_counts) > 0 ? (
                <ProjectCensus counts={detail.task_counts} />
              ) : (
                <span className="muted">No work has been filed in this project yet.</span>
              )}
              <ProjectFeed detail={detail} chrome={chrome} />
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

/**
 * NOT AN EMPTY RAIL, and not a spinner that never resolves.
 *
 * A project key that resolves to nothing is almost always a key that MOVED or
 * was never real — the engine's own refusal names the nearest ones — so the
 * honest answer prints the key that failed and says what its absence means.
 */
function NoSuchProject({ projectKey }: { projectKey: string }) {
  return (
    <EmptyState
      size="compact"
      icon={<DashboardGlyph size="xl" />}
      title={`No project called “${projectKey}”`}
      description="A project key is declared by a unit in the company configuration. Either it never existed, or the unit that declared it has since been renamed — the directory is what this company actually has."
    />
  );
}

/**
 * The five facts a project is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader scans
 * the same facts in the same order wherever the object appears, and a header
 * written twice is two orders as soon as somebody adds a sixth.
 */
function projectFacts(detail: WorkProjectDetail, chrome?: RowChrome): HeaderFact[] {
  return [
    {
      label: "Lead",
      // A HANDLE IS THE DATABASE'S WORD FOR A PERSON. Every other surface in
      // the tree resolves it through the chart and shows the name somebody is
      // actually called; this one printed the slug.
      value: detail.lead.handle ? (
        <SeatChip
          name={chrome?.seatName?.(detail.lead.handle) ?? detail.lead.handle}
          handle={detail.lead.handle}
        />
      ) : (
        <span className="muted">nobody</span>
      ),
    },
    {
      label: "Unit",
      value: detail.unit.name || detail.unit.key || <span className="muted">none</span>,
    },
    // THE THREE COUNTS ARE CELLS, not bare numbers: a project with nothing open
    // is the one an operator is most often looking for here, and a zero
    // rendered as a blank or as a dash is the one reading that hides it.
    { label: "Open", value: <NumberCell value={detail.task_counts.open} /> },
    { label: "Done", value: <NumberCell value={detail.task_counts.done} /> },
    { label: "Closed", value: <NumberCell value={detail.task_counts.closed} /> },
  ];
}

/** The one finding a project's own record can carry, on the page and in the rail. */
function ProjectBanners({ detail }: { detail: WorkProjectDetail }) {
  return (
    <>
      {/* A UNIT THE CHART NO LONGER HAS is a finding, not a blank: it is what
          leaves a project's work routed to nobody. */}
      {!detail.unit.resolved && (
        <Callout variant="warning">
          This project names the unit <span className="mono">{detail.unit.key}</span>, which the
          current org chart does not have — work filed here routes to nobody.
        </Callout>
      )}
    </>
  );
}

/**
 * A project's census, drawn once.
 *
 * IN THE STATUS TONES the badges use rather than the chart hues, so the same
 * fact is not two colours on one screen — and NEVER WITHOUT ITS LEGEND, since
 * an unlabelled stack of three colours is three colours.
 */
export function ProjectCensus({ counts }: { counts: WorkTaskCounts }) {
  // AN `id` PER SEGMENT, which is what both of their components key on — and it
  // is the status word rather than the position, so a census that gains a
  // fourth group later does not renumber the three that were there.
  const segments = [
    { id: "open", label: "Open", value: counts.open, color: "var(--info)" },
    { id: "done", label: "Done", value: counts.done, color: "var(--positive)" },
    { id: "closed", label: "Closed", value: counts.closed, color: DATA_COLOR_OTHER },
  ];
  return (
    <>
      <StackedBar segments={segments} />
      <Legend items={segments.map(({ id, label, color }) => ({ id, label, color }))} />
    </>
  );
}

/**
 * How the open work is distributed, as lines in the board's own order.
 *
 * THE CANONICAL ORDER FIRST, so the panel does not reshuffle between polls as a
 * status empties, and A STATUS WITH NO WORK IS A ZERO rather than an absence:
 * the query asked over the whole project, so a status nothing matched is a
 * count of none — not a fact nobody recorded.
 *
 * AND ANYTHING THIS BUILD HAS NOT HEARD OF is appended rather than dropped. The
 * wire evolves additively and a newer node may name a status this build does
 * not know; a column silently missing from a census is the one shape a reader
 * cannot notice.
 */
function statusCounts(groups: WorkGroup[]): { status: string; count: number }[] {
  const counted = new Map(groups.map((group) => [group.key, group.count]));
  const lines = OPEN_STATUSES.map((status) => ({ status, count: counted.get(status) ?? 0 }));
  for (const group of groups) {
    if (!OPEN_STATUSES.includes(group.key)) {
      lines.push({ status: group.key, count: group.count });
    }
  }
  return lines;
}

/** The statuses a task can be in while it is still somebody's problem. */
const OPEN_STATUSES: string[] = STATUSES.filter(
  (status) => status.group === "not_started" || status.group === "active",
).map((status) => status.value);
