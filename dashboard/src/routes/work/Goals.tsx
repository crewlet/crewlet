/**
 * Goals — the tier above projects.
 *
 * # The percentage is not stored anywhere
 *
 * A goal is at what its TARGETS say, and the engine computes that on every
 * read from the rows the read already holds. A stored number would be a second
 * answer to a question that already has one, and the two drift the moment a
 * task in a target closes without anybody editing the goal — which is the
 * ordinary case rather than the exotic one. So this screen renders a number
 * that is always as fresh as the work behind it.
 *
 * # Health is a person's judgement, and progress is not
 *
 * The targets say what has MOVED. Only an owner knows whether the movement is
 * on track, which is why health is a field somebody sets rather than a colour
 * derived from the bar — a goal at 90% with a week left and a goal at 90% with
 * a day left are different situations, and no arithmetic separates them.
 *
 * # A goal is an object, so it has three frames and one set of facts
 *
 * The list, the page and the rail all read a goal by the same five facts, in
 * the same order, out of [goalFacts] — a reader who opens the rail from the
 * list must not have to re-learn the goal because the header put its owners
 * where the list put its health.
 *
 * # Read-only, like the board
 *
 * Nothing here writes. A goal is set through the operator MCP surface, which
 * is attributed to a credential — where a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo, type ReactNode } from "react";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { QueryState } from "~/components/common.tsx";
import { Badge, Chip, Empty, Meter, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { href, useParam } from "~/app/router.tsx";
import { fmtDate, plural, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkGoal, WorkGoalTarget } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { Dash, NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ToolCallBlock } from "~/components/ToolCall.tsx";

/** The health values, and their tone. A closed set, so one the engine adds
 *  later renders as itself rather than vanishing. */
const HEALTH: Record<
  string,
  { label: string; tone: "positive" | "caution" | "critical" | "neutral" }
> = {
  on_track: { label: "On track", tone: "positive" },
  at_risk: { label: "At risk", tone: "caution" },
  off_track: { label: "Off track", tone: "critical" },
  done: { label: "Done", tone: "positive" },
};

/**
 * What a goal wears beside its title.
 *
 * HEALTH IS THE ONE STATE A GOAL HAS, so it is the one thing on this screen
 * that carries a tone — a group is identity and takes a neutral outline
 * wherever it appears.
 *
 * NO BADGE FOR AN UNSET HEALTH, and that is not an omission: a fresh goal
 * nobody has judged is not "on track", and rendering it as anything would be
 * the screen making the judgement. `undefined` rather than an empty element,
 * because an empty child still takes its gap in the header row.
 */
function goalFlags(goal: WorkGoal): ReactNode {
  const health = HEALTH[goal.health ?? ""];
  if (!health && !goal.archived) return undefined;
  return (
    <span className="row gap-1">
      {health && <Badge tone={health.tone}>{health.label}</Badge>}
      {goal.archived && <Badge outline>Archived</Badge>}
    </span>
  );
}

/**
 * The five facts a goal is read by, in one order, on its page and in the rail.
 *
 * ONE FUNCTION rather than two lists that happen to agree today: a reader
 * scans the same facts in the same order wherever the object appears, and a
 * header written twice is two orders as soon as somebody adds a sixth fact.
 *
 * They are the five [GoalPanel] draws in the list — who owns it, which group
 * it is in, when it is due, where it is, and how many targets say so — so the
 * rail answers "is this the one I meant" in the reader's own terms.
 */
