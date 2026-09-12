/**
 * The work board — the company's own tracker.
 *
 * # Why this screen exists at all, and what it is not
 *
 * It is NOT a second tracker. A company running Jira has none of these
 * questions registered, and this screen says so rather than drawing an empty
 * board: an operator who wired Jira and then found a blank Crewlet board
 * would reasonably conclude their integration was broken.
 *
 * # An unhydrated projection is not an empty company
 *
 * Every row here comes from this node's own projection of the fleet's record,
 * which is the same copy a seat's tools read — so an operator and an agent
 * looking at one item see one item. A node that has not finished its boot
 * reconcile REFUSES rather than answering empty, and `QueryState` renders
 * that refusal as "still catching up". Drawing it as "no work" would be an
 * answer somebody acts on, by filing the duplicate.
 *
 * # Read-only, deliberately
 *
 * Nothing here writes. An item is filed and moved by a seat's own tools, or
 * by an operator through the MCP surface, and both are attributed to
 * somebody — where a board button would write as "the dashboard", which is
 * not a person and not a seat and cannot be asked why.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  Badge,
  Banner,
  Chip,
  Empty,
  Panel,
  SearchInput,
  Segmented,
  Skeleton,
  Stat,
  StatRow,
} from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type {
  WorkGroup,
  WorkProjectDetail,
  WorkProjectRow,
  WorkIncomplete,
  WorkStatus,
  WorkSummary,
  WorkView,
} from "~/protocol/index.ts";

/** The board's own vocabulary, rendered. A closed set, so a status the engine
 *  adds later shows as itself rather than vanishing from the filter. */
/** The tracker's SIX. `blocked` is NOT one: a blocker is data carried beside
 *  the status, because a task can be both in progress and blocked and a single
 *  field cannot say so — so it is a badge and a filter, never a column value.
 *  `cancelled` is here and reads as "finished without being delivered", which
 *  is the whole of what a close reason used to say. */
const STATUSES: { value: WorkStatus; label: string }[] = [
  { value: "todo", label: "To do" },
  { value: "in_progress", label: "In progress" },
  { value: "in_review", label: "In review" },
  { value: "done", label: "Done" },
  { value: "cancelled", label: "Cancelled" },
  { value: "closed", label: "Closed" },
];

const STATUS_TONE: Record<string, "positive" | "caution" | "critical" | "info" | "neutral"> = {
  todo: "neutral",
  in_progress: "info",
  in_review: "caution",
  done: "positive",
  cancelled: "neutral",
  closed: "neutral",
};

const PRIORITY_TONE: Record<string, "positive" | "caution" | "critical" | "info" | "neutral"> = {
  low: "neutral",
  normal: "neutral",
  high: "caution",
  urgent: "critical",
};

