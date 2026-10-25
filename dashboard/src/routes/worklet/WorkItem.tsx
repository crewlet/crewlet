/**
 * One task, whole — as a page, and as the panel a list opens beside itself.
 *
 * # The page is the approved Issue artboard
 *
 * A reading column and a rail beside it, each its own scroller, flush with
 * the sheet. The column is the task in the order a person reads one: what it
 * is (title and description), what "done" means (its checklists), what it is
 * made of (its sub-tasks), and what has happened on it (its activity — the
 * changes, the conversation and every agent turn charged to it, with the one
 * running now at the foot and the box to write in under that). The rail is
 * its fields, its relations and pages, what it has cost in tokens and who
 * follows it.
 *
 * # Changed as the person reading it
 *
 * Every field, the title and description, a checklist tick, a sub-task, a
 * comment, an ask, a hand-off and following it are writes made as the
 * signed-in person (ADR-0024) — never hidden, and for a reader who cannot
 * make them, disabled with the one sentence that says why. The page's field
 * edits share one write and one refusal ([ItemEditsProvider]).
 *
 * # The page and the peek are one definition of what a task shows
 *
 * They differ in chrome and in nothing else. Written twice they would drift,
 * and the drift has a direction: the peek is the one somebody reads forty
 * times a day, so it would be the one that kept its fields while the page fell
 * behind. The peek heads itself with the task's identity (it has no page bar),
 * stacks the rail above the body in its one column, and leaves out what only
 * the page has room for: the cost and the record.
 *
 * # Opened by the person an ask names, without Worklet
 *
 * The engine answers `work_item` to a reader without Worklet while an open
 * ask on the task names them, so that they can answer it (and nothing else).
 * Every other read here (the project, the sub-tasks, the histories) is
 * Worklet's, so for such a reader none is asked: the task is drawn from its
 * own answer, its thread is the one that answer carried, and the ask in it is
 * answered on its row ([Activity]). The frame lets the task's address through
 * for exactly this reader (`App.tsx`'s `Screen`).
 */

import { useMemo } from "react";
import {
  ButtonLink,
  Callout,
  EmptyState,
  InlineCode,
  Menu,
  Skeleton,
  Tag,
  useMediaMatch,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  EllipsisGlyph,
  PackageGlyph,
  SquareKanbanGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage, type RowChrome } from "~/components/work.tsx";