function goalFacts({
  goal,
  now,
  seatName,
}: {
  goal: WorkGoal;
  now: number;
  seatName: (handle: string) => string;
}): Fact[] {
  const owners = goal.owners ?? [];
  const first = owners[0];
  return [
    {
      label: owners.length === 1 ? "Owner" : "Owners",
      // THE NAME, NEVER THE HANDLE. A handle is the database's word for a
      // person and every other surface resolves it through the chart before
      // showing it — a goal owned by "Ada Okonkwo" attributed to
      // `ada-okonkwo` beside a board that calls her by name is one person
      // under two names.
      value:
        owners.length > 0 ? (
          owners.map(seatName).join(", ")
        ) : (
          <Dash title="nobody owns this goal" />
        ),
      // ONLY WHERE THERE IS ONE PLACE TO GO. A list of three names cannot be
      // one link, and linking the whole line to the first owner would send a
      // reader to somebody they did not click on.
      path: owners.length === 1 && first ? ["company", "people", first] : undefined,
    },
    // A GROUP IS IDENTITY, so it is a plain word rather than a toned badge —
    // colour in this product says what state a thing is in and nothing else.
    { label: "Group", value: goal.group || <Dash title="this goal is in no group" /> },
    {
      label: "Due",
      value: goal.due_at ? (
        <span title={fmtDate(goal.due_at)}>{relTime(goal.due_at, now)}</span>
      ) : (
        <Dash title="no date is set on this goal" />
      ),
    },
    {
      label: "Progress",
      // ABSENT IS NOT ZERO, and here the difference is a goal somebody
      // escalates: "there is nothing to measure" and "nothing has happened"
      // are different facts, and a goal with no targets has the first.
      value:
        goal.progress === undefined ? (
          <Dash title="no targets, so there is nothing to measure" />
        ) : (
          `${Math.round(goal.progress * 100)}%`
        ),
    },
    {
      label: "Targets",
      value: <NumberCell value={goal.targets?.length ?? 0} title="what says whether it moved" />,
    },
  ];
}

export function Goals() {
  const now = useNow();
  // A HANDLE IS THE DATABASE'S WORD FOR A PERSON, and every other surface in
  // the tree resolves it through the chart before showing it. This screen
  // printed the slug on a goal's owner chips, so a goal owned by "Ada
  // Okonkwo" was attributed to `ada-okonkwo` beside a board that calls her
  // by name — and the chip is a FILTER, so the value it sends must stay the
  // handle while the word it shows is the name.
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { open: openPeek } = usePeekControls();
  const [group, setGroup] = useParam("group", "");
  const [owner, setOwner] = useParam("owner", "");
  const [archived, setArchived] = useParam("archived", "");

  const params: Record<string, unknown> = {};
  if (group) params.group = group;
  if (owner) params.owner = owner;
  if (archived) params.archived = true;

  // A goal changes when somebody edits it or when work under it moves, and
  // neither is pushed to this socket — so a poll is correct. Sixty seconds:
  // a goal is a quarter's question, not a minute's.
  const { data, loading, error } = useQuery("work_goals", params, { pollMs: 60_000 });
  const goals = useMemo(() => data?.goals ?? [], [data]);

  // WHAT `[` AND `]` WALK: the goals this screen actually loaded, filtered as
  // the reader left them. Published rather than handed to the rail, because
  // only the list knows that order — see `PeekHost`.
  usePeekNeighbours(
    useMemo(() => goals.map((g): ObjectRef => ({ kind: "goal", id: g.id })), [goals]),
  );

  // FROM THE ROWS, like the board's project filter: a filter built from the
  // page can honestly offer what the page contains.
  const groups = useMemo(() => {
    const seen = new Set<string>();
    for (const goal of goals) if (goal.group) seen.add(goal.group);
    return [...seen].sort();
  }, [goals]);

  const atRisk = goals.filter((g) => g.health === "at_risk" || g.health === "off_track");

  return (
    <>
      <PageActions>{<Badge outline>{plural(goals.length, "goal")}</Badge>}</PageActions>
      <PageNote>
        The tier above projects: an outcome, who owns it, and what its targets say. The percentage
        is computed from the work every time it is read — nothing stores one, so nothing can
        disagree with the board.
      </PageNote>

      {!loading && !error && goals.length > 0 && (
        <StatRow cols={3}>
          <Stat icon="target" label="Goals" value={goals.length} sub="not archived" />
          <Stat
            icon="alert"
            label="At risk or off track"
            value={atRisk.length}
            sub={atRisk.length ? atRisk.map((g) => g.name).join(", ") : "nobody has raised one"}
          />
          <Stat
            icon="check"
            label="Targets"
            value={goals.reduce((n, g) => n + (g.targets?.length ?? 0), 0)}
            sub="across every goal on screen"
          />
        </StatRow>
      )}

      <div className="toolbar">
        <Select
          value={group}
          onChange={setGroup}
          ariaLabel="Group"
          anyLabel="Every group"
          options={groups}
        />
        <Chip
          on={archived === "true"}
          onClick={() => setArchived(archived ? "" : "true")}
          title="Include archived goals"
        >
          Archived
        </Chip>
        {owner && (
          <Chip on onClick={() => setOwner("")} title="Clear this filter">
            {index.byHandle.get(owner)?.name ?? owner}
          </Chip>
        )}
      </div>

      {loading && !goals.length && <Skeleton rows={4} />}

      <QueryState
        error={error}
        loading={loading}
        empty={
          goals.length
            ? undefined
            : {
                title: "No goals are set",
                hint: "A goal is an outcome with targets under it — a project, a set of tasks, a number. Set one through your assistant's Crewlet tools, or widen these filters.",
              }
        }
      >
        {goals.map((goal) => (
          <GoalPanel
            key={goal.id}
            goal={goal}
            now={now}
            onOwner={setOwner}
            seatName={(handle) => index.byHandle.get(handle)?.name ?? handle}
            peek={openPeek}
          />
        ))}
      </QueryState>
    </>
  );
}

