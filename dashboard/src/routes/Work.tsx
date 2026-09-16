/**
 * The work board: the company's own tracker.
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
 * which is the same copy a seat's tools read, so an operator and an agent
 * looking at one item see one item. A node that has not finished its boot
 * reconcile REFUSES rather than answering empty, and `QueryState` renders
 * that refusal as "still catching up". Drawing it as "no work" would be an
 * answer somebody acts on, by filing the duplicate.
 *
 * # Read-only, deliberately
 *
 * Nothing here writes. An item is filed and moved by a seat's own tools, or
 * by an operator through the MCP surface, and both are attributed to
 * somebody, where a board button would write as "the dashboard", which is
 * not a person and not a seat and cannot be asked why.
 *
 * # What it is drawn with
 *
 * The design system, throughout. The table is `RecordTable`, so its pager,
 * its settings frame and the reader's own column order arrive with it and
 * live in the URL rather than in a hand-rolled row of controls; the filter
 * bar is a `Toolbar` of real pickers, which is what keeps every one of them
 * on screen in BOTH shapes this answer takes, flat rows and a grouped board.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, RecordTable, SeatChip } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { humanize, tsKey } from "~/lib/format.ts";
import type {
  WorkGroup,
  WorkActivityRecord,
  WorkProjectDetail,
  WorkProjectRow,
  WorkIncomplete,
  WorkStatus,
  WorkSummary,
  WorkView,
} from "~/protocol/index.ts";
import {
  ArrowForwardGlyph,
  BlockGlyph,
  ChatGlyph,
  DescriptionGlyph,
  FlagGlyph,
  GroupGlyph,
  LinkGlyph,
  ListGlyph,
  PersonGlyph,
  SearchGlyph,
  TimelineGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import {
  AutoGrid,
  ButtonLink,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  FilterChip,
  FilterChipGroup,
  InlineCode,
  Input,
  List,
  PageHeader,
  RelativeTime,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  StatCard,
  StatGroup,
  Tag,
  Toolbar,
  useNow,
} from "@crewlethq/ui";
import type { DataViewColumn, Tone } from "@crewlethq/ui";

/** The tracker's SIX. `blocked` is NOT one: a blocker is data carried beside
 *  the status, because a task can be both in progress and blocked and a single
 *  field cannot say so, so it is a badge and a filter, never a column value.
 *  `cancelled` is here and reads as "finished without being delivered", which
 *  is the whole of what a close reason used to say.
 *
 *  A CLOSED SET, so a status the engine adds later shows as itself rather than
 *  vanishing from the filter. */
const STATUSES: { value: WorkStatus; label: string }[] = [
  { value: "todo", label: "To do" },
  { value: "in_progress", label: "In progress" },
  { value: "in_review", label: "In review" },
  { value: "done", label: "Done" },
  { value: "cancelled", label: "Cancelled" },
  { value: "closed", label: "Closed" },
];

const STATUS_TONE: Record<string, Tone> = {
  todo: "neutral",
  in_progress: "info",
  in_review: "warning",
  done: "success",
  cancelled: "neutral",
  closed: "neutral",
};

const PRIORITY_TONE: Record<string, Tone> = {
  low: "neutral",
  normal: "neutral",
  high: "warning",
  urgent: "danger",
};

/** The axes the engine will group a board on, with the words a reader uses for
 *  them. The VALUES are the engine's own parameter names and the labels are
 *  ours: a picker offering `status_group` is a picker that only somebody who
 *  has read the wire format can use. */
const GROUPINGS: { value: string; label: string }[] = [
  { value: "", label: "No grouping" },
  { value: "status", label: "By status" },
  { value: "status_group", label: "By status group" },
  { value: "assignee", label: "By assignee" },
  { value: "priority", label: "By priority" },
  { value: "type", label: "By type" },
  { value: "tag", label: "By tag" },
];

/**
 * The three questions the open/closed control asks.
 *
 * `all` IS A WORD RATHER THAN THE EMPTY STRING, and that is not a spelling
 * choice. This parameter's absent value is `open`, and `useParam` writes a
 * filter by DELETING the key whenever the value it is given is empty, so the
 * third segment wrote nothing to the URL and read straight back as the first:
 * the board this replaces could not be shown everything at all, under a
 * comment explaining that a two-state control would make the third question
 * unreachable. A value an absent key does not already mean is what makes it
 * reachable.
 */
type Scope = "open" | "closed" | "all";

