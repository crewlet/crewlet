/**
 * One person's day: the seven claims on their attention, and nothing else.
 *
 * # Seven blocks, not one list
 *
 * A person who saw only their assignments would miss six other things asking
 * for their time: what a lead put at the top of their list, the questions
 * waiting on an answer, the sub-items they claimed on somebody else's task,
 * the work they were brought onto without owning, what moved on what they
 * follow, and what became workable while they were not looking. Each is a
 * different claim, and folding them into one list is how six of them go
 * unnoticed.
 *
 * # Every block is bounded the same
 *
 * Twenty rows each, so no block can crowd out another. It is the same rule the
 * engine's own answer follows, for the same reason: this is read as one page,
 * and a person with two hundred assignments would otherwise never see their
 * asks. How many a block holds is its HEADER's count, above the rows, so a
 * block that pages still says what it holds rather than what fits.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes. A priority list is reordered through the engine's own
 * write path, attributed to whoever did it; a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, RecordTable } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { tsKey } from "~/lib/format.ts";
import type { WorkAskRow, WorkChecklistRow, WorkSummary } from "~/protocol/index.ts";
import {
  Callout,
  Card,
  EmptyState,
  PageHeader,
  RelativeTime,
  Select,
  StatCard,
  StatGroup,
  Tag,
  Toolbar,
  useNow,
} from "@crewlethq/ui";
import {
  CheckCircleGlyph,
  FlagGlyph,
  GroupGlyph,
  HelpGlyph,
  InboxGlyph,
  ListGlyph,
  PersonGlyph,
  VisibilityGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * How often the day is re-read, in ms.
 *
 * There is no push behind this answer: it is assembled per request from the
 * work projection, so the screen has to ask. Half a minute is the cadence a
 * person leaving this page open expects of a queue somebody else fills, and
 * it is slow enough that seven blocks of twenty rows are not re-assembled
 * every few seconds for a reader who has walked away.
 */
const POLL_MS = 30_000;