export function Work() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // ONE CLOCK for the screen, ticking on its own: a relative time computed
  // from Date.now() at render is frozen until something else re-renders, so
  // "2 minutes ago" stays that for an hour on a screen nobody touches.
  const now = useNow();

  const [project, setProject] = useParam("project", "");
  const [status, setStatus] = useParam("status", "");
  const [assignee, setAssignee] = useParam("assignee", "");
  const [q, setQ] = useParam("q", "");
  // `open` is THREE-STATED on the wire and here: an absent filter asks for
  // everything, and reading it as false would show only finished work.
  const [scope, setScope] = useParam("scope", "open");
  const [view, setView] = useParam("view", "");
  const [type, setType] = useParam("type", "");
  const [groupBy, setGroupBy] = useParam("group_by", "");

  const params: Record<string, unknown> = {};
  // THE CONTAINER IS THE SCOPE, and an absent one is NEITHER the workspace
  // nor a project — the engine refuses to default it, because an omitted key
  // would otherwise be the most expensive query in the system. This screen is
  // a person's, so the default is everything and it says so.
  params.container = project ? `project:${project}` : "workspace";
  if (status) params.status = status;
  if (type) params.type = type;
  // A GROUPED ANSWER IS A DIFFERENT SHAPE: `groups` replaces `items`, and each
  // column's count is over the whole set rather than over the rows it carries.
  if (groupBy) params.group_by = groupBy;
  if (assignee) params.assignee = assignee;
  if (q) params.q = q;
  // OPEN AND CLOSED ARE STATUS GROUPS, not a boolean: the four groups are
  // what every rule in the tracker is written at, and `done` and `closed` are
  // two of them rather than one negation.
  if (scope === "open") params.status_group = "not_started,active";
  if (scope === "closed") params.status_group = "done,closed";

  // A change to an item publishes onto the seat inbox rather than to the
  // dashboard socket, so there is no push behind this and a poll is correct.
  // Twenty seconds: a board is read, not watched, and a tracker's own pace is
  // a person typing a comment.
  const { data, loading, error } = useQuery("work_items", params, { pollMs: 20_000 });

  // THE STRIP IS A SEPARATE QUESTION from the rows, for the reason
  // `containers` is separate from `pages`: it is drawn once per container and
  // the rows are redrawn on every filter change. Its poll is slower for the
  // same reason — a saved view is arranged by a person, not by the work.
  const strip = useQuery("work_views", { container: params.container }, { pollMs: 120_000 });
  // THE TYPES COME FROM THE CATALOGUE, never from the rows: a filter built
  // from the page can only offer the types that happen to be on it, so a
  // board showing no bugs would offer no way to ask for one. The catalogue
  // is the company's own vocabulary and changes about once a quarter, which
  // is why this poll is the slowest on the screen.
  const catalogue = useQuery("work_catalogue", undefined, { pollMs: 300_000 });
  // THE PROJECT LIST IS THE COMPANY'S, never the page's. Derived from the
  // rows it could only offer the projects that happen to be on screen, so a
  // board filtered to one project offered no way back to another — and the
  // counts beside each name are the MAINTAINED columns, three reads rather
  // than an aggregate over every task in the company. A minute, because a
  // project's shape changes at the pace somebody files work rather than at
  // the pace a board is read.
  const catalogueProjects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  // AND THE PROJECT'S OWN OVERVIEW, only while one is selected: its sprint,
  // its lead and the unit that owns it are what a board scoped to a project
  // cannot say from its rows.
  const overview = useQuery(
    "work_project",
    project ? { key: project } : undefined,
    { pollMs: 60_000 },
  );
  const types = catalogue.data?.types ?? [];
  const views = strip.data?.views ?? [];
  // THE VIEW IS A SET OF DEFAULTS, never a lock: picking one puts its
  // parameters on the URL, where every explicit control still overrides them.
  // Storing the view's id instead would make the filters lie about what is on
  // screen the moment somebody touched one.
  const applyView = (v: WorkView) => {
    setStatus(v.params?.status ?? "");
    setAssignee(v.params?.assignee ?? "");
    setQ(v.params?.q ?? "");
    setScope(scopeOf(v.params?.status_group));
    setType(v.params?.type ?? "");
    setView(v.key);
  };

  const groups = data?.groups ?? [];
  const rows = useMemo(
    () => [...(data?.items ?? [])].sort((a, b) => tsKey(b.updated) - tsKey(a.updated)),
    [data],
  );

  const shown = useMemo(() => shownRows(rows, groups), [groups, rows]);

  const projects = useMemo(
    () => projectKeys(catalogueProjects.data?.projects, shown),
    [catalogueProjects.data, shown],
  );

  const byStatus = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const item of shown) counts[item.status] = (counts[item.status] ?? 0) + 1;
    return counts;
  }, [shown]);

  // BLOCKED IS COUNTED FROM THE FLAG, not from a status: a task is blocked
  // AND in progress, so counting it as a status would have hidden it in
  // whichever of the two the row happened to carry.
  const blocked = useMemo(() => shown.filter((r) => r.blocked).length, [shown]);

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;

  return (
    <>
      <ScreenHead
        title="Work"
        sub="The company's own tracker — every item, who owns it and what moved it. Read-only here: work is filed and moved by the seats themselves, so every change is attributed to somebody."
      />

      {/* The counts are of what is ON SCREEN, and the label says so. A header
          that reported the page's length as the project's size would say
          "50 items" for every project with more than fifty. */}
      {!loading && !error && (
        <StatRow cols={4}>
          <Stat
            label="Shown"
            value={shown.length}
            sub={totalHint(data?.total_hint ?? 0, shown.length)}
          />
          <Stat label="In progress" value={byStatus.in_progress ?? 0} />
          <Stat label="Blocked" value={blocked} icon={blocked ? "alert" : undefined} />
          <Stat label="In review" value={byStatus.in_review ?? 0} />
        </StatRow>
      )}

      {/* THE TAB STRIP. Every container has three of these without anybody
          saving one, so it is never empty and never needs a setup gesture —
          which is also why a failure to read it leaves the board alone
          rather than blocking it: the filters below are the real control,
          and a strip is a shortcut to a set of them. */}
      {views.length > 0 && (
        <div className="toolbar" role="tablist" aria-label="Saved views">
          {views.map((v) => (
            <Chip
              key={v.key}
              on={view === v.key}
              onClick={() => applyView(v)}
              title={v.builtin ? `The built-in ${v.type}` : viewTitle(v)}
            >
              {v.pinned ? "★ " : ""}
              {v.name}
            </Chip>
          ))}
          {view && (
            <Chip onClick={() => setView("")} title="Stop following a view">
              Clear
            </Chip>
          )}
        </div>
      )}

      <div className="toolbar">
        <div style={{ flex: 1, maxWidth: 360 }}>
          <SearchInput
            value={q}
            onChange={setQ}
            ariaLabel="Find an item by key or title"
            placeholder="ENG-42, or words from the title"
          />
        </div>
        <Select
          value={project}
          onChange={setProject}
          ariaLabel="Project"
          anyLabel="Every project"
          options={projects}
        />
        <Select
          value={status}
          onChange={setStatus}
          ariaLabel="Status"
          anyLabel="Any status"
          options={STATUSES.map((s) => s.value)}
        />
        {types.length > 0 && (
          <Select
            value={type}
            onChange={setType}
            ariaLabel="Type"
            anyLabel="Any type"
            options={types.map((t) => t.slug)}
          />
        )}
        <Select
          value={groupBy}
          onChange={setGroupBy}
          ariaLabel="Group by"
          anyLabel="No grouping"
          options={["status", "status_group", "assignee", "priority", "type", "tag"]}
        />
        {/* THREE SEGMENTS, not a checkbox: "open", "closed" and "everything"
            are three real questions, and a two-state control would make the
            third unreachable — which is how a board that can never show a
            closed item ships. */}
        <Segmented
          value={scope}
          onChange={setScope}
          ariaLabel="Open or closed"
          options={[
            { value: "open", label: "Open" },
            { value: "closed", label: "Closed" },
            { value: "", label: "All" },
          ]}
        />
        {assignee && (
          <Chip on onClick={() => setAssignee("")} title="Clear this filter">
            {seatName(assignee)}
          </Chip>
        )}
      </div>

      {/* THE PROJECT'S OWN OVERVIEW, only while one is selected. Its sprint,
          its lead and the unit that owns it are facts about the CONTAINER,
          and a board can say none of them from its rows. */}
      {project && <ProjectOverview detail={overview.data} />}

      {loading && <Skeleton rows={6} />}

      {/* THE BOARD, when one was asked for. Each column reports its own
          count over the WHOLE set beside the rows it carries — so a column
          of four hundred says four hundred and hands back twenty, and the
          header never becomes a property of the page. */}
      {!loading && !error && groups.length > 0 && (
        <>
          {data?.groups_overlap && (
            <Banner tone="info">
              One item can be on several of these columns, so the counts add up to more than the
              total.
            </Banner>
          )}
          {data?.groups_dropped ? (
            <Banner tone="caution">
              {data.groups_dropped} more column{data.groups_dropped === 1 ? "" : "s"} did not fit
              and are not shown.
            </Banner>
          ) : null}
          <div className="board">
            {groups.map((group) => (
              <Panel
                key={group.key}
                title={group.label || group.key || "—"}
                count={group.count}
                padding="tight"
              >
                {group.rows.map((item) => (
                  <div key={item.id} className="row gap-sm">
                    <a className="mono" href={href(["work", item.key])}>
                      {item.key}
                    </a>
                    <a href={href(["work", item.key])} className="truncate">
                      {item.title}
                    </a>
                  </div>
                ))}
                {group.count > group.rows.length && (
                  <p className="t-caption faint">
                    {group.count - group.rows.length} more in this column
                  </p>
                )}
              </Panel>
            ))}
          </div>
        </>
      )}

      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length || groups.length
            ? undefined
            : {
                title: "Nothing matches",
                hint: "No item on this node's copy of the tracker matches these filters. Widen them, or check that work is being filed at all.",
              }
        }
      >
        <Coverage answer={data} />
        {groups.length === 0 && (
          <Panel>
            <DataTable
              rows={rows}
              rowKey={(r) => r.id}
              defaultSort={{ key: "updated", dir: "desc" }}
              columns={[
                {
                  key: "key",
                  header: "Key",
                  shrink: true,
                  sortValue: (r) => r.key,
                  cell: (r) => (
                    <a className="mono" href={href(["work", r.key])}>
                      {r.key}
                    </a>
                  ),
                },
                {
                  key: "title",
                  header: "Title",
                  sortValue: (r) => r.title,
                  cell: (r) => (
                    <a href={href(["work", r.key])} className="truncate">
                      {r.title}
                    </a>
                  ),
                },
                {
                  key: "status",
                  header: "Status",
                  shrink: true,
                  sortValue: (r) => r.status,
                  cell: (r) => (
                    <>
                      <Badge tone={STATUS_TONE[r.status] ?? "neutral"} dot>
                        {STATUSES.find((s) => s.value === r.status)?.label ?? r.status}
                      </Badge>
                      {/* BESIDE the status rather than instead of it, which is
                        the whole reason it is its own field: a task in
                        progress with an open blocker is both, and a column
                        that showed one would hide the other. */}
                      {r.blocked && (
                        <Badge tone="critical" outline>
                          Blocked
                        </Badge>
                      )}
                    </>
                  ),
                },
                {
                  key: "priority",
                  header: "Priority",
                  shrink: true,
                  sortValue: (r) => r.priority ?? "",
                  cell: (r) =>
                    r.priority && r.priority !== "normal" ? (
                      <Badge tone={PRIORITY_TONE[r.priority] ?? "neutral"}>{r.priority}</Badge>
                    ) : (
                      <span className="dim">—</span>
                    ),
                },
                {
                  key: "assignee",
                  header: "Assignee",
                  shrink: true,
                  sortValue: (r) => r.assignee ?? "",
                  cell: (r) =>
                    r.assignee ? (
                      <SeatChip name={seatName(r.assignee)} handle={r.assignee} />
                    ) : (
                      // NOBODY IS A STATE, and the one worth seeing: an
                      // unassigned item routes to the project's lead, and a
                      // project with no lead routes to nobody at all.
                      <span className="dim">Unassigned</span>
                    ),
                },
                {
                  key: "updated",
                  header: "Updated",
                  shrink: true,
                  align: "right",
                  sortValue: (r) => tsKey(r.updated),
                  cell: (r) => (
                    <span title={fmtDateTime(r.updated)}>{relTime(r.updated, now)}</span>
                  ),
                },
              ]}
            />
          </Panel>
        )}
      </QueryState>

      {!loading && !error && projects.length === 0 && shown.length === 0 && (
        <Empty
          icon="inbox"
          title="No work has been filed yet"
          hint="Seats file work with create_work_item, and an inbound webhook or a schedule is usually what starts them. A project's key comes from a unit's `project` field."
        />
      )}
    </>
  );
}