const SCOPES: { value: Scope; label: string }[] = [
  { value: "open", label: "Open" },
  { value: "closed", label: "Closed" },
  { value: "all", label: "All" },
];

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
  const [scopeParam, setScope] = useParam("scope", "open");
  // A VALUE OFF THE URL IS UNTRUSTED, and this one decides both what is drawn
  // and what is asked for. A spelling the control does not offer read as "all"
  // in the query and as the FIRST segment in the row, so the board showed
  // everything under a control saying Open.
  const scope: Scope = scopeParam === "open" || scopeParam === "closed" ? scopeParam : "all";
  const [view, setView] = useParam("view", "");
  const [type, setType] = useParam("type", "");
  const [groupBy, setGroupBy] = useParam("group_by", "");

  const params: Record<string, unknown> = {};
  // THE CONTAINER IS THE SCOPE, and an absent one is NEITHER the workspace
  // nor a project: the engine refuses to default it, because an omitted key
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
  // same reason: a saved view is arranged by a person, not by the work.
  const strip = useQuery("work_views", { container: params.container }, { pollMs: 120_000 });
  // THE TYPES COME FROM THE CATALOGUE, never from the rows: a filter built
  // from the page can only offer the types that happen to be on it, so a
  // board showing no bugs would offer no way to ask for one. The catalogue
  // is the company's own vocabulary and changes about once a quarter, which
  // is why this poll is the slowest on the screen.
  const catalogue = useQuery("work_catalogue", undefined, { pollMs: 300_000 });
  // THE PROJECT LIST IS THE COMPANY'S, never the page's. Derived from the
  // rows it could only offer the projects that happen to be on screen, so a
  // board filtered to one project offered no way back to another. The counts
  // beside each name are the MAINTAINED columns, three reads rather than an
  // aggregate over every task in the company. A minute, because a project's
  // shape changes at the pace somebody files work rather than at the pace a
  // board is read.
  const catalogueProjects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  // AND THE PROJECT'S OWN OVERVIEW, only while one is selected: its sprint,
  // its lead and the unit that owns it are what a board scoped to a project
  // cannot say from its rows.
  // ENABLED ON THE SELECTION, not just parameterised by it. The engine
  // refuses this question without a key, correctly, since there is no default
  // project, so passing no params is not "ask for everything", it is a query
  // that fails every poll: a warning a minute in the operator's log, and a
  // screen holding an error for a question nobody asked.
  const overview = useQuery("work_project", project ? { key: project } : undefined, {
    enabled: project !== "",
    pollMs: 60_000,
  });
  // AND WHAT HAPPENED, which is a different question from what is there: the
  // feed is ordered by the LOG rather than by anything this board sorts on,
  // so a change that moved nothing on screen is still visible.
  const feed = useQuery(
    "work_activity",
    { container: params.container, limit: 20 },
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

  const byHandle = index.byHandle;
  const seatName = (handle: string) => byHandle.get(handle)?.name ?? handle;

  const columns = useMemo<DataViewColumn<WorkSummary>[]>(
    () => [
      {
        key: "key",
        header: "Key",
        shrink: true,
        mono: true,
        sortable: true,
        // WHICH ITEM cannot be hidden: every other cell is a fact about a
        // task, and a row of them belonging to no key is a row nobody can
        // quote into a tool call or find again.
        hideable: false,
        sortValue: (r) => r.key,
        render: (r) => r.key,
      },
      {
        key: "title",
        header: "Title",
        sortable: true,
        sortValue: (r) => r.title,
        render: (r) => <span className="truncate">{r.title}</span>,
      },
      {
        key: "status",
        header: "Status",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.status,
        render: (r) => (
          <span className="row gap-1">
            <Tag variant={STATUS_TONE[r.status] ?? "neutral"} dot>
              {STATUSES.find((s) => s.value === r.status)?.label ?? r.status}
            </Tag>
            {/* BESIDE the status rather than instead of it, which is the whole
                reason it is its own field: a task in progress with an open
                blocker is both, and a column that showed one would hide the
                other. */}
            {r.blocked && (
              <Tag variant="danger" appearance="outline">
                Blocked
              </Tag>
            )}
          </span>
        ),
      },
      {
        key: "priority",
        header: "Priority",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.priority ?? "",
        // ONLY A PRIORITY WORTH ACTING ON IS DRAWN. The label is what keeps
        // the two blank cases apart for a reader who cannot see the mark:
        // "normal" is a decision somebody made, and nothing at all is not.
        render: (r) =>
          r.priority && r.priority !== "normal" ? (
            <Tag variant={PRIORITY_TONE[r.priority] ?? "neutral"}>{r.priority}</Tag>
          ) : (
            <EmptyValue label={r.priority ? "Normal" : "Not set"} />
          ),
      },
      {
        key: "assignee",
        header: "Assignee",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.assignee ?? "",
        render: (r) =>
          r.assignee ? (
            <SeatChip name={byHandle.get(r.assignee)?.name ?? r.assignee} handle={r.assignee} />
          ) : (
            // NOBODY IS A STATE, and the one worth seeing: an unassigned item
            // routes to the project's lead, and a project with no lead routes
            // to nobody at all. So it is a word rather than an absence mark.
            <span className="muted">Unassigned</span>
          ),
      },
      {
        key: "updated",
        header: "Updated",
        shrink: true,
        align: "right",
        sortable: true,
        firstDirection: "desc",
        sortValue: (r) => tsKey(r.updated),
        // NO `title` OF OUR OWN: the component already carries the exact
        // instant in its own `title` and in `dateTime`, and a title passed in
        // is overwritten by it, so one here is a string computed every render
        // for an attribute nothing reads.
        render: (r) => <RelativeTime value={r.updated} now={now} />,
      },
    ],
    [byHandle, now],
  );

  // WHICH EMPTY CASE THIS IS, decided once so exactly one of them is drawn.
  // "Nothing matches these filters" and "nothing has ever been filed" are
  // different facts with different remedies, and the screen this replaces
  // drew BOTH of them, stacked, on a company that had never filed anything.
  const nothingFiled = projects.length === 0 && shown.length === 0;

  return (
    <>
      <PageHeader
        title="Work"
        description="The company's own tracker: every item, who owns it and what moved it. Read-only here, because work is filed and moved by the seats themselves, so every change is attributed to somebody."
        actions={
          // ONE PERSON'S DAY IS NOT A NAV ENTRY, because route dispatch is a
          // switch on the first path segment and `work` already owns it: two
          // entries claiming one head would make one of them silently
          // unreachable. It is reached from here instead, which is also where
          // somebody is when they want it.
          <ButtonLink
            variant="secondary"
            href={href(["work", "me"])}
            trailingIcon={<ArrowForwardGlyph />}
          >
            My work
          </ButtonLink>
        }
      />

      {/* The counts are of what is ON SCREEN, and the label says so. A header
          that reported the page's length as the project's size would say
          "50 items" for every project with more than fifty. */}
      {!loading && !error && (
        <StatGroup columns={4}>
          <StatCard
            icon={<ListGlyph />}
            label="Shown"
            value={shown.length}
            sub={totalHint(data?.total_hint ?? 0, shown.length, data?.total_capped)}
          />
          <StatCard
            icon={<TimelineGlyph />}
            label="In progress"
            value={byStatus.in_progress ?? 0}
          />
          <StatCard
            icon={<BlockGlyph />}
            label="Blocked"
            value={blocked}
            sub="waiting on an open dependency"
            {...(blocked ? { tone: "warning" as const } : {})}
          />
          <StatCard icon={<GroupGlyph />} label="In review" value={byStatus.in_review ?? 0} />
        </StatGroup>
      )}

      {/* THE VIEW STRIP. Every container has three of these without anybody
          saving one, so it is never empty and never needs a setup gesture,
          which is also why a failure to read it leaves the board alone rather
          than blocking it: the filters below are the real control, and a
          strip is a shortcut to a set of them.

          A NAMED ROW OF FILTERS, not a tab list. It declared `role="tablist"`
          over children that were plain buttons, so it promised a reader tabs
          it does not contain, and it promised one tab stop with arrows inside
          it while delivering N stops with no arrow keys. The design system's
          own filter row is both halves of that promise kept: one stop, the
          arrows walking it, each chip announced with its pressed state. It is
          a RADIO row because a board follows one view at a time, and
          `allowNone` is what the Clear chip used to be: a second press on the
          view you are following stops following it. */}
      {views.length > 0 && (
        <FilterChipGroup
          label="Saved views"
          semantics="radio"
          allowNone
          value={view || null}
          onValueChange={(next) => {
            const picked = next ? views.find((v) => v.key === next) : undefined;
            if (picked) applyView(picked);
            else setView("");
          }}
        >
          {views.map((v) => (
            <FilterChip
              key={v.key}
              value={v.key}
              title={v.builtin ? `The built-in ${v.type}` : viewTitle(v)}
            >
              {/* PINNED IS THIS VIEWER'S, never the row's, so the mark carries
                  its own name rather than leaving a reader to guess what a
                  symbol before a title means. */}
              {v.pinned ? <FlagGlyph size="xs" title="Pinned for you" /> : null}
              {v.name}
            </FilterChip>
          ))}
        </FilterChipGroup>
      )}

      {/* THE FILTER BAR IS THE SCREEN'S OWN, and it is drawn above BOTH shapes
          this answer takes. A list view's own filter menu would go with the
          table, and the table is exactly what a grouped board replaces: every
          control here would disappear the moment somebody grouped the board,
          on the screen where narrowing matters most. */}
      <Toolbar label="Work filters" mode="group" sticky>
        <Input
          type="search"
          width="md"
          value={q}
          onChange={(event) => setQ(event.target.value)}
          onClear={() => setQ("")}
          aria-label="Find an item by key or title"
          placeholder="ENG-42, or words from the title"
          leading={<SearchGlyph size="sm" />}
        />
        <Select
          value={project}
          onChange={(next) => setProject(String(next))}
          ariaLabel="Project"
          width="auto"
          active={project !== ""}
          /* THE ONE PICKER HERE WHOSE SET IS UNBOUNDED, so it is the one that
             searches. Every other list on this bar is closed and short: six
             statuses, seven groupings, a type catalogue a company curates. The
             project list grows with the work, which is why the engine's own
             answer carries a `truncated` flag for it, and a search that
             appeared at some count would be a control that behaved differently
             on two companies for a reason no reader can see. */
          searchable
          options={[
            { value: "", label: "Every project" },
            ...projects.map((key) => ({ value: key, label: key })),
          ]}
        />
        <Select
          value={status}
          onChange={(next) => setStatus(String(next))}
          ariaLabel="Status"
          width="auto"
          active={status !== ""}
          options={[
            { value: "", label: "Any status" },
            ...STATUSES.map((s) => ({ value: s.value, label: s.label })),
          ]}
        />
        {types.length > 0 && (
          <Select
            value={type}
            onChange={(next) => setType(String(next))}
            ariaLabel="Type"
            width="auto"
            active={type !== ""}
            options={[
              { value: "", label: "Any type" },
              ...types.map((t) => ({ value: t.slug, label: t.name || t.slug })),
            ]}
          />
        )}
        <Select
          value={groupBy}
          onChange={(next) => setGroupBy(String(next))}
          ariaLabel="Group by"
          width="auto"
          active={groupBy !== ""}
          options={GROUPINGS.map((g) => ({ value: g.value, label: g.label }))}
        />
        {/* THREE SEGMENTS, not a checkbox: "open", "closed" and "everything"
            are three real questions, and a two-state control would make the
            third unreachable, which is how a board that can never show a
            closed item ships. A radio row rather than a tab row, because it
            opens no panel of its own and its parameter REPLACES rather than
            pushes, so arrowing across it costs no history entries. */}
        <SegmentedControl<Scope>
          label="Open or closed"
          semantics="radio"
          value={scope}
          onValueChange={setScope}
          options={SCOPES}
        />
        {assignee && (
          <FilterChip pressed onPressedChange={() => setAssignee("")} title="Clear this filter">
            Assignee: {seatName(assignee)}
          </FilterChip>
        )}
      </Toolbar>

      {/* THE PROJECT'S OWN OVERVIEW, only while one is selected. Its sprint,
          its lead and the unit that owns it are facts about the CONTAINER,
          and a board can say none of them from its rows. */}
      {project && <ProjectOverview detail={overview.data} />}

      <ActivityFeed records={feed.data?.records ?? []} now={now} />

      {loading && <Skeleton label="Loading the work board" variant="text" rows={6} />}

      {/* THE BOARD, when one was asked for. Each column reports its own count
          over the WHOLE set beside the rows it carries, so a column of four
          hundred says four hundred and hands back twenty, and the header
          never becomes a property of the page. */}
      {!loading && !error && groups.length > 0 && (
        <>
          {data?.groups_overlap && (
            <Callout variant="info">
              <span>
                One item can be on several of these columns, so the counts add up to more than the
                total.
              </span>
            </Callout>
          )}
          {data?.groups_dropped ? (
            <Callout variant="warning">
              <span>
                {data.groups_dropped} more column{data.groups_dropped === 1 ? "" : "s"} did not fit
                and are not shown.
              </span>
            </Callout>
          ) : null}
          <AutoGrid min="md" gap={3}>
            {groups.map((group) => (
              <Card key={group.key} as="section" padding="none">
                <Card.Header divided count={group.count}>
                  <Card.Title>
                    {group.label || group.key || <EmptyValue label="No value on this axis" />}
                  </Card.Title>
                </Card.Header>
                <List>
                  {group.rows.map((item) => (
                    <List.Item
                      key={item.id}
                      href={href(["work", item.key])}
                      leading={<InlineCode>{item.key}</InlineCode>}
                    >
                      <span className="truncate">{item.title}</span>
                    </List.Item>
                  ))}
                </List>
                {group.count > group.rows.length && (
                  <Card.Footer variant="meta">
                    {group.count - group.rows.length} more in this column
                  </Card.Footer>
                )}
              </Card>
            ))}
          </AutoGrid>
        </>
      )}

      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length || groups.length
            ? undefined
            : nothingFiled
              ? {
                  title: "No work has been filed yet",
                  // THE TOOL AND THE FIELD ARE SET AS WHAT THEY ARE. This is
                  // the sentence somebody reads before they go and configure
                  // something, and a tool name that reads as ordinary prose is
                  // one they retype wrong.
                  hint: (
                    <>
                      Seats file work with <InlineCode>create_work_item</InlineCode>, and an inbound
                      webhook or a schedule is usually what starts them. A project&apos;s key comes
                      from a unit&apos;s <InlineCode>project</InlineCode> field.
                    </>
                  ),
                }
              : {
                  title: "Nothing matches",
                  hint: "No item on this node's copy of the tracker matches these filters. Widen them, or check that work is being filed at all.",
                }
        }
      >
        <Coverage answer={data} />
        {groups.length === 0 && (
          <Card as="section" padding="none">
            <Card.Header divided icon={<ListGlyph size="sm" />} count={rows.length}>
              <Card.Title>Items</Card.Title>
            </Card.Header>
            <RecordTable
              screen="work"
              table="items"
              rows={rows}
              getRowKey={(r) => r.id}
              getRowHref={(r) => href(["work", r.key])}
              defaultSort={{ key: "updated", direction: "desc" }}
              // A POLL RE-RANKS A TABLE SORTED BY `updated`, and this one polls
              // every twenty seconds: without this the row somebody is reading
              // is not the row they click. The next press on a header re-ranks
              // everything.
              stableOrder
              rowTone={(r) => (r.blocked ? "warning" : null)}
              columns={columns}
            />
          </Card>
        )}
      </QueryState>
    </>
  );
}