export function MyWork() {
  const org = useOrg();
  const now = useNow();
  const [handle, setHandle] = useParam("handle", "");
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be reached
  // with nobody chosen and still offer somebody.
  //
  // A SEAT WITH NO HANDLE IS NOT ONE OF THEM. The engine's answer is keyed by
  // handle and refuses the question without one, and the projection reports ""
  // wherever it carries no derived hierarchy, so an option for such a seat is
  // an option that can never load a day. Worse, an empty handle sorts first
  // and became the screen's own default, which left it showing "Nobody chosen"
  // on a company that has seats.
  const people = useMemo(
    () =>
      indexOrg(org)
        .seats.filter((seat) => seat.handle !== "")
        // The NAME under the handle, which is what the listbox can carry and a
        // platform dropdown cannot: a handle on its own is an identifier, and
        // the question this screen asks is about a person.
        .map((seat) => ({ value: seat.handle, label: seat.handle, description: seat.name }))
        .sort((a, b) => a.value.localeCompare(b.value)),
    [org],
  );
  const whose = handle || people[0]?.value || "";
  // NOT UNTIL SOMEBODY IS CHOSEN, the same guard the board and the sprint
  // report take. `whose` is empty until the chart has loaded, and the engine
  // refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: POLL_MS,
  });
  const mine = state.data;

  return (
    <>
      <PageHeader
        title="My work"
        description="Everything one person is expected to look at: their priorities, what they hold, the questions waiting on them, and what became workable while they were away."
      />

      {/* A GROUP RATHER THAN A TOOLBAR, like every other filter row here: the
          control names the screen's subject rather than doing something to it,
          and it keeps its own tab stop instead of being one arrow-walked row of
          one. */}
      <Toolbar label="Whose work" mode="group" sticky>
        <Select
          value={whose}
          onChange={(next) => setHandle(String(next))}
          ariaLabel="Whose"
          placeholder="Pick somebody"
          options={people}
          width="auto"
          // A COMPANY'S SEAT COUNT IS UNBOUNDED, and this list is every one of
          // them. Type-ahead alone walks a list nobody can see the end of, so
          // the picker carries the search the listbox already has.
          searchable
          searchPlaceholder="Filter by handle or name"
          emptyMessage="No seat matches that"
        />
      </Toolbar>

      {!whose && (
        <EmptyState
          icon={<PersonGlyph />}
          title="Nobody chosen"
          // TWO DIFFERENT EMPTIES, said apart. A company with seats is waiting
          // for a choice; a company whose seats carry no handle cannot be
          // asked this question at all, and telling that reader to pick
          // somebody points them at a list with nothing in it.
          description={
            people.length > 0
              ? "A day belongs to somebody. Pick a seat above."
              : "A day belongs to somebody, and no seat in this company reports a handle to ask about. The engine derives handles from the company configuration."
          }
        />
      )}

      {whose && (
        <QueryState error={state.error} loading={state.loading}>
          {mine && (
            <>
              {/* COVERAGE IS SAID OUT LOUD, never swallowed: rows may be
                  missing, and a day rendered as complete when it is not is a
                  person who thinks they are done. */}
              {mine.complete === false && (
                <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
                  <span>
                    This node could not account for every change yet, so a block may be short.
                  </span>
                </Callout>
              )}
              <StatGroup columns={4}>
                <StatCard
                  icon={<FlagGlyph />}
                  label="Priorities"
                  value={mine.priorities.length}
                  sub="in the stored order"
                />
                <StatCard
                  icon={<InboxGlyph />}
                  label="Assigned"
                  value={mine.assigned.length}
                  sub="held by this person"
                />
                <StatCard
                  // THE ONE TILE THAT CHANGES ITS MARK. Every other block is
                  // this person's own queue; an unanswered question is
                  // somebody else waiting on them, so the tile carries the
                  // warning mark exactly while there is one and the ordinary
                  // question mark when there is not.
                  icon={mine.asked_of_me.length ? <WarningGlyph /> : <HelpGlyph />}
                  label="Asked"
                  value={mine.asked_of_me.length}
                  sub="waiting on an answer"
                />
                <StatCard
                  icon={<CheckCircleGlyph />}
                  label="Unblocked"
                  value={mine.unblocked_recent.length}
                  sub="newly workable"
                />
              </StatGroup>

              <Asks rows={mine.asked_of_me} now={now} />
              <TaskBlock
                table="priorities"
                title="Priorities"
                hint="What somebody put at the top of this list, in the order they put it."
                icon={<FlagGlyph size="sm" />}
                rows={mine.priorities}
                now={now}
              />
              <TaskBlock
                table="assigned"
                title="Assigned"
                icon={<InboxGlyph size="sm" />}
                rows={mine.assigned}
                now={now}
              />
              <TaskBlock
                table="unblocked"
                title="Unblocked"
                hint="Work whose blockers have all finished, the one block about a change rather than a state."
                icon={<CheckCircleGlyph size="sm" />}
                rows={mine.unblocked_recent}
                now={now}
              />
              <Checklist rows={mine.checklist_items} />
              <TaskBlock
                table="collaborating"
                title="Collaborating"
                hint="Brought on without owning."
                icon={<GroupGlyph size="sm" />}
                rows={mine.collaborating}
                now={now}
              />
              <TaskBlock
                table="watching"
                title="Watching"
                icon={<VisibilityGlyph size="sm" />}
                rows={mine.watching_recent}
                now={now}
              />
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/**
 * A block of tasks, ABSENT when it is empty rather than drawn as a shrug: a
 * page of seven "nothing here" panels buries the one that has something.
 *
 * THE ROWS ARE NOT SORTED BY THIS SCREEN, and that is load-bearing for
 * Priorities above all: the order IS the content there, because it is what
 * somebody decided, and a default sort would discard the decision. Every
 * column is sortable, so a reader can still ask a different question of the
 * block and get back to the stored order by clearing it.
 */
export function TaskBlock({
  table,
  title,
  hint,
  icon,
  rows,
  now,
}: {
  /** Which table on this screen. It prefixes the choices kept in the URL. */
  table: string;
  title: string;
  hint?: string;
  icon: ReactNode;
  rows: WorkSummary[];
  now: number;
}) {
  if (rows.length === 0) return null;
  return (
    <Card as="section" padding="none">
      <Card.Header divided icon={icon} count={rows.length} subtitle={hint}>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <RecordTable
        screen="my-work"
        table={table}
        rows={rows}
        getRowKey={(row) => row.id}
        getRowHref={(row) => href(["work", row.key])}
        // A LIVE TABLE. The day is re-read on a timer, so a block a reader has
        // ordered by one of its columns would otherwise re-rank underneath
        // them between two glances at it.
        stableOrder
        columns={[
          {
            key: "key",
            header: "Key",
            mono: true,
            shrink: true,
            sortable: true,
            // WHICH TASK cannot be hidden: every other cell here is a fact
            // about a task, and a row of them naming nothing is a row nobody
            // can act on.
            hideable: false,
            sortValue: (row) => row.key,
            render: (row) => row.key,
          },
          {
            key: "title",
            header: "Title",
            sortable: true,
            sortValue: (row) => row.title,
            render: (row) => <span className="truncate">{row.title}</span>,
          },
          {
            key: "status",
            header: "Status",
            shrink: true,
            sortable: true,
            sortValue: (row) => row.status,
            render: (row) => <Tag appearance="outline">{row.status}</Tag>,
          },
          {
            key: "updated",
            header: "Updated",
            shrink: true,
            sortable: true,
            // THE RECENT END FIRST. A press on a time column is a reader
            // asking what moved, so the first one answers with the newest;
            // ascending would hand them the oldest row in the block.
            firstDirection: "desc",
            // THROUGH `tsKey`, never the raw string. Timestamps arrive in two
            // encodings for the same instant and a string compare orders the
            // later one first; see the note at the top of `lib/format.ts`.
            sortValue: (row) => tsKey(row.updated),
            render: (row) => <RelativeTime className="t-caption" value={row.updated} now={now} />,
          },
        ]}
      />
    </Card>
  );
}

/**
 * The questions waiting on this person.
 *
 * FIRST on the page, because an unanswered question is the only block where
 * somebody else is blocked on THIS person rather than the other way round.
 */
export function Asks({ rows, now }: { rows: WorkAskRow[]; now: number }) {
  if (rows.length === 0) return null;
  return (
    <Card as="section" padding="none">
      {/* THE WARNING MARK, and it is honest here rather than decorative: this
          block is drawn only when somebody is actually waiting. */}
      <Card.Header divided icon={<WarningGlyph size="sm" />} count={rows.length}>
        <Card.Title>Asked of you</Card.Title>
      </Card.Header>
      <RecordTable
        screen="my-work"
        table="asks"
        rows={rows}
        getRowKey={(ask) => ask.comment}
        getRowHref={(ask) => href(["work", ask.key])}
        stableOrder
        columns={[
          {
            key: "key",
            header: "Key",
            mono: true,
            shrink: true,
            sortable: true,
            hideable: false,
            sortValue: (ask) => ask.key,
            render: (ask) => ask.key,
          },
          {
            key: "body",
            header: "Question",
            sortable: true,
            sortValue: (ask) => ask.body,
            // ONE LINE OF IT. The whole comment is on the task the row opens;
            // a question that ran to four lines would push the rest of the
            // block off the page it is meant to be read beside.
            render: (ask) => <span className="truncate">{ask.body}</span>,
          },
          {
            key: "asked_by",
            header: "Asked by",
            shrink: true,
            sortable: true,
            sortValue: (ask) => ask.asked_by,
            render: (ask) => ask.asked_by,
          },
          {
            key: "asked_at",
            header: "Asked",
            shrink: true,
            sortable: true,
            firstDirection: "desc",
            sortValue: (ask) => tsKey(ask.asked_at),
            render: (ask) => <RelativeTime className="t-caption" value={ask.asked_at} now={now} />,
          },
        ]}
      />
    </Card>
  );
}

/**
 * Sub-items claimed on somebody else's task.
 *
 * Its own block because no assignee filter over tasks reaches one: a person
 * holding six checklist items and no assignment reads their queue as empty.
 */
export function Checklist({ rows }: { rows: WorkChecklistRow[] }) {
  if (rows.length === 0) return null;
  return (
    <Card as="section" padding="none">
      <Card.Header
        divided
        icon={<ListGlyph size="sm" />}
        count={rows.length}
        subtitle="On other people's tasks."
      >
        <Card.Title>Checklist items</Card.Title>
      </Card.Header>
      <RecordTable
        screen="my-work"
        table="checklist"
        rows={rows}
        getRowKey={(item) => `${item.task}:${item.item}`}
        getRowHref={(item) => href(["work", item.task_key])}
        stableOrder
        columns={[
          {
            key: "task_key",
            header: "Task",
            mono: true,
            shrink: true,
            sortable: true,
            hideable: false,
            sortValue: (item) => item.task_key,
            render: (item) => item.task_key,
          },
          {
            key: "name",
            header: "Item",
            sortable: true,
            sortValue: (item) => item.name,
            render: (item) => <span className="truncate">{item.name}</span>,
          },
          {
            key: "task_title",
            header: "On",
            sortable: true,
            sortValue: (item) => item.task_title,
            render: (item) => <span className="truncate">{item.task_title}</span>,
          },
        ]}
      />
    </Card>
  );
}