/** One item: its description, its thread and everything that moved it. */
/**
 * EVERY ROW ON SCREEN, grouped or flat.
 *
 * A grouped answer carries NO flat `items` by construction — `groups` replaces
 * them, because returning both would be the same rows twice — so everything
 * derived from `items` alone reported a fully populated board as empty: the
 * header read 0 shown, 0 in progress, 0 blocked, and the panel at the foot of
 * the screen drew "No work has been filed yet" underneath the board's own
 * columns.
 *
 * The top-level rows are enough and subgroup rows are deliberately NOT added:
 * a subgroup's rows are a slice of its own column's, so folding them in would
 * count the same task twice.
 */
/** One project's overview strip — the container facts a board cannot show.
 *
 *  ABSENT RATHER THAN EMPTY while the read is in flight or a project has no
 *  answer: an overview rendered with zeroes is a project that looks
 *  unstaffed, unled and out of sprint, which is a conclusion somebody acts
 *  on. */
export function ProjectOverview({ detail }: { detail?: WorkProjectDetail | null }) {
  if (!detail) return null;
  const sprint = detail.sprints?.active;
  const pending = detail.sprints?.pending_spillovers ?? [];
  return (
    <>
      {/* A UNIT THE CHART NO LONGER HAS is a finding, not a blank: it is what
          leaves a project's work routed to nobody. */}
      {!detail.unit.resolved && (
        <Banner tone="caution">
          This project names the unit <span className="mono">{detail.unit.key}</span>, which the
          current org chart does not have — work filed here routes to nobody.
        </Banner>
      )}
      {/* A CLOSED SPRINT NOBODY HAS SETTLED. The work is neither carried
          forward nor dropped until a lead says which. */}
      {pending.length > 0 && (
        <Banner tone="caution">
          Sprint{pending.length === 1 ? "" : "s"} {pending.join(", ")} closed with the spillover
          still undecided — the unfinished work is waiting on a lead.
        </Banner>
      )}
      <StatRow cols={4}>
        <Stat label="Open" value={detail.task_counts.open} sub={detail.name} />
        <Stat label="Done" value={detail.task_counts.done} />
        <Stat
          label="Lead"
          value={detail.lead.handle || "none"}
          sub={detail.unit.name || detail.unit.key || "no unit"}
        />
        {/* THE MEASURE IS ON THE LABEL, because a bare "12 of 34" is points
            to one team and minutes to another. */}
        <Stat
          label={sprint ? `Sprint ${sprint.number}` : "Sprint"}
          value={sprint ? `${sprint.figures.done} / ${sprint.figures.committed +
            sprint.figures.added}` : "none"}
          sub={
            sprint
              ? `${sprint.figures.measure === "points" ? "points" : "minutes"} · ${
                sprint.days_remaining
              }d left`
              : "this project runs none"
          }
        />
      </StatRow>
    </>
  );
}