/** One project's overview strip: the container facts a board cannot show.
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
        <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
          <span>
            This project names the unit <InlineCode>{detail.unit.key}</InlineCode>, which the
            current org chart does not have. Work filed here routes to nobody.
          </span>
        </Callout>
      )}
      {/* A CLOSED SPRINT NOBODY HAS SETTLED. The work is neither carried
          forward nor dropped until a lead says which. */}
      {pending.length > 0 && (
        <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
          <span>
            Sprint{pending.length === 1 ? "" : "s"} {pending.join(", ")} closed with the spillover
            still undecided. The unfinished work is waiting on a lead.
          </span>
        </Callout>
      )}
      <StatGroup columns={4}>
        <StatCard label="Open" value={detail.task_counts.open} sub={detail.name} />
        <StatCard label="Done" value={detail.task_counts.done} />
        <StatCard
          label="Lead"
          value={detail.lead.handle || "none"}
          sub={detail.unit.name || detail.unit.key || "no unit"}
        />
        {/* THE MEASURE IS ON THE LABEL, because a bare "12 of 34" is points
            to one team and minutes to another. */}
        <StatCard
          label={sprint ? `Sprint ${sprint.number}` : "Sprint"}
          value={
            sprint
              ? `${sprint.figures.done} / ${sprint.figures.committed + sprint.figures.added}`
              : "none"
          }
          sub={
            sprint
              ? `${sprint.figures.measure === "points" ? "points" : "minutes"} · ${
                  sprint.days_remaining
                }d left`
              : "this project runs none"
          }
        />
      </StatGroup>
    </>
  );
}