export function GoalPanel({
  goal,
  now,
  onOwner,
  seatName,
  peek,
}: {
  goal: WorkGoal;
  now: number;
  onOwner: (h: string) => void;
  seatName: (handle: string) => string;
  /**
   * Open an object in the rail.
   *
   * ONE CALLBACK RATHER THAN A HOOK, and that is not a style choice: this
   * panel is rendered directly by its own suite, and `usePeekControls` reads
   * the navigator, so reaching for it here would make every case in that file
   * depend on a router it has no reason to stand up.
   */
  peek?: (ref: ObjectRef) => void;
}) {
  const ref: ObjectRef = { kind: "goal", id: goal.id };
  // A PANEL TITLE IS A REAL LINK EITHER WAY: its href is the goal's own page,
  // so ⌘-click and the middle button open a tab and the status bar says where
  // it goes; a plain left click peeks instead, because the list is where the
  // reader is and sending them away to read one outcome is the navigation
  // every tracker learned not to make. `rowPeekHandler` is the frame's ONE
  // copy of which clicks mean "open elsewhere".
  const title = peek ? (
    <a className="t-link" href={peekHref(ref)} onClick={rowPeekHandler(() => peek(ref))}>
      {goal.name}
    </a>
  ) : (
    goal.name
  );
  return (
    <Panel
      title={title}
      icon="target"
      actions={
        <span className="row gap-2">
          {goal.group && <Badge outline>{goal.group}</Badge>}
          {goalFlags(goal)}
        </span>
      }
    >
      <div className="row gap-2 wrap" style={{ marginBottom: "var(--space-3)" }}>
        {goal.owners.map((handle) => (
          // THE CHIP SHOWS A NAME AND SENDS A HANDLE: the word is for the
          // reader and the value is the filter's.
          <Chip
            key={handle}
            onClick={() => onOwner(handle)}
            title={`Only ${seatName(handle)}'s goals`}
          >
            {seatName(handle)}
          </Chip>
        ))}
        {goal.due_at && (
          <span className="t-caption faint" title={fmtDate(goal.due_at)}>
            due {relTime(goal.due_at, now)}
          </span>
        )}
      </div>

      {goal.description && <p className="t-body">{goal.description}</p>}

      <GoalTargets goal={goal} />
    </Panel>
  );
}

