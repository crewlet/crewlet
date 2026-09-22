/**
 * One task, whole — as a page, and as the panel the board opens beside itself.
 *
 * # The page and the peek are one definition of what a task shows
 *
 * They differ in chrome and in nothing else. Written twice they would drift,
 * and the drift has a direction: the peek is the one somebody reads forty
 * times a day, so it would be the one that kept its fields while the page
 * quietly fell behind — and a reader who clicked through from the panel to the
 * page to see "the whole thing" would find less than they started with.
 *
 * That is now true of the HEADER as well as of the body: both frames open with
 * the same [ObjectHeader] over the same [itemFacts], so the status, priority,
 * type, assignee and due date are read in one order wherever the task
 * appears. The page had no title at all before it — it published a status
 * badge into the page bar and went straight to its panels, so the one screen
 * about one task never said which task it was about.
 *
 * # Read-only, like the board it opens from
 *
 * A task is moved by a seat's own tools or by an operator through MCP, both
 * attributed to somebody. A button here would write as "the dashboard", which
 * is not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { renderMarkdown } from "~/lib/markdown.ts";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  AsksTag,
  Assignee,
  Coverage,
  DueMark,
  PriorityMark,
  RowList,
  StatusBadge,
  TypeIcon,
  type RowChrome,
} from "~/components/work.tsx";
import { Callout, Card, EmptyState, InlineCode, Skeleton, Tabs, Tag } from "@crewlethq/ui";
import {
  DeleteGlyph,
  ArrowForwardGlyph,
  CheckGlyph,
  ChatGlyph,
  HelpGlyph,
  GroupGlyph,
  LinkGlyph,
  NotificationsGlyph,
  Package2Glyph,
  RemoveGlyph,
  ScheduleGlyph,
  TimelineGlyph,
  TuneGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { Mark } from "~/ui/glyph.tsx";
import { plainText } from "~/lib/markdown.ts";
import { fmtDate, fmtDateTime, fmtCount, fmtDuration, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { reasonAbout } from "~/lib/reasons.ts";
import {
  changeMark,
  describeHistory,
  fieldValueState,
  fieldValueText,
  fmtMinutes,
  typeIcon,
  typeName,
} from "~/lib/work.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ToolCallBlock } from "~/components/ToolCall.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import type { PropertyGroup } from "~/app/frame/PropertiesRail.tsx";
import { ObjectHeader, type Fact, type SetBy } from "~/app/frame/ObjectHeader.tsx";
import { usePeekControls } from "~/app/frame/DetailRail.tsx";
import { pathOf, refToken } from "~/app/frame/objects.ts";
import { attribution, type ChangeField } from "~/lib/attribution.ts";
import type {
  WorkChange,
  WorkComment,
  WorkFieldDef,
  WorkItemDetail,
  WorkLink,
  WorkProjectDetail,
  WorkRoutingAnswer,
  WorkSummary,
  WorkTombstone,
} from "~/protocol/index.ts";

/**
 * What a relation is CALLED from this end.
 *
 * The derived half of a dependency is the one nobody authored — `waiting_on`
 * mirrored onto the blocker — and calling both ends "waiting on" would tell a
 * reader their task is blocked by the very task it is blocking. So the word
 * comes from the pair (kind, derived) rather than from the kind alone.
 */
function linkHeading(link: WorkLink): string {
  switch (link.kind) {
    case "waiting_on":
      return link.derived ? "Blocks" : "Waiting on";
    case "duplicates":
      return link.derived ? "Duplicated by" : "Duplicates";
    case "page":
      return "Pages";
    default:
      return "Linked";
  }
}

/**
 * The flags a task wears beside its title, and nothing else.
 *
 * ONLY WHAT IS EXCEPTIONAL — something else is holding this one up, and the
 * record has been archived. The status itself is a FACT rather than a flag,
 * because every task has one and a badge every task wears says nothing; these
 * two are drawn only for the tasks they are true of, which is what makes them
 * worth the reader's eye.
 *
 * IT TAKES THE DETAIL RATHER THAN THE TASK, because `blocked` is DERIVED and
 * lives on the answer: whether any dependency is still open is a fact about
 * other rows, computed in the same transaction as this one — see
 * [WorkItemDetail.blocked]. A task carries its links, never their states.
 *
 * `undefined` rather than an empty element, because an empty child still takes
 * its gap in the header row: an ordinary task would sit with a hole beside its
 * title where a state it is not in would have been.
 */