/** What happened in this container lately.
 *
 *  A DIFFERENT QUESTION from what is on the board: the feed is ordered by the
 *  log rather than by anything the rows sort on, so a change that moved
 *  nothing on screen, a comment, a watcher, a quiet re-type, is still
 *  visible. It renders the AUTHORED instant, which is what the writer's clock
 *  said and what "yesterday" has to keep meaning.
 *
 *  ABSENT when empty rather than drawn as an empty panel: a board with no
 *  history yet has nothing to say about it. */
export function ActivityFeed({ records, now }: { records: WorkActivityRecord[]; now: number }) {
  if (records.length === 0) return null;
  return (
    <Card as="section" padding="none">
      <Card.Header divided icon={<TimelineGlyph size="sm" />} count={records.length}>
        <Card.Title>Recent activity</Card.Title>
      </Card.Header>
      {/* A TEMPLATE ON THE LIST, never on the row: a track list set per row
          lines nothing up, and a feed a reader scans down is read as columns
          or not at all. */}
      <List template="auto minmax(0, 1fr) auto">
        {records.map((record) => (
          <List.Item
            key={record.id}
            leading={<Tag appearance="outline">{humanize(record.kind)}</Tag>}
            trailing={<RelativeTime value={record.at} now={now} />}
          >
            <span className="row gap-2">
              {record.subject_key && (
                <a className="mono" href={href(["work", record.subject_key])}>
                  {record.subject_key}
                </a>
              )}
              <span className="truncate">{describeChange(record)}</span>
              <span className="muted nowrap">{record.actor || "the engine"}</span>
            </span>
          </List.Item>
        ))}
      </List>
    </Card>
  );
}