/**
 * Where a goal is, and what says so.
 *
 * THE GOAL'S OWN BAR IS THE MEAN OF ITS TARGETS', unweighted — so two targets,
 * one enormous and one trivial, read as half done when the trivial one lands,
 * and the reader can see exactly which it was. A goal with no targets gets NO
 * bar, because "nothing has happened" and "there is nothing to measure" are
 * different facts.
 *
 * Shared by the list, the page and the rail, so the three cannot come to draw
 * a different number of bars for one goal.
 */
export function GoalTargets({ goal }: { goal: WorkGoal }) {
  const targets = goal.targets ?? [];
  if (goal.progress === undefined) {
    return (
      <p className="t-caption faint">
        No targets, so there is nothing to measure — which is not the same as nothing having
        happened.
      </p>
    );
  }
  return (
    <>
      <Meter
        fullMeans="achieved"
        used={Math.round(goal.progress * 100)}
        max={100}
        ariaLabel={`${goal.name} — overall progress`}
        label="Overall"
        right={`${Math.round(goal.progress * 100)}%`}
      />
      {targets.map((target) => (
        <TargetMeter key={target.id} target={target} />
      ))}
    </>
  );
}

export function TargetMeter({ target }: { target: WorkGoalTarget }) {
  if (target.progress === undefined) {
    return (
      <Meter
        fullMeans="achieved"
        used={0}
        max={100}
        ariaLabel={`${target.name} — progress`}
        label={target.name}
        right="—"
        tone="neutral"
      />
    );
  }
  return (
    <Meter
      fullMeans="achieved"
      used={Math.round(target.progress * 100)}
      max={100}
      ariaLabel={`${target.name} — progress`}
      label={target.name}
      right={targetRight(target)}
    />
  );
}

/** What a target says on its right, and it is DIFFERENT PER TYPE: a task
 *  target's "7 of 12" is the number a person acts on, where a percentage of it
 *  is the number they would have to divide back. */
function targetRight(target: WorkGoalTarget): string {
  if (target.type === "tasks") {
    return `${target.finished_tasks ?? 0} of ${target.total_tasks ?? 0}`;
  }
  if (target.type === "binary") {
    return target.done ? "done" : "not done";
  }
  const unit = target.unit ? ` ${target.unit}` : "";
  return `${target.current ?? 0}${unit} of ${target.goal ?? 0}${unit}`;
}

/**
 * Who a goal belongs to, as seats rather than as names.
 *
 * THE FACT LINE SAYS WHO AND THIS SAYS WHO THEY ARE — an avatar, whether the
 * seat is held by an agent or a person, and a link to them. On the list those
 * same handles are CHIPS, because there they are a filter over the goals on
 * screen; there is nothing to filter on a page about one goal, so here they
 * are what they actually are.
 */
export function GoalOwners({
  goal,
  seat,
}: {
  goal: WorkGoal;
  seat: (handle: string) => { name: string; kind?: "agent" | "human" };
}) {
  const owners = goal.owners ?? [];
  return (
    <Panel title="Owners" icon="users" count={owners.length} padding="none">
      {owners.length > 0 ? (
        <div className="list">
          {owners.map((handle) => {
            const who = seat(handle);
            return (
              <div key={handle} className="thread-entry">
                <SeatCell handle={handle} name={who.name} kind={who.kind} />
              </div>
            );
          })}
        </div>
      ) : (
        <Empty
          inline
          icon="users"
          title="Nobody owns this goal"
          hint="A goal with no owner is a goal nobody declares the health of — the check-in below is a person's judgement, and there is nobody here to make it."
        />
      )}
    </Panel>
  );
}

/** The hint under every "no such goal", on the page and in the rail alike. */
const NO_GOAL_HINT =
  "A goal is addressed by its uuid. It may have been removed, or this node's copy of the log may not have reached it yet.";

