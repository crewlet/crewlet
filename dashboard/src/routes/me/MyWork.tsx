/**
 * One person's day — the seven claims on their attention, as tabs.
 *
 * # Seven claims, and each is a different one
 *
 * A person who saw only their assignments would miss six other things asking
 * for their time: what a lead put at the top of their list, the questions
 * waiting on an answer, the sub-items they claimed on somebody else's task,
 * the work they were brought onto without owning, what moved on what they
 * follow, and what became workable while they were not looking.
 *
 * # Why they are tabs now, and what keeps the old promise
 *
 * They were seven stacked cards under four stat tiles that restated the counts
 * of the cards below them, and each card was ABSENT when empty — so the page's
 * shape changed with the day and the block that mattered was wherever it
 * happened to fall. A person with two hundred assignments never saw their
 * asks.
 *
 * The promise the stacking was making — that no block can crowd out another —
 * is kept by the STRIP rather than by the page: every tab carries its count,
 * always, so an unanswered question is visible as a number on a tab nobody has
 * opened. A tab with nothing in it is drawn and says so, which stacking could
 * not do without seven "nothing here" panels burying the one that had
 * something.
 *
 * # The Inbox is not one of them
 *
 * What REACHED somebody is a different question from what is ON them, and it
 * is the landing screen of this product (`#/inbox`). The card that drew it
 * here was the inbox in a narrower column with a smaller bound.
 *
 * # Assigned is the work list, narrowed to one person
 *
 * It is `routes/work/ItemsView.tsx` — the same component `#/work` and a
 * project's Items lens are — held here by an [ItemsHost] that fixes the one
 * thing this tab IS (the assignee) and what it opens on, and leaves the shape,
 * the grouping, the order, the columns and every other filter to the reader,
 * in the URL, exactly as they are on the other two screens. Written as a
 * second renderer it had no Filter menu, no Display menu, no scope switch, no
 * chips, no count line and no way past its two hundredth row, and each of
 * those was a rule the work list already kept.
 *
 * IT OPENS BANDED BY WHEN. "What have I missed, what is today, what is this
 * week" is the question somebody opens their own work to ask, where a status
 * grouping answers one nobody asked — every task they hold is in progress or
 * about to be. The bands are the ENGINE's `due:bucket` axis, cut against the
 * COMPANY's day start like the row's own overdue flag and every `due=` filter,
 * so the heading a task is under and the flag beside it can never disagree.
 * They were computed here, from the browser's own midnight and the browser's
 * own week, and only the bands that held rows were drawn — so a reader west of
 * the company saw a task banded Earlier that the same answer called due today,
 * and nothing said which of the six bands a quiet day was missing.
 *
 * # Whose day it is decides the pronoun, everywhere
 *
 * Including the wake reasons. `reasonPhrase` is the second person — "assigned
 * to you" — and on an operator reading a report's day that is a sentence about
 * the reader, who is not on the list.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes. A priority list is reordered through the engine's own
 * write path, attributed to whoever did it; a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { renderMarkdown } from "~/lib/markdown.ts";
import { href, useParam } from "~/app/router.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { Callout, EmptyState, InlineCode, Select, Skeleton, Tabs, Tag } from "@crewlethq/ui";
import { KeyGlyph, PersonGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useNow } from "~/lib/clock.ts";
import { pageCount, type Scope } from "~/lib/work.ts";
import { ItemsView, type ItemsHost } from "~/routes/work/ItemsView.tsx";
import type { WorkAskRow, WorkChecklistRow, WorkMyWork, WorkSummary } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * The engine's bound on each block of `work_my_work`.
 *
 * `tracker.MyWorkRows`. It is not a page size this screen chose and it is not
 * one it can change — which is why a count drawn from one of these blocks says
 * `20+` at the bound rather than `20`: the figure is the ceiling, not the
 * company's. The Assigned tab is the one that escapes it, by asking the
 * tracker's own question instead.
 */
const BLOCK_ROWS = 20;

/**
 * The scope the Assigned tab's own COUNT is taken over.
 *
 * The tracker's default — unfinished work — because a day is about what is
 * still to do, and it is spelled here rather than read off the list's scope
 * segment on purpose: the strip's number says how much is on this person, and
 * a reader who presses Closed to check something finished has not emptied
 * their day. `SCOPE_GROUPS.open` is the same two groups on the list's side.
 */
const ASSIGNED_SCOPE = "not_started,active";