/** One commit in a sentence.
 *
 *  FROM THE DELTAS where there are any, because "todo to in_progress" is what
 *  every reader of a change wants and the kind alone does not say it. The
 *  excerpt is the fallback, and the kind is the last resort: a row with
 *  neither is still a row, and rendering it blank would make a real commit
 *  look like a rendering bug.
 *
 *  THE ABSENT SIDE OF A DELTA IS A MARK rather than nothing, because
 *  "assignee:  → ada" reads as a rendering bug where a marked absence reads
 *  as an assignment. It is a STRING rather than an `EmptyValue`, so the mark
 *  is a character: this is one sentence a feed row renders, a tooltip carries
 *  and a suite compares, not a node tree. */
export function describeChange(record: WorkActivityRecord): string {
  const moved = Object.entries(record.fields ?? {});
  if (moved.length > 0) {
    return moved.map(([field, d]) => `${field}: ${d.from || "—"} → ${d.to || "—"}`).join(", ");
  }
  if (record.excerpt) return record.excerpt;
  return record.kind.replaceAll("_", " ");
}

/** The project filter's options: the COMPANY's own listing, falling back to
 *  whatever is on the page.
 *
 *  The listing is the honest set: a filter built from the rows can only offer
 *  the projects that happen to be on them, so a board already narrowed to one
 *  offered exactly one choice and no way back. The fallback is not decoration:
 *  the listing is a separate poll, and a filter that empties while it is in
 *  flight is a control that flickers every time the board is re-read. */