/** The project filter's options: the COMPANY's own listing, falling back to
 *  whatever is on the page.
 *
 *  The listing is the honest set — a filter built from the rows can only offer
 *  the projects that happen to be on them, so a board already narrowed to one
 *  offered exactly one choice and no way back. The fallback is not decoration:
 *  the listing is a separate poll, and a filter that empties while it is in
 *  flight is a control that flickers every time the board is re-read. */
export function projectKeys(
  listed: WorkProjectRow[] | undefined,
  shown: WorkSummary[],
): string[] {
  if (listed && listed.length > 0) return listed.map((p) => p.key);
  const keys = new Set<string>();
  for (const item of shown) keys.add(item.project);
  return [...keys].sort();
}

export function shownRows(items: WorkSummary[], groups: WorkGroup[]): WorkSummary[] {
  if (groups.length === 0) return items;
  return groups.flatMap((group) => group.rows);
}

export function WorkItem({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const { data, loading, error } = useQuery(
    "work_item",
    { id },
    { enabled: id !== "", pollMs: 15_000 },
  );

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const item = data?.task;

  return (
    <>
      <ScreenHead
        title={item?.key || id || "Item"}
        sub={item?.title}
        badges={
          item ? (
            <>
              <Badge tone={STATUS_TONE[item.status] ?? "neutral"} dot>
                {STATUSES.find((s) => s.value === item.status)?.label ?? item.status}
              </Badge>
              {data?.blocked && (
                <Badge tone="critical" outline>
                  Blocked
                </Badge>
              )}
              {item.type && <Badge outline>{item.type}</Badge>}
              {item.project && <Badge outline>{item.project}</Badge>}
            </>
          ) : undefined
        }
      />

      {loading && <Skeleton rows={8} />}

      <QueryState error={error} loading={loading}>
        {item && (
          <div className="stack">
            <Coverage answer={data} />
            <Panel title="Description">
              {item.body ? (
                <div className="prose">{item.body}</div>
              ) : (
                <span className="dim">No description was written.</span>
              )}
            </Panel>

            <Panel title="Ownership">
              <StatRow cols={3}>
                <Stat
                  label="Assignee"
                  value={item.assignee ? seatName(item.assignee) : "Unassigned"}
                />
                <Stat label="Reporter" value={item.reporter ? seatName(item.reporter) : "—"} />
                {/* THE REASSIGNMENT COUNT IS A BUDGET, not trivia: an item
                    handed on too many times has stopped being work and
                    started being a hot potato, and the engine refuses the
                    next hand-off rather than letting it circle. */}
                <Stat
                  label="Hand-offs"
                  value={item.reassignments ?? 0}
                  sub="each reassignment spends the item's own budget"
                  icon={(item.reassignments ?? 0) >= 6 ? "alert" : undefined}
                />
              </StatRow>
              {item.watchers?.length ? (
                <div
                  className="row wrap"
                  style={{ gap: "var(--space-2)", marginTop: "var(--space-3)" }}
                >
                  <span className="dim">Watching:</span>
                  {item.watchers.map((w) => (
                    <SeatChip key={w} name={seatName(w)} handle={w} />
                  ))}
                </div>
              ) : null}
            </Panel>

            {data.links?.length ? (
              <Panel title="Links">
                <ul className="list">
                  {data.links.map((link) => (
                    <li
                      key={`${link.kind}:${link.other}`}
                      className="row"
                      style={{ gap: "var(--space-2)" }}
                    >
                      <Badge outline>{link.kind.replace(/_/g, " ")}</Badge>
                      <a href={href(["work", link.key || link.other])} className="mono">
                        {link.key || link.other}
                      </a>
                      <span className="truncate">{link.title}</span>
                      {/* The DERIVED half is the one nobody authored — an
                          editor has to change the other end. */}
                      {link.derived && <span className="dim">(the other end authored this)</span>}
                    </li>
                  ))}
                </ul>
              </Panel>
            ) : null}

            <Panel title={`Thread (${data.comments?.length ?? 0})`}>
              {data.comments?.length ? (
                <div className="stack">
                  {data.comments.map((c) => (
                    <div key={c.id} className="comment">
                      <div className="row" style={{ gap: "var(--space-2)" }}>
                        <SeatChip name={seatName(c.author)} handle={c.author} />
                        <span className="dim" title={fmtDateTime(c.created_at)}>
                          {relTime(c.created_at, now)}
                        </span>
                        {c.updated_at && <span className="dim">(edited)</span>}
                        {c.resolved && <Badge tone="positive">Resolved</Badge>}
                      </div>
                      <div className="prose">{c.body}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <span className="dim">Nobody has commented.</span>
              )}
            </Panel>

            <Panel title="History">
              {data.history?.length ? (
                <ul className="list">
                  {data.history.map((change) => (
                    <li key={change.id} className="row" style={{ gap: "var(--space-2)" }}>
                      <Icon name="activity" size="sm" />
                      <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                      <span className="dim">{change.kind.replace(/_/g, " ")}</span>
                      {/* The snapshot records what changed as VALUES, not as
                          from/to pairs — the applier writes the state the
                          change produced, and the previous value survives in
                          the entry before it. */}
                      {change.fields &&
                        Object.keys(change.fields).map((field) => (
                          <span key={field} className="dim">
                            {field}
                          </span>
                        ))}
                      {/* A COMMIT THAT WOKE NOBODY is a fact about the change
                          rather than about its importance: a bulk edit is
                          quiet by construction. */}
                      {change.quiet && <span className="dim">(quiet)</span>}
                      <span className="spacer" />
                      <span className="dim" title={fmtDateTime(change.at)}>
                        {relTime(change.at, now)}
                      </span>
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="dim">Nothing has moved this item yet.</span>
              )}
            </Panel>
          </div>
        )}
      </QueryState>
    </>
  );
}

/** The coverage half every tracker answer carries — a board's and a task's
 *  alike, which is why this is its own type rather than either answer's. */
interface CoverageFacts {
  read_level?: string;
  complete?: boolean;
  log_seq?: number;
  applied_through?: number;
  incomplete?: WorkIncomplete;
}

/** How stale an answer may be, and what it could not account for.
 *
 * # Two different facts, and a screen that shows only one lies
 *
 * `read_level` says how FRESH the answer is — whether it came from this
 * node's rows as they stood, or from a position the caller's own write is at
 * or below. `complete` says whether the answer could account for everything
 * it was asked about: a node holding records this build cannot decode has
 * rows that may be missing, rows that should have left and may still be
 * present, and totals computed over the incomplete set.
 *
 * A screen that renders the freshness badge and swallows the coverage flag is
 * worse than a stale tile, because a person reads "a moment ago" and
 * concludes the board is right. So the incomplete line is a BANNER above the
 * rows rather than a badge beside them, it names the count and the scope, and
 * it says the remedy — which is a build that can read the records, not a
 * refresh.
 */
function Coverage({ answer }: { answer?: CoverageFacts | null }) {
  if (!answer) return null;
  const behind =
    answer.applied_through !== undefined &&
    answer.log_seq !== undefined &&
    answer.applied_through < answer.log_seq;

  return (
    <>
      {answer.complete === false && (
        <Banner tone="caution">
          <strong>
            This answer is incomplete
            {answer.incomplete
              ? ` — ${answer.incomplete.records} record(s) this build cannot read`
              : ""}
          </strong>{" "}
          Rows may be missing, rows that should have gone may still be here, and the counts were
          computed over what is shown.
          {answer.incomplete?.scope?.length
            ? ` Affected: ${answer.incomplete.scope.join(", ")}.`
            : ""}
          {answer.incomplete
            ? ` Record version ${answer.incomplete.version}, from sequence ${answer.incomplete.from.seq} — a build that can read it is what resolves this, not a refresh.`
            : ""}
        </Banner>
      )}
      <div className="row" style={{ gap: "var(--space-2)" }}>
        {answer.read_level && (
          <Badge outline title="How fresh this answer is, as the engine actually served it">
            {answer.read_level}
          </Badge>
        )}
        {/* APPLIED_THROUGH BESIDE SEQ, so a node holding something it cannot
            apply is visible as its own state rather than as lag. */}
        {behind && (
          <Badge tone="caution" outline>
            applied through {answer.applied_through} of {answer.log_seq}
          </Badge>
        )}
      </div>
    </>
  );
}

/** The board's own count against the engine's hint.
 *
 * The hint is CAPPED by construction — an exact total over an unbounded set
 * is the one query in this grammar that turns a poll into a scan — so at the
 * ceiling it says "10000+" rather than a number nobody needs.
 */
function totalHint(hint: number, shown: number): string {
  if (hint <= 0) return "";
  if (hint >= 10_000) return "of 10000+ matching";
  if (hint <= shown) return `of ${hint} matching`;
  return `of ${hint} matching — page through for the rest`;
}

/** scopeOf maps a view's status_group back onto the board's three segments.
 *
 *  THE SEGMENTS ARE A SHORTHAND for the two groups each names, so a view
 *  carrying exactly those groups lands on the segment rather than on "All"
 *  with an invisible filter. Anything else is "All": a view filtering to one
 *  group is not one of the three questions the control asks. */
function scopeOf(group: string | undefined): string {
  if (group === "not_started,active") return "open";
  if (group === "done,closed") return "closed";
  return "";
}

/** viewTitle says whose a saved view is, which is the one thing its name
 *  cannot: two people's "My work" are two different views. */
function viewTitle(v: WorkView): string {
  const parts: string[] = [];
  if (v.owner) parts.push(`${v.owner}'s`);
  parts.push(v.type);
  if (v.protected) parts.push("— only its owner may change it");
  return parts.join(" ");
}
