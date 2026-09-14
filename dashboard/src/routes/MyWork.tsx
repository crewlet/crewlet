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
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { Badge, Banner, Empty, Panel, Select, Stat, StatRow } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useViewer } from "~/lib/viewer.ts";
import { reasonPhrase } from "~/lib/reasons.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkAskRow, WorkChecklistRow, WorkSummary } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

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
  // WHOSE DAY THIS IS, resolved rather than guessed.
  //
  // This fell back to `handles[0]` — the ALPHABETICALLY FIRST SEAT — so a
  // screen titled "My work" rendered a stranger's day to everybody, and the
  // engine had no way to tell it otherwise because `work_my_work` demanded a
  // handle and was registered operator-only. The `viewer` question walks the
  // binding that has always existed: a presented token resolves to an operator
  // id, and a seat names that id in `contact.crewlet_operator_id`.
  //
  // An explicit choice still wins — an operator reading a report's day is a
  // real thing to do, and the header says whose day it is either way.
  const viewer = useViewer();
  const whose = handle || viewer.handle;
  // NOT UNTIL SOMEBODY IS CHOSEN — the same guard the board and the sprint
  // report take. `whose` is empty until the chart has loaded, and the engine
  // refuses this question without a handle.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
  });
  const mine = state.data;

  // AND WHAT REACHED THEM, which is a different question from what is on
  // them. `work_inbox` names the ONE reason of twenty under which each change
  // found this person — the fact no commercial tracker records — and the
  // reader behind it has existed, tested and swept on a 365-day retention,
  // since the tracker did.
  const inbox = useQuery("work_inbox", whose ? { handle: whose, limit: 12 } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
  });
  const notices = inbox.data?.notices ?? [];

  return (
    <>
      {/* WHOSE DAY, said out loud. An operator reading a report's day is a
          real thing to do, and a screen called "My work" showing somebody
          else's without saying so is how a reader acts on work that is not
          theirs. */}
      <PageActions>
        {whose ? (
          <Badge
            outline={whose !== viewer.handle}
            tone={whose === viewer.handle ? "positive" : undefined}
          >
            {whose === viewer.handle
              ? "yours"
              : `${index.byHandle.get(whose)?.name ?? whose}’s day`}
          </Badge>
        ) : undefined}
        {
          <a className="t-link" href={href(["work"])}>
            Tracker →
          </a>
        }
      </PageActions>
      <PageNote>
        Everything one person is expected to look at — their priorities, what they hold, the
        questions waiting on them, and what became workable while they were away.
      </PageNote>

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

      {/* THREE STATES, and they are not one empty state. A reader with no
          token, a reader whose token names no seat, and a reader who simply
          has not chosen somebody need three different sentences — and only
          the last of them is a choice anybody can make on this screen. */}
      {!whose &&
        (viewer.anonymous ? (
          <Empty
            icon="key"
            title="No credential is presented"
            hint="A day belongs to a person, and this browser has not said who it is. Set an API token, or pick somebody below to read their day."
          />
        ) : viewer.unbound ? (
          <Empty
            icon="user"
            title="This token is not bound to a person"
            hint={`Give a human seat contact.crewlet_operator_id: ${viewer.operatorID} in the company configuration and this becomes their day. Until then, pick somebody below.`}
          />
        ) : (
          <Empty title="Nobody chosen" hint="A day belongs to somebody." />
        ))}

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

              {/* WHAT REACHED THEM, and WHY. Every other block on this
                  screen answers "what is on you"; this one answers "what
                  happened that you were told about", which is the question
                  an inbox is. The reason is the row's opening fact because
                  it is the one nothing else in this category records. */}
              <Panel
                title="Reached you"
                icon="inbox"
                count={inbox.data?.unread ?? notices.length}
                padding="none"
                subtitle={
                  inbox.data
                    ? `${notices.length} most recent · counting as primary: ${(
                        inbox.data.primary_reasons ?? []
                      )
                        .map(reasonPhrase)
                        .join(" · ")}`
                    : undefined
                }
              >
                <QueryState
                  error={inbox.error}
                  loading={inbox.loading}
                  empty={
                    notices.length
                      ? undefined
                      : {
                          title: "Nothing has reached them",
                          hint: "A notice is written when a change names somebody — as an assignee, a mention, a question, a watcher. This person's record has none.",
                        }
                  }
                >
                  <div className="list">
                    {notices.map((notice) => (
                      <div
                        key={notice.record_id}
                        className="thread-entry"
                        style={{ opacity: notice.read ? 0.72 : 1 }}
                      >
                        <div className="row gap-1">
                          {notice.subject_key && (
                            <a className="mono t-link" href={href(["work", notice.subject_key])}>
                              {notice.subject_key}
                            </a>
                          )}
                          <span className="truncate t-cell" style={{ flex: 1 }}>
                            {notice.excerpt || notice.kind.replace(/_/g, " ")}
                          </span>
                          {notice.addressed && (
                            <Badge tone="caution" title="this asks something of them">
                              asks
                            </Badge>
                          )}
                          <Badge
                            outline
                            title={notice.fallback ? "nobody better was found" : undefined}
                          >
                            {reasonPhrase(notice.reason)}
                          </Badge>
                          <span className="t-caption">{relTime(notice.at, now)}</span>
                        </div>
                        {notice.actor && (
                          <span className="t-caption">
                            by {chrome.seatName?.(notice.actor) ?? notice.actor}
                            {notice.actor_kind && notice.actor_kind !== "seat"
                              ? ` (${notice.actor_kind})`
                              : ""}
                          </span>
                        )}
                      </div>
                    ))}
                  </div>
                </QueryState>
                <footer className="panel-foot">
                  Read and snoozed marks are written by this person's own assistant, through
                  mark_inbox — the dashboard shows what it recorded.
                </footer>
              </Panel>

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