export function itemFlags(detail: WorkItemDetail): ReactNode {
  const item = detail.task;
  if (!detail.blocked && !item.archived && !item.removed) return undefined;
  return (
    <span className="row gap-1">
      {/* IN THE TRASH, AND SAID FIRST. The detail read does not filter removed
          tasks, so a removed task's page resolves and answers exactly like a
          live one's — and the wire has always carried the tombstone while this
          screen had no field for it, so a task somebody deleted rendered as an
          ordinary open task. A reader could comment on it, wonder why it was
          on no board, and never be told. */}
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
 * The removal, said in full where the reader is about to act on the task.
 *
 * A TAG IN THE HEADER SAYS WHICH STATE; this says who, when, and whether the
 * task went on its own or with its container — `removed_with` names the
 * parent whose removal took this one along, which is the difference between
 * somebody deleting this task and somebody deleting the epic above it.
 *
 * AND IT SAYS THE REMOVAL IS REVERSIBLE, because that is the fact that decides
 * what a reader does next: there is no window and nothing is destroyed, so
 * this is a state rather than the end of the record.
 */
export function RemovedNote({ tomb, now }: { tomb: WorkTombstone; now: number }) {
  return (
    <Callout variant="warning" icon={<DeleteGlyph size="md" />}>
      <span>
        <strong>This is in the trash.</strong> {tomb.by || "somebody"}
        {tomb.kind ? ` (${tomb.kind})` : ""} removed it {relTime(tomb.at, now)}
        {tomb.removed_with ? (
          <>
            {" "}
            along with <InlineCode>{tomb.removed_with}</InlineCode>
          </>
        ) : null}
        . Removal is reversible at any age — nothing is destroyed, and the call below brings it
        back.
      </span>
    </Callout>
  );
}

/**
 * The six facts a task is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today. The page and
 * the peek are one definition of what a task shows — see this file's own doc —
 * and a header written twice is two orders as soon as somebody adds a seventh
 * fact, with the drift landing on the page nobody reads forty times a day.
 *
 * THEY ARE THE PROPERTIES RAIL'S OWN LEADING ROWS, in the rail's own order:
 * status, priority and type out of State, the assignee out of People, the due
 * date out of Plan. A reader who scans the header and then the rail below it
 * is reading one object rather than re-learning it.
 *
 * NO `setBy` HERE, deliberately, although a [Fact] carries one: who last moved
 * a field is what the rail answers row by row, and the same attribution drawn
 * twice on one screen would put a second, quieter copy of the history under a
 * line that exists to be scanned in a second.
 *
 * AN ABSENT PRIORITY, TYPE OR DUE DATE IS DROPPED rather than dashed — a
 * [FactLine] renders no line for an undefined value — because a fresh task has
 * none of the three and a header of em dashes says less than a shorter one.
 * The assignee is the exception and is always drawn: nobody holding a task is
 * an answer a reader comes to this line for, stated in the rail's own words.
 */
function itemFacts({
  detail,
  chrome,
  now,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  now: number;
}): Fact[] {
  const item = detail.task;
  const seatName = chrome.seatName ?? ((handle: string) => handle);
  return [
    { label: "Status", value: <StatusBadge status={item.status} defs={chrome.statuses} /> },
    {
      label: "Priority",
      // THE RAIL'S OWN CONDITION. `none` is the absence of a priority and
      // `normal` is a priority somebody chose, which is why the word is asked
      // for: a properties line answering "what is this set to" reads "normal"
      // where a board card draws nothing at all.
      value:
        item.priority && item.priority !== "none" ? (
          <PriorityMark priority={item.priority} word />
        ) : undefined,
    },
    {
      label: "Type",
      value: item.type ? (
        <span className="row gap-1">
          <TypeIcon type={item.type} types={chrome.types} decorative />
          {typeName(item.type, chrome.types)}
        </span>
      ) : undefined,
    },
    {
      label: "Assignee",
      // NOT LINKED THROUGH THE FACT'S OWN `path`: a [SeatChip] is already the
      // one way a person appears in a list, avatar and link together, and
      // wrapping it in a second anchor would nest one link inside another.
      value: item.assignee ? (
        <SeatChip name={seatName(item.assignee)} handle={item.assignee} />
      ) : (
        <span className="muted">Unassigned</span>
      ),
    },
    {
      label: "Due",
      // NO OVERDUE MARK HERE. A task carries no `overdue` — it is DERIVED per
      // row and lives on the row that carries it, precisely so no renderer
      // re-derives it differently and one screen calls a task overdue where
      // the next does not.
      value: item.due_at ? <DueMark due={item.due_at} now={now} /> : undefined,
    },
  ];
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function WorkItem({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const state = useQuery("work_item", { id }, { enabled: id !== "", pollMs: 15_000 });
  const item = state.data?.task;

  // THE PROJECT'S OWN VOCABULARY, because an item carries slugs and ids: a
  // status renders in the word the team uses, a type in the name the company
  // declared, and a custom field's value as the option's NAME rather than the
  // uuid it is stored under.
  // ENABLED ON THE ITEM, not just parameterised by it. `item` is absent until
  // `work_item` answers — every mount, and every poll that finds nothing — and
  // a params object that is `undefined` is an EMPTY one on the wire rather
  // than a skipped question, so without this the engine is asked for a project
  // with no key and refuses it with `bad_params` on the way in.
  const project = useQuery("work_project", item ? { key: item.project } : undefined, {
    enabled: Boolean(item),
    pollMs: 300_000,
  });

  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: project.data?.types,
    statuses: project.data?.statuses,
  };

  // THE TRAIL IS THE PAGE BAR'S. This screen drew its own — workspace, project,
  // key — above the title, so a reader had two breadcrumbs on one page,
  // disagreeing about where they were the moment either one changed.
  //
  // AND THE ADDRESSED SEGMENT IS OFTEN A UUID: `work_item` takes either
  // spelling and every internal link carries the id, so a trail reading
  // `work / 4f3c…` told a reader nothing they did not already have from the URL
  // bar. This is what the comment that stood here has always claimed was
  // happening and nothing was doing.
  //
  // THE KEY AND NOT THE TITLE: a breadcrumb is the ADDRESS rather than a
  // title, `ENG-42` is an address a person can read and paste, and the title
  // is on the header immediately below it. A crumb spends itself on a name
  // only where the address is a uuid nobody can read.
  usePageLabels(item ? { [id]: item.key } : {});

  return (
    <>
      {/* THE STATE IS THE HEADER'S, not the page bar's. The bar used to carry
          the status, the blocked flag, the type and the archived badge, and
          the header below now says all four — drawn in both places they are
          one state with two renderings, which is exactly what one
          [ObjectHeader] per object exists to end. What stays here is the way
          OUT, which the header has no room for and the bar is for. */}
      <PageActions>
        {item ? (
          // THE PROJECT IS A PATH AND THE TASK IS THE FRAME'S `peek=` TOKEN.
          // This carried `?project=&item=`, which are the two spellings the
          // screen retired when a project became an object with a page and
          // the rail moved into the frame — nothing reads either key any
          // more, so the one control that promised "this task, on its own
          // board" landed on the company-wide board with the rail shut.
          <a
            className="t-link"
            href={href(["work", item.project], {
              peek: refToken({ kind: "item", id: item.key }),
            })}
          >
            Open on the board →
          </a>
        ) : undefined}
      </PageActions>

      {state.loading && !state.data && (
        <Skeleton variant="text" rows={8} label="Loading the item" />
      )}

      <QueryState error={state.error} loading={state.loading}>
        {state.data && item && (
          <>
            <ObjectHeader
              kind="Item"
              // THE TASK'S OWN TYPE GLYPH in the eyebrow, and the type's WORD
              // in the facts below: the icon is what a reader recognises a bug
              // from across a board by, and it carries no label of its own.
              icon={typeIcon(item.type)}
              identifier={item.key}
              title={item.title}
              status={itemFlags(state.data)}
              facts={itemFacts({ detail: state.data, chrome, now })}
            />
            {item.removed && <RemovedNote tomb={item.removed} now={now} />}
            <Coverage answer={state.data} />
            <div className="work-item">
              <div className="work-item-main">
                <ItemBody detail={state.data} chrome={chrome} now={now} />
              </div>
              <div className="work-item-side">
                <Card>
                  <Card.Header icon={<TuneGlyph size="sm" />}>
                    <Card.Title>Properties</Card.Title>
                  </Card.Header>
                  <ItemProps detail={state.data} chrome={chrome} project={project.data} />
                </Card>
                <ItemLinks detail={state.data} chrome={chrome} />
              </div>
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The peek
// ---------------------------------------------------------------------------

/**
 * The same task, beside the board it was opened from.
 *
 * IT KEEPS THE READER WHERE THEY ARE, which is the whole reason it exists: a
 * board is scanned, and sending somebody to a page and back to read one
 * description is the navigation every tracker learned not to make.
 *
 * THE CHROME IS THE FRAME'S — see `app/frame/DetailRail.tsx`. This renders the
 * item's own content and nothing else: the panel, its resize grip, the way out
 * to the page and the close control are the same on every kind of object, and
 * a second copy of them here is how the tracker's peek came to be the only
 * peek in the product.
 *
 * AND THE HEADER IS THE PAGE'S, one size down — the same [ObjectHeader] over
 * the same [itemFacts], so the two frames of one task cannot come to name the
 * same six things in two orders.
 *
 * IT RESOLVES ITS OWN PEOPLE, like every other peek in the product. It used
 * to take a [RowChrome] instead, and the frame is what mounts it — from a
 * `peek=` token, knowing nothing about an org chart — so the only caller
 * handed it `{}` and every person in the rail rendered as a raw handle while
 * the page for the same task named them. The chart is a store read, not
 * something a list has to thread through.
 */
export function ItemPeek({ itemKey }: { itemKey: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const state = useQuery("work_item", { id: itemKey }, { enabled: itemKey !== "", pollMs: 15_000 });
  const item = state.data?.task;
  // GUARDED LIKE THE FULL SCREEN'S, and for the same reason: the peek mounts
  // before its item is read, so an unguarded call asks for a project with no
  // key every time somebody opens one.
  const project = useQuery("work_project", item ? { key: item.project } : undefined, {
    enabled: Boolean(item),
    pollMs: 300_000,
  });

  // THE PAGE'S OWN CHROME, built from the same two reads: the chart for the
  // names and the project for its vocabulary.
  const inner: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    types: project.data?.types,
    statuses: project.data?.statuses,
  };

  return (
    <>
      {state.loading && !state.data && (
        <Skeleton variant="text" rows={6} label="Loading the item" />
      )}
      <QueryState error={state.error} loading={state.loading}>
        {state.data &&
          (item ? (
            <>
              {/* THE PAGE'S OWN HEADER, one size down. It used to be a type
                  glyph, a mono key, a title line and a row of badges written
                  out here — four pieces of the page, re-drawn, which is how
                  the two frames of one task came to say the same facts in two
                  orders. */}
              <ObjectHeader
                size="peek"
                kind="Item"
                icon={typeIcon(item.type)}
                identifier={item.key}
                title={item.title}
                status={itemFlags(state.data)}
                facts={itemFacts({ detail: state.data, chrome: inner, now })}
              />
              <div className="col gap-3">
                {item.removed && <RemovedNote tomb={item.removed} now={now} />}
                <Coverage answer={state.data} />
                <ItemProps detail={state.data} chrome={inner} project={project.data} compact />
                <ItemLinks detail={state.data} chrome={inner} flush />
                <ItemBody detail={state.data} chrome={inner} now={now} flush />
              </div>
            </>
          ) : (
            // NOT AN EMPTY PANEL, and not a spinner that never resolves. An
            // answer carrying no task is what a build older or newer than this
            // one can send, and the honest reply names the address that
            // resolved to nothing rather than drawing a blank rail over the
            // list. A refusal — `not_found` for a key nobody minted — is
            // [QueryState]'s own sentence above.
            <EmptyState
              size="compact"
              icon={<Package2Glyph size="xl" />}
              title={`Nothing answers to “${itemKey}”`}
              description="A task is addressed by its key or by its uuid, and both resolve. This node holds neither — it may have been removed, or its log may not have reached this far."
            />
          ))}
      </QueryState>
    </>
  );
}

// ---------------------------------------------------------------------------
// The three shared pieces
// ---------------------------------------------------------------------------

/**
 * The children of a task, and the one rule this panel exists to keep.
 *
 * A FAILED SUBTREE READ IS NOT AN EMPTY ONE. This is the ONLY read that can
 * see a task's children — the detail answer carries links and comments but
 * never the tree below it — so nothing else on the screen contradicts it, and
 * a refusal that renders as no panel says "this task has no subtasks" about a
 * task that may have twenty. It is the same confusion the `scope` parameter
 * produced before it was corrected: the read failed, every child came back
 * absent, and the panel simply did not draw.
 *
 * So the error is drawn WHERE THE ROWS WOULD HAVE BEEN, and only a read that
 * actually answered is allowed to conclude the task is a leaf.
 */
export function Subtasks({
  rows,
  error,
  now,
  chrome,
  peek,
}: {
  rows: WorkSummary[];
  error?: string | null;
  now: number;
  chrome: RowChrome;
  /**
   * Open a child in the rail instead of navigating to it.
   *
   * ONE CALLBACK RATHER THAN A HOOK: this panel is rendered directly by its
   * own suite,
   * and `usePeekControls` reads the navigator, so reaching for it here would
   * make every case in that file stand up a router it has no use for.
   *
   * ABSENT IS A REAL SETTING — the row stays the plain link it already is —
   * and the peek passes nothing: see [ItemBody].
   */
  peek?: (row: WorkSummary) => void;
}) {
  if (error) {
    return (
      <Card padding="none">
        <Card.Header>
          <Card.Title>Subtasks</Card.Title>
        </Card.Header>
        <QueryState error={error} loading={false} />
      </Card>
    );
  }
  if (rows.length === 0) return null;
  return (
    <Card padding="none">
      <Card.Header count={rows.length}>
        <Card.Title>Subtasks</Card.Title>
      </Card.Header>
      <RowList
        rows={rows}
        now={now}
        chrome={chrome}
        hrefOf={(row) => href(["work", row.key])}
        onOpen={peek}
      />
    </Card>
  );
}

/**
 * A titled block of the body — a panel on the page, a labelled section in the
 * peek.
 *
 * AT MODULE SCOPE, AND THAT IS THE WHOLE POINT. It was two arrow components
 * built inside [ItemBody]'s render and chosen with `flush`, so its type
 * identity was new on every render — and this screen re-renders once a second,
 * because it holds a live clock for its relative times. React reconciles on
 * type identity, so the description's rendered markdown was torn down and
 * rebuilt every tick: a reader selecting a sentence to copy lost the selection
 * within a second, along with focus and every other piece of per-node DOM
 * state. `flush` is a prop here rather than a branch at definition time for
 * exactly that reason.
 */
function BodySection({
  title,
  flush,
  children,
}: {
  title: string;
  flush?: boolean;
  children: ReactNode;
}) {
  if (flush) {
    return (
      <section className="col gap-2">
        <div className="t-label">{title}</div>
        {children}
      </section>
    );
  }
  return (
    <Card>
      <Card.Header>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      {children}
    </Card>
  );
}

/** The description, the subtasks, the checklists and the thread. */
export function ItemBody({
  detail,
  chrome,
  now,
  flush,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  now: number;
  /** Inside the peek, where the panels are the page's own chrome already. */
  flush?: boolean;
}) {
  const item = detail.task;
  // WHOSE NAME WOULD LAND on a change made from the block at the foot — the
  // token's own, not a seat's, which is what an operator write is attributed
  // to. Here rather than passed in, because the peek renders this same body
  // and would otherwise have to thread it through for one line of prose.
  const viewer = useViewer();
  const { open: openPeek } = usePeekControls();
  const [tab, setTab] = useParam("thread", "comments");
  // WHICH CHANGE'S ROUTING IS OPEN. A filter rather than a section: stepping
  // through the announced changes must not fill the back stack.
  const [record, setRecord] = useParam("record", "", "filter");

  // A SUBTREE IS ITS OWN QUESTION. `parent=` turns the root-only subtask mode
  // off, so this is the one read that can see the children of a task — the
  // detail answer carries links and comments but never the tree below it.
  //
  // THE SERVER'S OWN PARAMETER NAMES, and only those. `scope` is this
  // screen's word for a group of statuses and the engine's grammar has no
  // such key — it REFUSES a parameter it does not read rather than ignoring
  // one, which is right, and the whole read failed. Every child was then an
  // empty result indistinguishable from a task with no subtasks, so the
  // panel never drew at all. Every status is wanted here: a subtask that is
  // done is a subtask, and hiding it makes a finished tree look empty.
  const children = useQuery(
    "work_items",
    item ? { container: `project:${item.project}`, parent: item.id, limit: 100 } : undefined,
    { enabled: Boolean(item), pollMs: 60_000 },
  );
  const subtasks: WorkSummary[] = children.data?.items ?? [];
  const comments = detail.comments ?? [];
  const history = detail.history ?? [];

  return (
    <>
      <BodySection title="Description" flush={flush}>
        {item.body ? (
          <div className="prose md">{renderMarkdown(item.body)}</div>
        ) : (
          <span className="muted">No description was written.</span>
        )}
      </BodySection>

      {/* A CHILD PEEKS FROM THE PAGE AND NAVIGATES FROM THE RAIL, which is the
          same split `SubUnitLinks` makes and for the same reason: the rail
          holds one object at a time and `[` and `]` step the list it was
          opened FROM, so a subtask swapped into it in place of its parent
          would leave the stepper walking a list this task is not in. On the
          page there is no such list to lose — the task stays behind the rail,
          which is the whole of why a peek is not a navigation. */}
      <Subtasks
        rows={subtasks}
        error={children.error}
        now={now}
        chrome={chrome}
        peek={flush ? undefined : (row) => openPeek({ kind: "item", id: row.key })}
      />

      {(item.checklists ?? []).map((list) => (
        <Card key={list.id}>
          <Card.Header icon={<CheckGlyph size="sm" />} count={list.items?.length ?? 0}>
            <Card.Title>{list.name}</Card.Title>
          </Card.Header>
          {(list.items ?? []).map((entry) => (
            <div key={entry.id} className={`work-check${entry.done ? " done" : ""}`}>
              {entry.done ? <CheckGlyph size="sm" /> : <RemoveGlyph size="sm" />}
              <span className="work-check-name">{entry.name}</span>
              {entry.promoted_to && (
                <a className="t-link" href={href(["work", entry.promoted_to])}>
                  became a subtask →
                </a>
              )}
              <span className="spacer" />
              {entry.assignee && <Assignee handle={entry.assignee} seatName={chrome.seatName} />}
            </div>
          ))}
        </Card>
      ))}

      <Card padding="tight">
        {/* THEIR STRIP, AND THE PANEL STAYS A SIBLING. `Tabs` activates on
            click rather than on arrow, exactly as ours did, so nothing about
            the keyboard moves. `panelId`/`TabPanel` are deliberately not
            wired: the three views are siblings here rather than one element,
            which is the case our own `children` was already optional for. */}
        <Tabs
          ariaLabel="Thread or history"
          value={tab}
          onValueChange={(next) => setTab(next as typeof tab)}
          items={[
            {
              value: "comments",
              label: "Thread",
              icon: <ChatGlyph size="sm" />,
              count: comments.length,
            },
            {
              value: "history",
              label: "History",
              icon: <TimelineGlyph size="sm" />,
              count: history.length,
            },
            // THE FACT NO OTHER TRACKER RECORDS. Every tracker can say a
            // change notified somebody; this one records, per change and
            // per person, the ONE reason of twenty it reached them under
            // and whether it ASKS anything of them.
            {
              value: "woke",
              label: "Woke",
              icon: <NotificationsGlyph size="sm" />,
              count: history.filter((entry) => !entry.quiet).length,
            },
          ]}
        />
        {tab === "comments" ? (
          <Thread comments={comments} chrome={chrome} now={now} more={detail.comments_cursor} />
        ) : tab === "woke" ? (
          <Woke history={history} chrome={chrome} now={now} record={record} onPick={setRecord} />
        ) : (
          <History detail={detail} chrome={chrome} now={now} />
        )}
      </Card>
      {/* THE READ-ONLY PRODUCT'S ANSWER TO AN EDIT BUTTON. Closed by default:
          it is what to do when somebody wants to change this, not what the
          screen is about. */}
      <ToolCallBlock
        subject={{ kind: "item", id: detail.task.key, removed: Boolean(detail.task.removed) }}
        viewer={viewer.handle}
      />
    </>
  );
}

function Thread({
  comments,
  chrome,
  now,
  more,
}: {
  comments: WorkComment[];
  chrome: RowChrome;
  now: number;
  more?: string;
}) {
  if (comments.length === 0) {
    return <span className="muted">Nobody has commented.</span>;
  }
  return (
    <div className="col gap-3" style={{ paddingTop: "var(--space-3)" }}>
      {more && (
        <span className="t-caption">
          Older comments are on the engine's own thread — this is the newest page of it.
        </span>
      )}
      {comments.map((comment) => (
        <div
          key={comment.id}
          className={`comment${comment.ask ? " work-ask" : ""}`}
          style={comment.reply_to ? { marginLeft: "var(--space-4)" } : undefined}
        >
          <div className="row gap-2 wrap">
            <SeatChip
              name={chrome.seatName?.(comment.author) ?? comment.author}
              handle={comment.author}
            />
            <span className="muted" title={fmtDateTime(comment.created_at)}>
              {relTime(comment.created_at, now)}
            </span>
            {comment.updated_at && <span className="muted">(edited)</span>}
            {/* AN ASK IS THE ONE STATE IN A THREAD THAT IS ABOUT THE READER:
                somebody owes it an answer, and it does not block a close. */}
            {comment.ask && (
              <Tag variant="warning" leadingIcon={<HelpGlyph size="xs" />}>
                asked {chrome.seatName?.(comment.ask) ?? comment.ask}
              </Tag>
            )}
            {comment.answers && (
              <Tag variant="info" leadingIcon={<ArrowForwardGlyph size="xs" />}>
                answers a question
              </Tag>
            )}
            {comment.resolved && <Tag variant="success">Resolved</Tag>}
          </div>
          {/* A REMOVED COMMENT KEEPS ITS ROW so replies still resolve against
              something — and says so, rather than rendering as an empty one. */}
          <div className="prose md">
            {comment.removed ? (
              <span className="muted">(this comment was removed)</span>
            ) : (
              renderMarkdown(comment.body)
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

function History({
  detail,
  chrome,
  now,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  now: number;
}) {
  const history = detail.history ?? [];
  if (history.length === 0) {
    return <span className="muted">Nothing has moved this item yet.</span>;
  }
  return (
    <div className="work-hist" style={{ paddingTop: "var(--space-2)" }}>
      {history.map((entry) => (
        <div key={entry.id} className="work-hist-row">
          {/* THE KIND, AS A MARK. Every row drew the same timeline glyph, so a
              comment and a field edit were the same picture — while the
              cross-item feed, which renders the kind as a tag in a column of its
              own, never had the problem. Decorative on purpose — no `title`, so
              it stays aria-hidden: `glyph.tsx`'s own rule is that a mark in a row
              that already says the word reads the word twice, and the sentence
              beside this one always carries the fact (the deltas name the field,
              and `changePhrase` names the kind when there are none). The mark is
              the fast scan; the text is the authority. */}
          <Mark name={changeMark(entry.kind)} size="xs" />
          <span className="work-hist-what">
            <strong>
              {entry.actor ? (chrome.seatName?.(entry.actor) ?? entry.actor) : "the engine"}
            </strong>{" "}
            {describeHistory(entry, chrome)}
          </span>
          {/* A CONTROL DOES NOT LIVE IN A TRUNCATING CELL. `turn →` sat after the
              sentence inside the same `.truncate` span, so the ellipsis ate it on
              every row whose text filled the track — which was every comment row,
              because the text WAS the comment. Its own track also puts it in the
              same place on every row, which is what lets a reader scan a column
              instead of hunting each line. */}
          <span className="work-hist-tail">
            {/* A COMMIT THAT ANNOUNCED NOTHING — a fact about the change rather
                than about its importance: a bulk edit is quiet by
                construction. */}
            {entry.quiet && <span className="muted">quiet</span>}
            {entry.turn_id && (
              <a className="t-link" href={href(["activity", "turns", entry.turn_id])}>
                turn →
              </a>
            )}
          </span>
          <span className="work-hist-when" title={fmtDateTime(entry.at)}>
            {relTime(entry.at, now)}
          </span>
        </div>
      ))}
    </div>
  );
}

/**
 * Who each announced change woke, and under which reason.
 *
 * # The list is the item's OWN history, not a second query
 *
 * The detail answer already carries every change; the only thing missing was
 * the routing, and that is one question per change rather than one per item.
 * So the left half is a filter over what is already here — the announced
 * changes — and only the SELECTED one is asked about.
 *
 * # A quiet change has no row
 *
 * `quiet` means the commit carried no notification at all, which is most field
 * edits and every bulk one. Listing them here would bury the handful that
 * actually reached somebody under the hundreds that did not try to.
 *
 * # Three empty answers, three sentences
 *
 * `nobody`, `swept` and `unknown` are different facts and send a reader
 * somewhere different — at who has left the company, at the retention horizon,
 * and at neither. They are one empty list on every other tracker.
 */
function Woke({
  history,
  chrome,
  now,
  record,
  onPick,
}: {
  history: WorkChange[];
  chrome: RowChrome;
  now: number;
  record: string;
  onPick: (id: string) => void;
}) {
  const announced = useMemo(() => history.filter((entry) => !entry.quiet), [history]);
  // THE NEWEST ANNOUNCED CHANGE by default, because a reader opening this tab
  // is almost always asking about what just happened.
  const selected = record || announced[0]?.id || "";
  const routing = useQuery("work_routing", { record_id: selected }, { enabled: selected !== "" });

  if (announced.length === 0) {
    return (
      <EmptyState
        size="compact"
        icon={<NotificationsGlyph size="xl" />}
        title="Nothing here announced anything"
        description="Every change to this item was quiet — a field edit, or part of a bulk call. A quiet commit still writes a history row, which is how you can tell it from nothing having happened."
      />
    );
  }

  return (
    <div className="split" style={{ paddingTop: "var(--space-2)" }}>
      <div className="list">
        {announced.map((entry) => (
          <button
            key={entry.id}
            type="button"
            className={`thread-entry as-row${entry.id === selected ? " selected" : ""}`}
            onClick={() => onPick(entry.id)}
          >
            <span className="row gap-1">
              {/* THE KIND HERE TOO. Every row of this list is a notification
                  by construction, so a notifications mark on each said only what
                  the tab above already says, and the rows were as
                  indistinguishable as the history's were. */}
              <Mark name={changeMark(entry.kind)} size="xs" />
              <span className="truncate t-cell" style={{ flex: 1 }}>
                <strong>
                  {entry.actor ? (chrome.seatName?.(entry.actor) ?? entry.actor) : "the engine"}
                </strong>{" "}
                {describeHistory(entry, chrome)}
              </span>
              <span className="t-caption" title={fmtDateTime(entry.at)}>
                {relTime(entry.at, now)}
              </span>
            </span>
          </button>
        ))}
      </div>

      <QueryState error={routing.error} loading={routing.loading}>
        {routing.data && <Routing answer={routing.data} chrome={chrome} />}
      </QueryState>
    </div>
  );
}

/** One change's recipients, or the reason there are none to show. */
export function Routing({ answer, chrome }: { answer: WorkRoutingAnswer; chrome: RowChrome }) {
  if (answer.recipients.length === 0) {
    return <NobodyWoken answer={answer} />;
  }
  // THE EXCERPT IS USUALLY ONE SENTENCE FOR EVERYBODY, so it is shown once
  // above the list rather than repeated under every name. It CAN differ per
  // recipient — a mention's excerpt is the sentence naming them — and a row
  // that differs still carries its own, which is the only case where the
  // repetition was ever information.
  const shared =
    answer.recipients.length > 0 &&
    // COMPARED RAW, flattened only once it is chosen: two different excerpts
    // that flatten to one string are still two excerpts, and collapsing them
    // here would claim a sentence everybody got that nobody was sent.
    answer.recipients.every((r) => r.excerpt === answer.recipients[0]?.excerpt)
      ? plainText(answer.recipients[0]?.excerpt ?? "")
      : "";
  return (
    <div className="col gap-2">
      <div className="row gap-1 wrap">
        <Tag appearance="outline">
          {answer.recipients.length} {answer.recipients.length === 1 ? "person" : "people"}
        </Tag>
        {answer.addressed > 0 && <AsksTag count={answer.addressed} />}
        {answer.fallback > 0 && (
          <Tag appearance="outline" title="reached only because nobody better was found">
            {answer.fallback} by fallback
          </Tag>
        )}
      </div>
      {shared && <p className="t-caption">{shared}</p>}
      <div className="list">
        {answer.recipients.map((r) => (
          <div key={r.handle} className="thread-entry">
            <div className="row gap-1">
              <SeatChip name={chrome.seatName?.(r.handle) ?? r.handle} handle={r.handle} />
              <span className="spacer" />
              {/* WHETHER IT ASKED IS ITS OWN MARK, never a tint on the reason.
                  Folded into the reason chip's colour it made the chip say two
                  things in one pill, and the ground it said the second one in is
                  the accent — which this product spends on where the READER is,
                  and which a pressed FilterChip takes byte for byte. `AsksTag`
                  is the mark My work has drawn for this all along. */}
              {r.addressed && <AsksTag />}
              {/* THE REASON IS THE ROW'S POINT, so it is a badge rather
                  than a caption: it is the one of twenty that found them.
                  IN THE THIRD PERSON — this list is about colleagues, and
                  `reasonPhrase`'s "assigned to you" beside somebody else's
                  name is a sentence about the wrong person. */}
              <Tag appearance="outline">{reasonAbout(r.reason)}</Tag>
              {r.fallback && (
                <Tag appearance="outline" title="nobody better was found">
                  substitute{r.fallback_rank ? ` #${r.fallback_rank}` : ""}
                </Tag>
              )}
            </div>
            {!shared && r.excerpt && <p className="t-caption">{plainText(r.excerpt)}</p>}
          </div>
        ))}
      </div>
      {answer.truncated && (
        <p className="t-caption">
          The list was cut. A change this engine wrote cannot name this many people, so a peer on a
          different build wrote a larger recipient set.
        </p>
      )}
    </div>
  );
}

/** The three empty answers, each with its own remedy. */
function NobodyWoken({ answer }: { answer: WorkRoutingAnswer }) {
  if (!answer.held) {
    return (
      <EmptyState
        size="compact"
        icon={<HelpGlyph size="xl" />}
        title="No such change here"
        description="A record id this node holds no history row for — a link from before a purge, or a reanchor that has not replayed this far."
      />
    );
  }
  if (answer.delivery === "swept") {
    return (
      <EmptyState
        size="compact"
        icon={<ScheduleGlyph size="xl" />}
        title="Beyond the retention window"
        description={`This change announced something, and it is older than ${
          answer.retained_from ? fmtDateTime(answer.retained_from) : "the inbox horizon"
        } — so whether anybody was reached is a fact this node no longer holds. The history it points at is untouched.`}
      />
    );
  }
  if (answer.delivery === "unknown") {
    return (
      <EmptyState
        size="compact"
        icon={<HelpGlyph size="xl" />}
        title="Cannot say"
        description="This change announced something and no recipients remain, and this node was not told how long an inbox is kept — so an absent set cannot be dated."
      />
    );
  }
  return (
    <EmptyState
      size="compact"
      icon={<GroupGlyph size="xl" />}
      title="It announced, and reached nobody"
      description="Every candidate was the person making the change, or has left the company. The notification was formed and had nowhere to go — which is a different fact from a quiet commit, and the only place it is visible."
    />
  );
}

/**
 * Everything about the task that is not its prose.
 *
 * AN ABSENT VALUE IS AN EM DASH, never a zero: zero is a measurement, and a
 * panel that rendered an unestimated task as "0m" and an unheld one as a blank
 * would be making two claims nobody wrote down.
 */
export function ItemProps({
  detail,
  chrome,
  project,
  compact,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  project?: WorkProjectDetail | null;
  compact?: boolean;
}) {
  const item = detail.task;
  const defs = useMemo(() => {
    const map = new Map<string, WorkFieldDef>();
    for (const group of project?.fields ?? []) {
      for (const field of group.fields) map.set(field.id, field);
    }
    return map;
  }, [project]);
  const seatName = chrome.seatName ?? ((h: string) => h);
  const spend = item.spend;

  // WHO SET EACH OF THESE, from the change log the page already holds. See
  // [attribution]: the keys are the tracker's own field names, and a property
  // whose last change fell outside the history window carries no line rather
  // than borrowing the oldest one still visible.
  const now = useNow();
  const setBy = useMemo(() => attribution(detail.history), [detail.history]);
  // NAMED THE WAY THE ROWS BESIDE IT NAME PEOPLE. The change log carries a
  // handle, and the line rendered it raw — `agent-ceo · 21h ago` under a
  // value whose Reporter row said "Agent CEO" — while the History tab one
  // card down resolved the same actor through the chart. One person, two
  // names, on one screen.
  const by = (field: ChangeField): SetBy | undefined => {
    const who = setBy.get(field);
    return who
      ? { ...who, actor: seatName(who.actor), ago: who.at ? relTime(who.at, now) : undefined }
      : undefined;
  };

  // EVERY ROW SAYS WHICH KIND OF ABSENCE IT HAS. A property left out of the
  // list does not exist on this object; one with no `value` exists and holds
  // nothing, and renders the dash; one whose empty state means something says
  // that instead. See [PropertiesRail].
  const groups: PropertyGroup[] = [
    {
      name: "State",
      properties: [
        {
          label: "Status",
          value: <StatusBadge status={item.status} defs={chrome.statuses} />,
          setBy: by("status"),
        },
        {
          label: "Priority",
          value:
            item.priority && item.priority !== "none" ? (
              <PriorityMark priority={item.priority} word />
            ) : undefined,
          setBy: by("priority"),
        },
        {
          label: "Type",
          value: item.type ? (
            <span className="row gap-1">
              <TypeIcon type={item.type} types={chrome.types} decorative />
              {typeName(item.type, chrome.types)}
            </span>
          ) : undefined,
          setBy: by("type"),
        },
      ],
    },
    {
      name: "People",
      properties: [
        {
          label: "Assignee",
          value: item.assignee ? (
            <SeatChip name={seatName(item.assignee)} handle={item.assignee} />
          ) : (
            <span className="muted">Unassigned</span>
          ),
          setBy: by("assignee"),
        },
        {
          label: "Reporter",
          value: item.reporter ? (
            <SeatChip name={seatName(item.reporter)} handle={item.reporter} />
          ) : undefined,
        },
        // DROPPED rather than dashed: a task nobody is collaborating on has
        // no collaborators, which is not the same as an empty set of them.
        ...((item.collaborators ?? []).length > 0
          ? [
              {
                label: "Collaborators",
                value: (
                  <span className="row wrap gap-1">
                    {(item.collaborators ?? []).map((handle) => (
                      <SeatChip key={handle} name={seatName(handle)} handle={handle} />
                    ))}
                  </span>
                ),
              },
            ]
          : []),
        ...((item.watchers ?? []).length > 0
          ? [
              {
                label: "Watching",
                value: (
                  <span className="row wrap gap-1">
                    {(item.watchers ?? []).map((handle) => (
                      <span key={handle} className="row gap-1">
                        <SeatChip name={seatName(handle)} handle={handle} />
                        {/* MUTED IS NOT THE SAME AS NOT WATCHING, and both
                            travel: a set that carried only the difference
                            would silently re-add everybody on the next
                            mention. */}
                        {(item.muted ?? []).includes(handle) && (
                          <span className="muted">(muted)</span>
                        )}
                      </span>
                    ))}
                  </span>
                ),
              },
            ]
          : []),
      ],
    },
    {
      name: "Plan",
      properties: [
        {
          label: "Start",
          value: item.start_at ? fmtDate(item.start_at) : undefined,
          setBy: by("start"),
        },
        {
          label: "Due",
          value: item.due_at ? (
            <span className="row gap-1">
              {fmtDate(item.due_at)}
              {item.due_all_day && <span className="muted">all day</span>}
            </span>
          ) : undefined,
          setBy: by("due"),
        },
        {
          label: "Estimate",
          value: item.estimate_minutes ? fmtMinutes(item.estimate_minutes) : undefined,
          setBy: by("estimate"),
        },
        { label: "Points", value: item.points ? item.points : undefined, setBy: by("points") },
        ...((item.tags ?? []).length > 0
          ? [
              {
                label: "Tags",
                value: (
                  <span className="row wrap gap-1">
                    {(item.tags ?? []).map((slug) => (
                      <Tag key={slug} appearance="outline">
                        {project?.tags?.find((t) => t.slug === slug)?.label ?? slug}
                      </Tag>
                    ))}
                  </span>
                ),
                setBy: by("tags"),
              },
            ]
          : []),
      ],
    },
  ];

  if ((detail.fields ?? []).length > 0) {
    groups.push({
      name: "Fields",
      properties: (detail.fields ?? []).map((field) => {
        const state = fieldValueState(field);
        return {
          label: field.name || field.slug || field.id,
          value: (
            <>
              {fieldValueText(field, defs, seatName)}
              {state && <span className="work-field-state">{state}</span>}
            </>
          ),
        };
      }),
    });
  }

  if (!compact) {
    groups.push(
      {
        name: "Cost",
        properties: [
          // THE REASSIGNMENT COUNT IS A BUDGET, not trivia: an item handed on
          // too many times has stopped being work and started being a hot
          // potato, and the engine refuses the next hand-off rather than
          // letting it circle.
          {
            label: "Hand-offs",
            value: (
              <span className="row gap-1">
                {item.reassignments ?? 0}
                {(item.reassignments ?? 0) >= 6 && <WarningGlyph size="xs" />}
              </span>
            ),
          },
          { label: "Turns", value: spend?.turns ? spend.turns : undefined },
          { label: "Tokens", value: spend?.tokens ? fmtCount(spend.tokens) : undefined },
          { label: "Model time", value: spend?.wall_ms ? fmtDuration(spend.wall_ms) : undefined },
        ],
      },
      {
        name: "Record",
        properties: [
          { label: "Created", value: item.created_at ? fmtDateTime(item.created_at) : undefined },
          { label: "Updated", value: item.updated_at ? fmtDateTime(item.updated_at) : undefined },
          {
            label: "In status since",
            value: item.status_entered_at ? fmtDateTime(item.status_entered_at) : undefined,
          },
          ...(item.done_at ? [{ label: "Delivered", value: fmtDateTime(item.done_at) }] : []),
          {
            label: "Filed into",
            value: item.filed_unit ? (
              <span className="truncate">{item.filed_unit}</span>
            ) : (
              <span className="muted">no unit</span>
            ),
          },
          // ROUTING IS THE MUTABLE HALF — whose lead hears about this now —
          // and it is shown only where it has MOVED, because the pair being
          // equal is the ordinary case and repeating it is noise.
          ...(item.routing_unit && item.routing_unit !== item.filed_unit
            ? [{ label: "Routes to", value: item.routing_unit }]
            : []),
          ...((item.former_keys ?? []).length > 0
            ? [
                {
                  label: "Former keys",
                  value: <span className="mono">{(item.former_keys ?? []).join(", ")}</span>,
                },
              ]
            : []),
        ],
      },
    );
  }

  return <PropertiesRail groups={groups} />;
}

/**
 * What this task is attached to, grouped by what the relation MEANS.
 *
 * The derived half of a dependency is the one nobody authored, so it is headed
 * "Blocks" rather than "Waiting on" — an editor knows which end to change, and
 * a reader is not told their task is blocked by the task it is blocking.
 */
export function ItemLinks({
  detail,
  chrome,
  flush,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  flush?: boolean;
}) {
  const links = detail.links ?? [];
  if (links.length === 0) return null;
  const groups = new Map<string, WorkLink[]>();
  for (const link of links) {
    const heading = linkHeading(link);
    const held = groups.get(heading);
    if (held) held.push(link);
    else groups.set(heading, [link]);
  }

  const body = (
    <div className="col gap-3">
      {[...groups].map(([heading, rows]) => (
        <div key={heading} className="col gap-1">
          <div className="t-label">{heading}</div>
          {rows.map((link) => (
            <div key={`${link.kind}:${link.other}`} className="row gap-2">
              {/* A PAGE IS ADDRESSED BY THE FRAME'S OWN MAP, never by a head
                  spelled here. This read `["pages", …]` — a screen the
                  application does not have — so every task→page link landed on
                  NotFound. `pathOf` is the one definition of where each kind
                  lives, and for a page it is `#/knowledge/{CONTAINER}/{Title}`.
                  `key` IS that address when the other end was resolved; a page
                  edge that came back with only its id still lands in the
                  knowledge base, which answers honestly that it holds no such
                  container — where the old head answered with no screen at
                  all. */}
              <a
                className="mono t-link"
                href={href(
                  link.kind === "page"
                    ? pathOf({ kind: "page", id: link.key || link.other })
                    : ["work", link.key || link.other],
                )}
              >
                {link.key || link.other}
              </a>
              <span className="truncate">{link.title}</span>
              {link.status && <StatusBadge status={link.status} defs={chrome.statuses} />}
              {/* THE REPAIR AND THE FINDING ARE DIFFERENT STATES: the first is
                  a commit the tracker duty writes 30 seconds later, the second
                  is an edge whose mirror was refused for good. */}
              {link.one_sided && !link.one_sided_final && (
                <Tag variant="info" title="The duty writes the other half shortly">
                  mirror pending
                </Tag>
              )}
              {link.one_sided_final && (
                <Tag variant="warning" title="The other end is gone, removed, or full">
                  one-sided
                </Tag>
              )}
            </div>
          ))}
        </div>
      ))}
    </div>
  );

  if (flush) {
    return (
      <section className="col gap-2">
        <div className="t-label">Links</div>
        {body}
      </section>
    );
  }
  return (
    <Card>
      <Card.Header icon={<LinkGlyph size="sm" />} count={links.length}>
        <Card.Title>Links</Card.Title>
      </Card.Header>
      {body}
    </Card>
  );
}

export { linkHeading };