export function projectKeys(listed: WorkProjectRow[] | undefined, shown: WorkSummary[]): string[] {
  if (listed && listed.length > 0) return listed.map((p) => p.key);
  const keys = new Set<string>();
  for (const item of shown) keys.add(item.project);
  return [...keys].sort();
}

/**
 * EVERY ROW ON SCREEN, grouped or flat.
 *
 * A grouped answer carries NO flat `items` by construction: `groups` replaces
 * them, because returning both would be the same rows twice, so everything
 * derived from `items` alone reported a fully populated board as empty. The
 * header read 0 shown, 0 in progress, 0 blocked, and the panel at the foot of
 * the screen drew "No work has been filed yet" underneath the board's own
 * columns.
 *
 * The top-level rows are enough and subgroup rows are deliberately NOT added:
 * a subgroup's rows are a slice of its own column's, so folding them in would
 * count the same task twice.
 */
export function shownRows(items: WorkSummary[], groups: WorkGroup[]): WorkSummary[] {
  if (groups.length === 0) return items;
  return groups.flatMap((group) => group.rows);
}

/** One item: its description, its thread and everything that moved it. */
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
      <PageHeader
        title={item?.key || id || "Item"}
        description={item?.title}
        badges={
          item ? (
            <>
              <Tag variant={STATUS_TONE[item.status] ?? "neutral"} dot>
                {STATUSES.find((s) => s.value === item.status)?.label ?? item.status}
              </Tag>
              {data?.blocked && (
                <Tag variant="danger" appearance="outline">
                  Blocked
                </Tag>
              )}
              {item.type && <Tag appearance="outline">{item.type}</Tag>}
              {item.project && <Tag appearance="outline">{item.project}</Tag>}
            </>
          ) : undefined
        }
      />

      {loading && <Skeleton label="Loading this item" variant="text" rows={8} />}

      <QueryState error={error} loading={loading}>
        {item && (
          <>
            <Coverage answer={data} />
            <Card as="section" padding="none">
              <Card.Header divided icon={<DescriptionGlyph size="sm" />}>
                <Card.Title>Description</Card.Title>
              </Card.Header>
              <Card.Body>
                {item.body ? (
                  <div className="prose">{item.body}</div>
                ) : (
                  <span className="muted">No description was written.</span>
                )}
              </Card.Body>
            </Card>

            <Card as="section" padding="none">
              <Card.Header divided icon={<PersonGlyph size="sm" />}>
                <Card.Title>Ownership</Card.Title>
              </Card.Header>
              <Card.Body>
                <Stack gap={3}>
                  <StatGroup columns={3}>
                    <StatCard
                      label="Assignee"
                      value={item.assignee ? seatName(item.assignee) : "Unassigned"}
                    />
                    <StatCard
                      label="Reporter"
                      value={
                        item.reporter ? (
                          seatName(item.reporter)
                        ) : (
                          <EmptyValue label="Not recorded" />
                        )
                      }
                    />
                    {/* THE REASSIGNMENT COUNT IS A BUDGET, not trivia: an item
                        handed on too many times has stopped being work and
                        started being a hot potato, and the engine refuses the
                        next hand-off rather than letting it circle. */}
                    <StatCard
                      label="Hand-offs"
                      value={item.reassignments ?? 0}
                      sub="each reassignment spends the item's own budget"
                      {...((item.reassignments ?? 0) >= 6 ? { tone: "warning" as const } : {})}
                    />
                  </StatGroup>
                  {item.watchers?.length ? (
                    <div className="row wrap gap-2">
                      <span className="muted">Watching:</span>
                      {item.watchers.map((w) => (
                        <SeatChip key={w} name={seatName(w)} handle={w} />
                      ))}
                    </div>
                  ) : null}
                </Stack>
              </Card.Body>
            </Card>

            {data.links?.length ? (
              <Card as="section" padding="none">
                <Card.Header divided icon={<LinkGlyph size="sm" />} count={data.links.length}>
                  <Card.Title>Links</Card.Title>
                </Card.Header>
                <List template="auto minmax(0, 1fr) auto">
                  {data.links.map((link) => (
                    <List.Item
                      key={`${link.kind}:${link.other}`}
                      leading={<Tag appearance="outline">{humanize(link.kind)}</Tag>}
                      {...(link.derived
                        ? {
                            // The DERIVED half is the one nobody authored: an
                            // editor has to change the other end.
                            trailing: <span className="muted">the other end authored this</span>,
                          }
                        : {})}
                    >
                      <span className="row gap-2">
                        <a href={href(["work", link.key || link.other])} className="mono">
                          {link.key || link.other}
                        </a>
                        <span className="truncate">{link.title}</span>
                      </span>
                    </List.Item>
                  ))}
                </List>
              </Card>
            ) : null}

            <Card as="section" padding="none">
              <Card.Header
                divided
                icon={<ChatGlyph size="sm" />}
                count={data.comments?.length ?? 0}
              >
                <Card.Title>Thread</Card.Title>
              </Card.Header>
              {data.comments?.length ? (
                <Stack gap={0}>
                  {data.comments.map((c) => (
                    <div key={c.id} className="thread-entry">
                      <div className="row gap-2">
                        <SeatChip name={seatName(c.author)} handle={c.author} />
                        <RelativeTime className="t-caption" value={c.created_at} now={now} />
                        {c.updated_at && <span className="muted">(edited)</span>}
                        {c.resolved && <Tag variant="success">Resolved</Tag>}
                      </div>
                      <div className="prose">{c.body}</div>
                    </div>
                  ))}
                </Stack>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<ChatGlyph />}
                  title="Nobody has commented"
                  description="A seat comments with comment_on_work_item, and a person through the MCP surface. Both are attributed."
                />
              )}
            </Card>

            <Card as="section" padding="none">
              <Card.Header
                divided
                icon={<TimelineGlyph size="sm" />}
                count={data.history?.length ?? 0}
              >
                <Card.Title>History</Card.Title>
              </Card.Header>
              {data.history?.length ? (
                <List template="auto minmax(0, 1fr) auto">
                  {data.history.map((change) => (
                    <List.Item
                      key={change.id}
                      leading={<TimelineGlyph size="sm" />}
                      trailing={<RelativeTime value={change.at} now={now} />}
                    >
                      <span className="row gap-2">
                        <span>{change.actor ? seatName(change.actor) : "the engine"}</span>
                        <span className="muted">{humanize(change.kind)}</span>
                        {/* The snapshot records what changed as VALUES, not as
                            from/to pairs: the applier writes the state the
                            change produced, and the previous value survives in
                            the entry before it. */}
                        {change.fields &&
                          Object.keys(change.fields).map((field) => (
                            <span key={field} className="muted">
                              {field}
                            </span>
                          ))}
                        {/* A COMMIT THAT ANNOUNCED NOTHING, one that carried no
                            notification at all. A fact about the change rather
                            than about its importance: a bulk edit is quiet by
                            construction. A commit WITHOUT this marker announced
                            something; whether it reached anybody is resolved
                            against the live roster at wake time and is not what
                            this says. */}
                        {change.quiet && <span className="muted">(quiet)</span>}
                      </span>
                    </List.Item>
                  ))}
                </List>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<TimelineGlyph />}
                  title="Nothing has moved this item yet"
                  description="Every commit against an item writes a row here, including the quiet ones that woke nobody."
                />
              )}
            </Card>
          </>
        )}
      </QueryState>
    </>
  );
}

