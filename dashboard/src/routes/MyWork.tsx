/**
 * One person's day — the seven claims on their attention, and the feed.
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
 * Twenty rows each, so no block can crowd out another — the same rule the
 * engine's own answer follows, for the same reason: this is read as one page
 * and a person with two hundred assignments would otherwise never see their
 * asks.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes. A priority list is reordered through the engine's own
 * write path, attributed to whoever did it; a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { Banner, Empty, Panel, Select, Stat, StatRow } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkAskRow, WorkChecklistRow, WorkSummary } from "~/protocol/index.ts";

export function MyWork() {
  const org = useOrg();
  const now = useNow();
  const [handle, setHandle] = useParam("handle", "");
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be
  // reached with nobody chosen and still offer somebody.
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
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
  };
  // THE PICKER'S FIRST ENTRY IS AN OBJECT, so the fallback takes its `value`:
  // the option carries a label for the person reading and a handle for the
  // engine, and the bare entry here would have sent `[object Object]`.
  const whose = handle || handles[0]?.value || "";
  // NOT UNTIL SOMEBODY IS CHOSEN — the same guard the board and the sprint
  // report take. `whose` is empty until the chart has loaded, and the engine
  // refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
  });
  const mine = state.data;

  return (
    <>
      <ScreenHead
        title="My work"
        sub="Everything one person is expected to look at — their priorities, what they hold, the questions waiting on them, and what became workable while they were away."
        actions={
          <a className="t-link" href={href(["work"])}>
            Tracker →
          </a>
        }
      />

      <div className="toolbar">
        <Select
          value={whose}
          onChange={setHandle}
          ariaLabel="Whose"
          anyLabel="Pick somebody"
          options={handles}
        />
        <span className="spacer" />
        <Coverage answer={mine} />
      </div>

      {!whose && <Empty title="Nobody chosen" hint="A day belongs to somebody." />}

      {whose && (
        <QueryState error={state.error} loading={state.loading}>
          {mine && (
            <>
              {/* COVERAGE IS A BANNER, never swallowed: rows may be missing,
                  and a day rendered as complete when it is not is a person
                  who thinks they are done. */}
              {mine.complete === false && (
                <Banner tone="caution">
                  This node could not account for every change yet, so a block may be short.
                </Banner>
              )}
              <StatRow cols={4}>
                <Stat label="Priorities" value={mine.priorities.length} sub="in the stored order" />
                <Stat label="Assigned" value={mine.assigned.length} />
                <Stat
                  label="Asked"
                  value={mine.asked_of_me.length}
                  icon={mine.asked_of_me.length ? "alert" : undefined}
                  sub="waiting on an answer"
                />
                <Stat label="Unblocked" value={mine.unblocked_recent.length} sub="newly workable" />
              </StatRow>

              <Asks rows={mine.asked_of_me} now={now} chrome={chrome} />
              <TaskBlock
                title="Priorities"
                hint="What somebody put at the top of this list, in the order they put it."
                rows={mine.priorities}
                now={now}
                chrome={chrome}
              />
              <TaskBlock title="Assigned" rows={mine.assigned} now={now} chrome={chrome} />
              <TaskBlock
                title="Unblocked"
                hint="Work whose blockers have all finished — the one block about a change rather than a state."
                rows={mine.unblocked_recent}
                now={now}
                chrome={chrome}
              />
              <Checklist rows={mine.checklist_items} />
              <TaskBlock
                title="Collaborating"
                hint="Brought on without owning."
                rows={mine.collaborating}
                now={now}
                chrome={chrome}
              />
              <TaskBlock title="Watching" rows={mine.watching_recent} now={now} chrome={chrome} />
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/** A block of tasks, ABSENT when it is empty rather than drawn as a shrug: a
 *  page of seven "nothing here" panels buries the one that has something. */
export function TaskBlock({
  title,
  hint,
  rows,
  now,
  chrome,
}: {
  title: string;
  hint?: string;
  rows: WorkSummary[];
  now: number;
  chrome?: RowChrome;
}) {
  if (rows.length === 0) return null;
  // THE TRACKER'S OWN ROW, so a task looks the same here as it does on the
  // board it came from: this page used to render four of a row's facts and
  // the board six, and only one of the two knew a task could be blocked.
  return (
    <Panel title={title} subtitle={hint} count={rows.length} padding="none">
      <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(["work", row.key])} />
    </Panel>
  );
}

/** The questions waiting on this person.
 *
 *  FIRST on the page, because an unanswered question is the only block where
 *  somebody else is blocked on THIS person rather than the other way round. */
export function Asks({
  rows,
  now,
  chrome,
}: {
  rows: WorkAskRow[];
  now: number;
  chrome?: RowChrome;
}) {
  if (rows.length === 0) return null;
  return (
    <Panel title="Asked of you" count={rows.length} icon="help">
      <div className="col gap-3">
        {rows.map((ask) => (
          // THE CAUTION RAIL, the same mark a question wears in a thread:
          // this is the one block where somebody else is blocked on THIS
          // person rather than the other way round.
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
            <div className="prose">{ask.body}</div>
          </div>
        ))}
      </div>
    </Panel>
  );
}

/** Sub-items claimed on somebody else's task.
 *
 *  Its own block because no assignee filter over tasks reaches one: a person
 *  holding six checklist items and no assignment reads their queue as empty. */
export function Checklist({ rows }: { rows: WorkChecklistRow[] }) {
  if (rows.length === 0) return null;
  return (
    <Panel title="Checklist items" count={rows.length} subtitle="On other people's tasks.">
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
    </Panel>
  );
}