/**
 * What the Assigned tab OPENS on, as the list's own parameters.
 *
 * A MODULE CONSTANT rather than a literal in the render, which
 * [ItemsHost.opens] requires: the list memoises its question on this object,
 * so a new one every render is a new question every render.
 *
 * `due:bucket` is the engine's relative due axis — Overdue, Earlier, Today,
 * This week, Later, No due date — cut against the company's own day. `due`
 * orders what is inside each band by the same date the band was cut on, so a
 * band reads soonest first. Both are DEFAULTS: the Display menu overrides
 * either, and the address keeps whichever the reader chose.
 */
const ASSIGNED_OPENS: Record<string, string> = { group_by: "due:bucket", sort: "due" };

/** The tabs, in the order the strip draws them; the first is the default. */
const TABS = [
  "assigned",
  "priorities",
  "asks",
  "unblocked",
  "collaborating",
  "watching",
  "checklist",
] as const;
type Tab = (typeof TABS)[number];

export function MyWork() {
  const org = useOrg();
  const now = useNow();
  const [handle, setHandle] = useParam("handle", "");
  const [tab, setTab] = useTab("tab", TABS);
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be reached
  // with nobody chosen and still offer somebody.
  const index = useMemo(() => indexOrg(org), [org]);
  // THE PICKER OFFERS NAMES AND SENDS HANDLES. A list of slugs is the
  // database's vocabulary; the person choosing knows their colleagues by name.
  const handles = useMemo(
    () =>
      index.seats
        .map((s) => ({ value: s.handle, label: s.name }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [index],
  );
  const chrome: RowChrome = {
    seatName: (h) => index.byHandle.get(h)?.name ?? h,
  };
  // WHOSE DAY THIS IS, resolved rather than guessed.
  //
  // This fell back to the ALPHABETICALLY FIRST SEAT, so a screen titled "My
  // work" rendered a stranger's day to everybody. The `viewer` question walks
  // the binding that has always existed: a presented token resolves to an
  // operator id, and a seat names that id in `contact.crewlet_operator_id`.
  //
  // An explicit choice still wins — an operator reading a report's day is a
  // real thing to do, and the header says whose day it is either way.
  const viewer = useViewer();
  const whose = handle || viewer.handle;
  const ownDay = whose !== "" && whose === viewer.handle;
  const they = ownDay ? "you" : "them";

  // NOT UNTIL SOMEBODY IS CHOSEN — `whose` is empty until the chart has
  // loaded, and the engine refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
  });
  const mine = state.data;

  // AND THE ASSIGNMENTS AS THE TRACKER'S OWN QUESTION, asked whichever tab is
  // open: its total is what the Assigned tab's count says, and a count that
  // appeared only once its tab was opened would be a strip that changes as you
  // walk it.
  //
  // THE SAME QUESTION THE TAB'S OWN LIST ASKS, and deliberately not the same
  // READ: this one is the strip's count and carries no arrangement at all, so
  // it stays put while the reader groups, sorts and narrows the list under it.
  // A count that moved with the filters would answer "how much is on this
  // person" with "how much is on screen", which is what the other six tabs'
  // own bounded blocks would then disagree with.
  const assigned = useQuery(
    "work_items",
    whose ? { container: "workspace", assignee: whose, status_group: ASSIGNED_SCOPE } : undefined,
    { enabled: whose !== "", pollMs: 30_000 },
  );

  // WHAT THE ASSIGNED TAB IS, handed to the list that draws it — see
  // [ItemsHost]. Memoised on the two values it reads, because the list asks
  // the engine again whenever the lock moves and a fresh object every render
  // would be a fresh question every render.
  const host = useMemo<ItemsHost>(
    () => ({
      assignee: whose,
      opens: ASSIGNED_OPENS,
      empty: (scope: Scope) => assignedEmpty(scope, they),
    }),
    [whose, they],
  );

  return (
    <>
      {/* WHOSE DAY, said out loud. An operator reading a report's day is a real
          thing to do, and a screen called "My work" showing somebody else's
          without saying so is how a reader acts on work that is not theirs.

          ONE PILL, WHATEVER IT SAYS: colour carries STATE here and never
          identity, and the reader who must not miss this tag is the operator on
          somebody ELSE'S day — so both branches take the same pill and the
          WORDS carry whose day it is. */}
      <PageActions>
        {whose ? (
          <Tag>{ownDay ? "yours" : `${index.byHandle.get(whose)?.name ?? whose}’s day`}</Tag>
        ) : undefined}
        <a className="t-link" href={href(["inbox"])}>
          Inbox →
        </a>
        <a className="t-link" href={href(["work"])}>
          All work →
        </a>
      </PageActions>
      <PageNote>
        Everything one person is expected to look at — what they hold, the order somebody put it in,
        the questions waiting on them, and what became workable while they were away.
      </PageNote>

      <div className="toolbar">
        {/* THE EMPTY OPTION IS A REAL ROW, not their `placeholder`: choosing
            nobody is a state this screen has — it is how a reader gets back to
            "pick somebody" — and a placeholder is only ever the label over an
            unset value, with nothing to select.

            IN THE PAGE RATHER THAN IN A SIDEBAR. Whose day this is a FILTER on
            the screen you are on, and the product's own grammar puts a filter
            in the page's bar and keeps the sidebar for destinations with paths
            of their own (`app/nav.ts`). */}
        <Select
          width="auto"
          value={whose}
          onChange={(value) => setHandle(String(value))}
          ariaLabel="Whose day"
          placeholder="Pick somebody"
          active={whose !== ""}
          options={[{ value: "", label: "Pick somebody" }, ...handles]}
        />
        <span className="spacer" />
        <Coverage answer={mine} />
      </div>

      {/* THREE STATES, and they are not one empty state. A reader with no
          token, a reader whose token names no seat, and a reader who simply has
          not chosen somebody need three different sentences — and only the last
          of them is a choice anybody can make on this screen. */}
      {!whose &&
        (viewer.anonymous ? (
          <EmptyState
            icon={<KeyGlyph size={32} />}
            title="No credential is presented"
            description="A day belongs to a person, and this browser has not said who it is. Set an API token, or pick somebody above to read their day."
          />
        ) : viewer.unbound ? (
          <EmptyState
            icon={<PersonGlyph size={32} />}
            title="This token is not bound to a person"
            description={`Give a human seat contact.crewlet_operator_id: ${viewer.operatorID} in the company configuration and this becomes their day. Until then, pick somebody above.`}
          />
        ) : (
          <EmptyState
            icon={<PersonGlyph size={32} />}
            title="Nobody chosen"
            description="A day belongs to somebody."
          />
        ))}

      {whose && (
        <QueryState error={state.error} loading={state.loading}>
          {state.loading && !mine && <Skeleton variant="text" rows={6} label="Loading the day" />}
          {mine && (
            <>
              {/* COVERAGE IS A BANNER, never swallowed: rows may be missing,
                  and a day rendered as complete when it is not is a person who
                  thinks they are done. */}
              {mine.complete === false && (
                <Callout variant="warning">
                  This node could not account for every change yet, so a tab may be short.
                </Callout>
              )}

              <Tabs
                ariaLabel="Which claim"
                value={tab}
                onValueChange={(value) => setTab(value as Tab)}
                items={TABS.map((key) => ({
                  value: key,
                  // EVERY COUNT, ALWAYS, which is what replaces the stacking:
                  // a tab nobody has opened still says how much is on it.
                  label: `${TAB_LABEL[key]} ${countFor(key, mine, assigned.data?.total_hint)}`,
                }))}
              />

              {/* THE WORK LIST, NARROWED TO ONE PERSON — not a second
                  renderer. `ItemsView` brings its own Filter and Display
                  menus, its chips, its scope switch, its count line and its
                  five shapes; what this screen supplies is the one narrowing
                  the tab IS and what it opens on. */}
              {tab === "assigned" && <ItemsView host={host} />}
              {tab === "priorities" && (
                <Priorities mine={mine} now={now} chrome={chrome} they={they} />
              )}
              {tab === "asks" && (
                <Asks
                  rows={mine.asked_of_me}
                  now={now}
                  chrome={chrome}
                  ownDay={ownDay}
                  whenEmpty={
                    <EmptyState
                      size="compact"
                      title={`Nothing is waiting on ${they}`}
                      description="A question is a comment that asks something of somebody rather than informing them, and the engine records which."
                    />
                  }
                />
              )}
              {tab === "unblocked" && (
                <Block
                  rows={mine.unblocked_recent}
                  now={now}
                  chrome={chrome}
                  hint="Work whose blockers have all finished — the one tab about a change rather than a state."
                  empty={`Nothing that was waiting on something else has become workable for ${they}.`}
                />
              )}
              {tab === "collaborating" && (
                <Block
                  rows={mine.collaborating}
                  now={now}
                  chrome={chrome}
                  hint="Brought on without owning."
                  empty={`Nobody has brought ${they} onto a task they do not own.`}
                />
              )}
              {tab === "watching" && (
                <Block
                  rows={mine.watching_recent}
                  now={now}
                  chrome={chrome}
                  hint="Followed, and changed recently."
                  empty={`Nothing ${ownDay ? "you follow" : "they follow"} has moved lately.`}
                />
              )}
              {tab === "checklist" && (
                <Checklist
                  rows={mine.checklist_items}
                  whenEmpty={
                    <EmptyState
                      size="compact"
                      title="Nothing is claimed"
                      description={`A checklist item on somebody else's task can name ${they} as the one who does it.`}
                    />
                  }
                />
              )}
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/** What each tab is called. */
const TAB_LABEL: Record<Tab, string> = {
  assigned: "Assigned",
  priorities: "Priorities",
  asks: "Asks",
  unblocked: "Unblocked",
  collaborating: "Collaborating",
  watching: "Watching",
  checklist: "Checklist",
};

/**
 * What a tab's count says, and what it is a count OF.
 *
 * THE BLOCKS ARE BOUNDED AND THE ASSIGNMENTS ARE NOT. Six of these come back
 * capped at [BLOCK_ROWS], so a bare length is the CEILING on anybody busy —
 * `pageCount` is this product's own idiom for that and writes `20+`. Assigned
 * asks the tracker, whose `total_hint` is a count over the matching set, so it
 * needs no hedge; while that read is in flight there is no number to draw and
 * the tab carries none rather than a zero, which would read as an empty day.
 */
function countFor(tab: Tab, mine: WorkMyWork, assignedTotal: number | undefined): string {
  if (tab === "assigned") return assignedTotal === undefined ? "" : String(assignedTotal);
  const rows =
    tab === "priorities"
      ? mine.priorities
      : tab === "asks"
        ? mine.asked_of_me
        : tab === "unblocked"
          ? mine.unblocked_recent
          : tab === "collaborating"
            ? mine.collaborating
            : tab === "watching"
              ? mine.watching_recent
              : mine.checklist_items;
  return pageCount(rows.length, rows.length >= BLOCK_ROWS);
}

/**
 * What the Assigned tab says when it holds nothing and nothing is narrowing it.
 *
 * IN THIS PERSON'S VOICE, and one sentence per scope, because the list's own
 * three container sentences cannot say either half: they are about what has
 * been filed in a project or opened in a company, and this tab is about one
 * desk inside both. "Nothing has been filed yet" over a company with four
 * hundred tasks and one idle seat is false about the only subject the reader
 * came for.
 *
 * THE SECOND PERSON IS NOT THE DEFAULT. `they` is "you" on somebody's own day
 * and "them" on a report's, which is the same rule every other claim on this
 * screen keeps — an operator reading a colleague's day must not be told their
 * own queue is empty.
 */
function assignedEmpty(scope: Scope, they: string): { title: string; description: string } {
  if (scope === "closed") {
    return {
      title: `Nothing assigned to ${they} has been finished`,
      description:
        "Open is what is still to do, and All shows both. The scope switch in the bar is what moves between them.",
    };
  }
  if (scope === "all") {
    return {
      title: `Nothing is assigned to ${they}`,
      description:
        "Not open, and nothing finished either. A lead assigns work with the tracker's own tools, and a webhook or a schedule is usually what starts them.",
    };
  }
  return {
    title: `Nothing is assigned to ${they}`,
    description:
      "A lead assigns work with the tracker's own tools, and a webhook or a schedule is usually what starts them. Finished work is under Closed.",
  };
}

/**
 * The order somebody means to work in — theirs, or a lead's for them.
 *
 * IN THE STORED ORDER, never re-sorted: the order is the content — it is what
 * somebody decided — and sorting it discards the decision.
 */
function Priorities({
  mine,
  now,
  chrome,
  they,
}: {
  mine: WorkMyWork;
  now: number;
  chrome: RowChrome;
  they: string;
}) {
  // WHO SET IT, WHEN IT WAS NOT THEIRS. A person who starts the day on work
  // they did not choose can see who chose it, and their own next change clears
  // the stamp — taking your queue back is the gesture that says you have seen
  // it. It is on the person's own record rather than on this answer, which is
  // why it is a second read.
  const person = useQuery("work_person", { handle: mine.handle }, { pollMs: 60_000 });
  const setBy = person.data?.priorities_set_by;
  return (
    <>
      {setBy && (
        <Callout variant="info">
          {chrome.seatName?.(setBy) ?? setBy} put this order in place
          {person.data?.priorities_set_at ? ` ${relTime(person.data.priorities_set_at, now)}` : ""}.
          The next change {they === "you" ? "you make" : "they make"} to it clears the stamp.
        </Callout>
      )}
      {mine.priorities.length === 0 ? (
        <EmptyState
          size="compact"
          title={`Nothing is at the top of ${they === "you" ? "your" : "their"} list`}
          description="A queue is written with set_priorities — by the person whose it is, or by a lead in their line."
        />
      ) : (
        <div className="work-list">
          <RowList
            rows={mine.priorities}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
          />
        </div>
      )}
    </>
  );
}

/** One of the six bounded blocks, drawn as the same list as everything else. */
function Block({
  rows,
  now,
  chrome,
  hint,
  empty,
}: {
  rows: WorkSummary[];
  now: number;
  chrome: RowChrome;
  hint: string;
  empty: string;
}) {
  if (rows.length === 0) {
    // A TAB IS DRAWN EVEN WHEN IT IS EMPTY, which is the whole difference from
    // the stacked cards this replaces: they vanished, taking their own name
    // with them, so a reader could not tell "nothing here" from "this product
    // does not have that".
    return <EmptyState size="compact" title="Nothing here" description={empty} />;
  }
  return (
    <>
      <p className="t-caption">{hint}</p>
      <div className="work-list">
        <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(["work", row.key])} />
      </div>
    </>
  );
}

/**
 * The questions waiting on this person.
 *
 * THE ONE TAB WHERE SOMEBODY ELSE IS BLOCKED ON THIS PERSON rather than the
 * other way round, which is why its count wears the caution tone in the strip
 * and why each row carries the literal call that answers it: a model handed a
 * comment id still has to compose the call, and every one it composes
 * differently is a round spent being refused.
 */
export function Asks({
  rows,
  now,
  chrome,
  // WHOSE QUESTIONS THESE ARE, and the DEFAULT IS SOMEBODY ELSE'S. This block
  // is rendered on a report's day here and on every seat page, which is written
  // in the third person throughout — so a caller that says nothing is a caller
  // that has not claimed the rows are the reader's.
  ownDay = false,
  // AND WHAT AN EMPTY ONE DRAWS IS THE CALLER'S DECISION, never a default that
  // suits one of them: a TAB must say something, because a tab that rendered
  // nothing would leave its own name on the strip over a blank panel; a block
  // STACKED among other cards must say nothing, because three "nothing here"
  // panels on a quiet seat bury the one card that has something.
  whenEmpty,
}: {
  rows: WorkAskRow[];
  now: number;
  chrome?: RowChrome;
  ownDay?: boolean;
  whenEmpty?: ReactNode;
}) {
  if (rows.length === 0) return <>{whenEmpty ?? null}</>;
  return (
    <div className="col gap-3">
      {rows.map((ask) => (
        // THE CAUTION RAIL, the same mark a question wears in a thread.
        <div key={ask.comment} className="comment work-ask">
          <div className="row gap-2 wrap">
            <a className="mono t-link" href={href(["work", ask.key])}>
              {ask.key}
            </a>
            <span className="truncate">{ask.title}</span>
            <span className="spacer" />
            <SeatChip
              name={chrome?.seatName?.(ask.asked_by) ?? ask.asked_by}
              handle={ask.asked_by}
            />
            <span className="muted">{relTime(ask.asked_at, now)}</span>
          </div>
          <div className="prose md">{renderMarkdown(ask.body)}</div>
          {/* THE CALL THAT ANSWERS IT, pre-filled. The dashboard writes
              nothing, so what it offers is the gesture somebody's own
              assistant makes on their behalf. */}
          {ask.answer_with && <InlineCode>{ask.answer_with}</InlineCode>}
        </div>
      ))}
    </div>
  );
}

/**
 * Sub-items claimed on somebody else's task.
 *
 * Its own tab because no assignee filter over tasks reaches one: a person
 * holding six checklist items and no assignment reads their queue as empty.
 */
export function Checklist({
  rows,
  whenEmpty,
}: {
  rows: WorkChecklistRow[];
  /** As [Asks]'s: what an empty one draws is the caller's decision. */
  whenEmpty?: ReactNode;
}) {
  if (rows.length === 0) return <>{whenEmpty ?? null}</>;
  return (
    <div className="col">
      {rows.map((item) => (
        <div key={`${item.task}:${item.item}`} className={`work-check${item.done ? " done" : ""}`}>
          <a className="mono t-link" href={href(["work", item.task_key])}>
            {item.task_key}
          </a>
          <span className="work-check-name">{item.name}</span>
          <span className="spacer" />
          <span className="muted truncate">{item.task_title}</span>
        </div>
      ))}
    </div>
  );
}