import { RestoreButton } from "~/components/writes.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import {
  authorKinds,
  indexOrg,
  kindWithAuthors,
  statusOf,
  activityOf,
  seatResolvers,
} from "~/lib/seats.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { turnIdOf, watchHref } from "~/lib/turns.ts";
import { useNow } from "~/lib/clock.ts";
import { typeIcon, type LabelContext, detailItem, itemAddress, projectPath } from "~/lib/work.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { usePeekControls } from "~/app/frame/peekAddress.ts";
import { refToken } from "~/app/frame/objects.ts";
import { usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { ListPosition } from "./ListPosition.tsx";
import { ItemEditNote, ItemEditsProvider } from "./item/edit.tsx";
import { Checklists, ItemDescription, ItemTitle, Subtasks } from "./item/Body.tsx";
import { Activity } from "./item/Activity.tsx";
import { ItemRail } from "./item/Rail.tsx";
import type {
  AgentRow,
  WorkItemDetail,
  WorkProjectDetail,
  WorkSummary,
  WorkTombstone,
} from "~/protocol/index.ts";

/** How often a task and its sub-tasks are asked again: nothing pushes either. */
const ITEM_POLL_MS = 15_000;
const SUBTASKS_POLL_MS = 60_000;

/** What a reader who cannot change the task is told, once. */
const READ_ONLY = "The fields, checklists and description are read-only here.";

/**
 * The flags a task wears above its title, and nothing else.
 *
 * ONLY WHAT IS EXCEPTIONAL — something else is holding this one up, the record
 * has been archived, it is in the trash. The status is a FIELD rather than a
 * flag, because every task has one and a badge every task wears says nothing.
 *
 * IT TAKES THE DETAIL RATHER THAN THE TASK, because `blocked` is DERIVED and
 * lives on the answer (see [WorkItemDetail.blocked]).
 *
 * `undefined` rather than an empty element, so an ordinary task draws no row.
 */
export function itemFlags(detail: WorkItemDetail) {
  const item = detail.task;
  if (!detail.blocked && !item.archived && !item.removed) return undefined;
  return (
    <span className="row gap-1 task-flags">
      {/* IN THE TRASH, AND SAID FIRST. The detail read does not filter
          removed tasks, so a removed task's page resolves and answers exactly
          like a live one's — without this it rendered as an ordinary open
          task. */}
      {item.removed && <Tag variant="danger">In the trash</Tag>}
      {detail.blocked && (
        <Tag variant="danger" appearance="outline">
          Blocked
        </Tag>
      )}
      {item.archived && <Tag appearance="outline">Archived</Tag>}
    </span>
  );
}

/**
 * The removal, said where the reader is about to act on the task: who, when,
 * and whether it went with its container.
 */
export function RemovedNote({ tomb, now }: { tomb: WorkTombstone; now: number }) {
  return (
    <Callout variant="warning" icon={<TrashGlyph size="md" />}>
      <span>
        <strong>In the trash.</strong> Removed by {tomb.by || "somebody"}
        {tomb.kind ? ` (${tomb.kind})` : ""} {relTime(tomb.at, now)}
        {tomb.removed_with ? (
          <>
            {" "}
            along with <InlineCode>{tomb.removed_with}</InlineCode>
          </>
        ) : null}
        .
      </span>
    </Callout>
  );
}

/**
 * The chart's names and kinds, the project's vocabulary — and the kinds this
 * item's OWN writers were recorded under: a task filed through an operator
 * token has a reporter the chart does not list, drawn with a person's circle
 * because its writes say so ([kindWithAuthors]).
 */
function useItemChrome(
  index: OrgIndex,
  detail: WorkItemDetail | null | undefined,
  project: WorkProjectDetail | null | undefined,
): RowChrome & LabelContext {
  const authors = useMemo(
    () => authorKinds([...(detail?.history ?? []), ...(detail?.comments ?? [])]),
    [detail?.history, detail?.comments],
  );
  return useMemo(() => {
    const chart = seatResolvers(index);
    return {
      ...chart,
      seatKind: kindWithAuthors(chart.seatKind, authors),
      types: project?.types,
      statuses: project?.statuses,
      tags: project?.tags,
    };
  }, [index, authors, project]);
}

/**
 * The seat running a turn on this task now — joined on the item the engine
 * charges the turn to, never on a `work_key`, and only while the seat's own
 * state is `working`.
 */
export function liveOn(
  rows: readonly AgentRow[],
  item: { id: string; key: string },
): AgentRow | null {
  for (const row of rows) {
    if (row.activity !== "working") continue;
    const on = row.live_call?.work_item ?? row.turn?.work_item ?? null;
    if (on && (on.key === item.key || on.id === item.id)) return row;
  }
  return null;
}

/**
 * One task's reads: the task, its project's vocabulary and its sub-tasks —
 * the last two Worklet's, asked only of a reader holding it (see the
 * file's doc).
 */
function useTask(id: string) {
  const { worklet } = useViewer();
  const state = useQuery("work_item", { id }, { enabled: id !== "", pollMs: ITEM_POLL_MS });
  const item = state.data?.task;
  // ENABLED ON THE ITEM, not just parameterised by it: `undefined` params are
  // an EMPTY question on the wire rather than a skipped one, and the engine
  // refuses a project with no key.
  const project = useQuery("work_project", item ? { key: item.project } : undefined, {
    enabled: Boolean(item) && worklet,
    pollMs: 300_000,
  });
  // A SUBTREE IS ITS OWN QUESTION, and the one read that can see a task's
  // children. Every status is wanted: a done sub-task is a sub-task.
  const children = useQuery(
    "work_items",
    item ? { container: `project:${item.project}`, parent: item.id, limit: 100 } : undefined,
    { enabled: Boolean(item) && worklet, pollMs: SUBTASKS_POLL_MS },
  );
  return { state, item, project, children, worklet };
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function WorkItem({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const agents = useAgents();
  const now = useNow();
  const { state, item, project, children, worklet } = useTask(id);
  const chrome = useItemChrome(index, state.data, project.data);
  const { open: openPeek } = usePeekControls();
  const nav = useNavigator();
  // THE TASK AND ITS RAIL EACH SCROLL ON THEIR OWN, so the page takes the
  // window's height rather than growing — above a phone, where the two are one
  // column and the page scrolls as any other.
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  useFillScreen(!phone);
  const live = item ? liveOn(agents, item) : null;
  const statusFor = useMemo(() => {
    const byHandle = new Map(agents.map((row) => [row.handle ?? "", row]));
    return (handle: string) => statusOf(activityOf(byHandle.get(handle)));
  }, [agents]);

  // THE TRAIL IS THE PAGE BAR'S, keyed on the key rather than the uuid the
  // address may carry, and the project's crumb is its name.
  const projectName = project.data?.name ?? "";
  usePageLabels(
    item ? { [id]: item.key, ...(projectName ? { [item.project]: projectName } : {}) } : {},
  );
  const liveTurn = turnIdOf(live);
  // THE TASK'S ADDRESS, which every write, peek and step on this page names
  // it by: its key, or its id where another task claimed that key first.
  const address = state.data ? itemAddress(detailItem(state.data)) : "";

  // "THIS TASK, ON ITS OWN BOARD", with the rail open: the project is a path
  // and the task is the frame's `peek=` token.
  const openOnBoard = () => {
    if (item && address) {
      nav.to(projectPath(item.project), { peek: refToken({ kind: "item", id: address }) });
    }
  };
  // AND ON A PHONE IT IS IN THE BAR'S ONE "MORE", beside the star and the
  // link, rather than a second ellipsis next to the frame's.
  usePageMenu(
    item
      ? [
          {
            key: "board",
            label: "Open on the board",
            icon: <SquareKanbanGlyph size="sm" />,
            onSelect: openOnBoard,
          },
        ]
      : [],
  );

  return (
    <>
      <PageActions>
        {/* WHERE THIS TASK SITS IN THE LIST IT WAS OPENED FROM. */}
        {item && address && worklet && <ListPosition address={address} />}
        {/* WATCH LIVE ONLY WHILE A TURN IS RUNNING ON THIS TASK. */}
        {live && liveTurn && (
          <ButtonLink
            size="small"
            variant="secondary"
            leadingIcon={<ActivityGlyph size="sm" />}
            href={watchHref(liveTurn)}
          >
            Watch live
          </ButtonLink>
        )}
        {/* A task in the trash is offered the way back. */}
        {item?.removed && address && (
          <RestoreButton key={item.id} item={address} label={item.key} />
        )}
        {item && (
          <span className="page-action-folds">
            <Menu
              label={`More for ${item.key}`}
              icon={<EllipsisGlyph size="sm" />}
              align="end"
              items={[
                {
                  key: "board",
                  label: "Open on the board",
                  icon: <SquareKanbanGlyph size="sm" />,
                  onSelect: openOnBoard,
                },
              ]}
            />
          </span>
        )}
      </PageActions>

      {state.loading && !state.data && (
        <Skeleton variant="text" rows={8} label="Loading the task" />
      )}
      {/* A MISSING TASK IS AN EMPTY STATE WITH A WAY BACK, as a missing
          project and a missing artifact are, never a full-width grey line. */}
      {state.error === "not_found" ? (
        <EmptyState
          icon={<SquareKanbanGlyph size="xl" />}
          title={`No task called “${id}”`}
          description="It may have been removed, or the link names a key that never existed."
          action={
            <ButtonLink variant="secondary" href={href(["worklet", "tasks"])}>
              Back to Tasks
            </ButtonLink>
          }
        />
      ) : null}
      <QueryState
        error={state.error === "not_found" ? null : state.error}
        refusal={state.refusal}
        loading={state.loading}
      >
        {state.data && item && (
          <ItemEditsProvider
            item={address}
            label={item.key}
            version={item.version}
            seatName={chrome.seatName}
          >
            <div className="task-frame">
              <div className="task-main">
                <div className="task-col">
                  {itemFlags(state.data)}
                  {item.removed && <RemovedNote tomb={item.removed} now={now} />}
                  <Coverage answer={state.data} />
                  <ItemEditNote readOnly={READ_ONLY} />
                  <ItemTitle item={item} />
                  <ItemDescription item={item} />
                  <Checklists item={item} chrome={chrome} />
                  {worklet && (
                    <Subtasks
                      rows={children.data?.items ?? []}
                      error={children.error}
                      refusal={children.refusal}
                      chrome={chrome}
                      parent={item.removed ? undefined : item}
                      statusOf={statusFor}
                      peek={(row: WorkSummary) => openPeek({ kind: "item", id: itemAddress(row) })}
                    />
                  )}
                  <Activity
                    item={item}
                    address={address}
                    chrome={chrome}
                    index={index}
                    now={now}
                    live={live}
                    {...(state.data.comments ? { thread: state.data.comments } : {})}
                  />
                </div>
              </div>
              <aside className="task-rail" aria-label={`Properties of ${item.key}`}>
                <ItemRail detail={state.data} chrome={chrome} project={project.data} now={now} />
              </aside>
            </div>
          </ItemEditsProvider>
        )}
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The peek
// ---------------------------------------------------------------------------

/**
 * The same task, beside the list it was opened from.
 *
 * IT KEEPS THE READER WHERE THEY ARE — a board is scanned, and sending somebody
 * to a page and back to read one description is the navigation every tracker
 * learned not to make. The chrome (the panel, its grip, the way out to the page
 * and the close control) is the frame's, `app/frame/PeekHost.tsx`.
 *
 * IT RESOLVES ITS OWN PEOPLE: the frame mounts it from a `peek=` token knowing
 * nothing about an org chart, and the chart is a store read.
 */
export function ItemPeek({ itemKey }: { itemKey: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const agents = useAgents();
  const now = useNow();
  const { state, item, project, children, worklet } = useTask(itemKey);
  const chrome = useItemChrome(index, state.data, project.data);
  const live = item ? liveOn(agents, item) : null;
  // THE ADDRESS ITS WRITES NAME, as the page's — see [WorkItem].
  const address = state.data ? itemAddress(detailItem(state.data)) : "";

  return (
    <>
      {state.loading && !state.data && (
        <Skeleton variant="text" rows={6} label="Loading the task" />
      )}
      <QueryState error={state.error} refusal={state.refusal} loading={state.loading}>
        {state.data &&
          (item ? (
            <ItemEditsProvider
              item={address}
              label={item.key}
              version={item.version}
              seatName={chrome.seatName}
            >
              {/* IDENTITY AND STATE MARKS ONLY: the rail below is in the SAME
                  column and states every field once. */}
              <ObjectHeader
                size="peek"
                kind="Task"
                icon={typeIcon(item.type)}
                identifier={item.key}
                title={item.title}
                status={itemFlags(state.data)}
              />
              <div className="col gap-3 task-peek">
                {item.removed && <RemovedNote tomb={item.removed} now={now} />}
                <Coverage answer={state.data} />
                <ItemEditNote readOnly={READ_ONLY} />
                <ItemRail
                  detail={state.data}
                  chrome={chrome}
                  project={project.data}
                  now={now}
                  compact
                />
                <ItemDescription item={item} flush />
                <Checklists item={item} chrome={chrome} />
                {/* A CHILD NAVIGATES FROM THE PEEK rather than replacing its
                    parent in it: `[` and `]` step the list the peek was
                    opened from, which this task is in and its child is not. */}
                {worklet && (
                  <Subtasks
                    rows={children.data?.items ?? []}
                    error={children.error}
                    refusal={children.refusal}
                    chrome={chrome}
                  />
                )}
                <Activity
                  item={item}
                  address={address}
                  chrome={chrome}
                  index={index}
                  now={now}
                  live={live}
                  param="peek_activity"
                  {...(state.data.comments ? { thread: state.data.comments } : {})}
                />
              </div>
            </ItemEditsProvider>
          ) : (
            // AN ANSWER CARRYING NO TASK is what a newer build can send (this
            // one answers not_found); the honest reply names the address.
            <EmptyState
              size="compact"
              icon={<PackageGlyph size="xl" />}
              title={`Nothing answers to “${itemKey}”`}
              description="It may have been removed."
            />
          ))}
      </QueryState>
    </>
  );
}