/** The coverage half every tracker answer carries, a board's and a task's
 *  alike, which is why this is its own type rather than either answer's. */
interface CoverageFacts {
  read_level?: string;
  complete?: boolean;
  log_seq?: number;
  applied_through?: number;
  incomplete?: WorkIncomplete;
}

/**
 * How stale an answer may be, and what it could not account for.
 *
 * # Two different facts, and a screen that shows only one lies
 *
 * `read_level` says how FRESH the answer is, whether it came from this node's
 * rows as they stood, or from a position the caller's own write is at or
 * below. `complete` says whether the answer could account for everything it
 * was asked about: a node holding records this build cannot decode has rows
 * that may be missing, rows that should have left and may still be present,
 * and totals computed over the incomplete set.
 *
 * A screen that renders the freshness badge and swallows the coverage flag is
 * worse than a stale tile, because a person reads "a moment ago" and
 * concludes the board is right. So the incomplete line is a CALLOUT above the
 * rows rather than a badge beside them, it names the count and the scope, and
 * it says the remedy, which is a build that can read the records, not a
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
        <Callout
          variant="warning"
          icon={<WarningGlyph size="sm" />}
          title={
            answer.incomplete
              ? `This answer is incomplete: ${answer.incomplete.records} record(s) this build cannot read`
              : "This answer is incomplete"
          }
        >
          <span>
            Rows may be missing, rows that should have gone may still be here, and the counts were
            computed over what is shown.
            {answer.incomplete?.scope?.length
              ? ` Affected: ${answer.incomplete.scope.join(", ")}.`
              : ""}
            {answer.incomplete
              ? ` Record version ${answer.incomplete.version}, from sequence ${answer.incomplete.from.seq}. A build that can read it is what resolves this, not a refresh.`
              : ""}
          </span>
        </Callout>
      )}
      <div className="row gap-2">
        {answer.read_level && (
          <Tag
            appearance="outline"
            title="How fresh this answer is, as the engine actually served it"
          >
            {answer.read_level}
          </Tag>
        )}
        {/* APPLIED_THROUGH BESIDE SEQ, so a node holding something it cannot
            apply is visible as its own state rather than as lag. */}
        {behind && (
          <Tag variant="warning" appearance="outline">
            applied through {answer.applied_through} of {answer.log_seq}
          </Tag>
        )}
      </div>
    </>
  );
}