/**
 * One goal.
 *
 * `work_goals` has taken an `id` since it existed and `#/goals/{id}` fell
 * through to the list, because the route dispatch discarded `route.path[1]` —
 * so every link to a goal, from a target, from a check-in or from a colleague,
 * landed on every goal.
 *
 * A GOAL IS AN OBJECT, so the page opens with the same [ObjectHeader] its rail
 * does, over the same [goalFacts]: what was a panel header with the goal's
 * name, its group and its health in it is now the page's own header, and the
 * health badge the page bar used to publish is in the one place a state
 * belongs.
 *
 * The CHECK-IN FEED is the part a list cannot show: `updates[]` is the only
 * part of a goal a person writes in prose, each one carrying the health they
 * declared at the time, and it was served by the engine and declared by
 * nothing on this side.
 */
export function Goal({ id }: { id: string }) {
  const now = useNow();
  const viewer = useViewer();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const seat = seatLookup(index);
  const { data, loading, error } = useQuery("work_goals", id ? { id } : undefined, {
    enabled: id !== "",
    pollMs: 60_000,
  });
  const goal = data?.goals?.[0];
  // THE GOAL'S NAME IN THE CRUMB, not the uuid it is addressed by: a trail
  // reading `goals / 4f3c…` tells a reader nothing they did not already know
  // from the URL bar.
  usePageLabels(goal ? { [id]: goal.name } : {});

  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["goals"])}>
            All goals →
          </a>
        }
      </PageActions>
      <PageNote>
        {goal?.description ||
          "A goal is the tier above projects: what the company is trying to move, and what says whether it moved."}
      </PageNote>
      {loading && !goal && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}
        empty={goal ? undefined : { title: "No goal with that id", hint: NO_GOAL_HINT }}
      >
        {goal && (
          <>
            <ObjectHeader
              kind="Goal"
              icon="target"
              title={goal.name}
              status={goalFlags(goal)}
              facts={goalFacts({ goal, now, seatName })}
            />

            <Panel
              title="Targets"
              icon="check"
              count={goal.targets?.length ?? 0}
              subtitle="what says whether the outcome moved"
            >
              <GoalTargets goal={goal} />
            </Panel>

            <GoalOwners goal={goal} seat={seat} />

            <GoalCheckIns goal={goal} seatName={seatName} now={now} />
            <ToolCallBlock subject={{ kind: "goal", id }} viewer={viewer.handle} />
          </>
        )}
      </QueryState>
    </>
  );
}

/**
 * What somebody SAID about this goal, newest first.
 *
 * THE ONE PART OF A GOAL A PERSON WRITES. Health is a judgement and never
 * inferred, so each entry is somebody's reading at a moment with the reasoning
 * they gave for it — which is what the health badge in the header is a
 * one-word summary OF. Newest first, because a reader wants the current view
 * and then how it got there; the engine appends, so the stored list is
 * oldest-first and this reverses it.
 *
 * ONE COPY, shared by the page and the rail. The rail asks for `latest` — the
 * newest entry alone, and a line saying how many are behind it — because a
 * feed in a 420 px column is the page in a narrow one, and the reason the rail
 * exists is that the list behind it stays on screen. The entry ITSELF is the
 * same in both frames, which is the part that would have drifted: a check-in
 * whose health badge appeared on one screen and not the other is a goal with
 * two histories.
 */
export function GoalCheckIns({
  goal,
  seatName,
  now,
  latest,
}: {
  goal: WorkGoal;
  seatName: (handle: string) => string;
  now: number;
  latest?: boolean;
}) {
  const updates = [...(goal.updates ?? [])].reverse();
  const shown = latest ? updates.slice(0, 1) : updates;
  const behind = updates.length - shown.length;
  return (
    <Panel
      title={latest ? "Latest check-in" : "Check-ins"}
      icon="activity"
      // NO COUNT CHIP ON THE RAIL'S, because the panel deliberately shows one
      // of them: a chip reading 7 over a single entry is a panel that looks
      // like it failed to render the other six.
      count={latest ? undefined : updates.length}
      padding="none"
      subtitle="the only part of a goal a person writes"
    >
      {shown.length > 0 ? (
        <div className="list">
          {shown.map((update, i) => (
            <div key={`${update.at}-${i}`} className="thread-entry">
              <div className="row gap-1">
                <strong className="t-cell">{seatName(update.author)}</strong>
                {update.health && HEALTH[update.health] && (
                  <Badge tone={HEALTH[update.health]!.tone}>{HEALTH[update.health]!.label}</Badge>
                )}
                <span className="spacer" />
                <span className="t-caption" title={update.at}>
                  {relTime(update.at, now)}
                </span>
              </div>
              {update.text && <p className="t-caption">{update.text}</p>}
            </div>
          ))}
          {/* WHAT IS BEHIND IT, never silence: one entry with nothing said
              about the rest reads as a goal checked in on once, which is a
              different goal from one checked in on weekly. */}
          {behind > 0 && (
            <div className="thread-entry t-caption faint">
              {plural(behind, "earlier check-in")} — the goal&rsquo;s own page has the history.
            </div>
          )}
        </div>
      ) : (
        <div className="thread-entry t-caption faint">
          Nobody has checked in on this goal. A check-in is a person declaring its health and saying
          why — the engine never infers one from progress.
        </div>
      )}
    </Panel>
  );
}