/**
 * The board's own count against the engine's hint.
 *
 * The hint is CAPPED by construction, since an exact total over an unbounded
 * set is the one query in this grammar that turns a poll into a scan, and the
 * ANSWER says whether it was, so this reads a flag rather than carrying a
 * copy of the engine's ceiling.
 */
function totalHint(hint: number, shown: number, capped?: boolean): string {
  if (hint <= 0) return "";
  // THE ENGINE SAYS WHETHER IT COUNTED THAT FAR, and the threshold is not
  // repeated here: comparing the hint against a ceiling of our own was a
  // second copy of the engine's, wrong at exactly one value, since a set of
  // exactly ten thousand is EXACT and was read as "10000+".
  if (capped) return `of ${hint}+ matching`;
  if (hint <= shown) return `of ${hint} matching`;
  return `of ${hint} matching, page through for the rest`;
}

/** scopeOf maps a view's status_group back onto the board's three segments.
 *
 *  THE SEGMENTS ARE A SHORTHAND for the two groups each names, so a view
 *  carrying exactly those groups lands on the segment rather than on "All"
 *  with an invisible filter. Anything else is "All": a view filtering to one
 *  group is not one of the three questions the control asks. */
function scopeOf(group: string | undefined): Scope {
  if (group === "not_started,active") return "open";
  if (group === "done,closed") return "closed";
  return "all";
}

/** viewTitle says whose a saved view is, which is the one thing its name
 *  cannot: two people's "My work" are two different views. */
function viewTitle(v: WorkView): string {
  const parts: string[] = [];
  if (v.owner) parts.push(`${v.owner}'s`);
  parts.push(v.type);
  if (v.protected) parts.push("(only its owner may change it)");
  return parts.join(" ");
}