/**
 * One goal, in the rail.
 *
 * # It reads its own goal
 *
 * A peek is not a projection of the row that opened it: a list row carries
 * what the list needed, and this is opened from a pasted URL as often as from
 * a panel. `work_goals` has taken an `id` since it existed, so asking for one
 * is the same question the list makes with one more key — which is what keeps
 * the rail's figures and the list's from ever being two computations.
 *
 * # Three panels, and none of them is the page
 *
 * The header already carries who owns the goal, where it is and how far along
 * it is. What is left is what a percentage cannot answer — what the outcome
 * actually is, WHICH targets the number is the mean of, and what the last
 * person to look at it SAID — and that is where it stops: the whole check-in
 * feed, the owners as seats and the ToolCallBlock are the page's, because a
 * rail that grew them would be the page in a narrow column and the reason the
 * rail exists is that the list behind it stays on screen.
 *
 * THE LATEST CHECK-IN RATHER THAN THE OWNERS, which the header's own `Owner`
 * fact already names. Health is the one thing on this object nobody can derive
 * — the badge beside the title is a person's judgement — so the entry that
 * MADE that judgement, with the reasoning attached, is what turns the badge
 * from a colour into an answer.
 */
export function GoalPeek({ id }: { id: string }) {
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const { data, loading, error } = useQuery("work_goals", id ? { id } : undefined, {
    enabled: id !== "",
    pollMs: 60_000,
  });
  const goal = data?.goals?.[0];

  return (
    <>
      {loading && !data && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading}>
        {data &&
          (goal ? (
            <>
              <ObjectHeader
                size="peek"
                kind="Goal"
                icon="target"
                title={goal.name}
                status={goalFlags(goal)}
                facts={goalFacts({ goal, now, seatName })}
              />
              <div className="col gap-3">
                {/* WHAT IT IS FOR, always drawn — including when nobody wrote
                    one. "Is this the one I meant" is the question the rail
                    answers, and a description that is simply missing from the
                    panel reads as one the reader failed to scroll to. */}
                <Panel title="Outcome" icon="target">
                  {goal.description ? (
                    <p className="t-body measure">{goal.description}</p>
                  ) : (
                    <span className="muted">No description is written for this goal.</span>
                  )}
                </Panel>

                <Panel
                  title="Targets"
                  icon="check"
                  count={goal.targets?.length ?? 0}
                  subtitle="what says whether the outcome moved"
                >
                  <GoalTargets goal={goal} />
                </Panel>

                <GoalCheckIns goal={goal} seatName={seatName} now={now} latest />
              </div>
            </>
          ) : (
            // NOT AN EMPTY PANEL, and not a spinner that never resolves. A
            // goal id reaches this from a pasted URL and from a bookmark as
            // often as from a row, so the honest answer names the id that
            // resolved to nothing and says what its absence means.
            <Empty inline icon="target" title={`No goal with id “${id}”`} hint={NO_GOAL_HINT} />
          ))}
      </QueryState>
    </>
  );
}
