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
 * # Read-only, like the board
 *
 * Nothing here writes. A goal is set through the operator MCP surface, which
 * is attributed to a credential — where a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { QueryState } from "~/components/common.tsx";
import { Badge, Chip, Meter, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useParam } from "~/app/router.tsx";
import { fmtDate, plural, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkGoal, WorkGoalTarget } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

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
  const goals = data?.goals ?? [];

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
}: {
  goal: WorkGoal;
  now: number;
  onOwner: (h: string) => void;
  seatName: (handle: string) => string;
}) {
  const health = HEALTH[goal.health ?? ""];
  return (
    <Panel
      title={goal.name}
      icon="target"
      actions={
        <span className="row gap-2">
          {goal.group && <Badge outline>{goal.group}</Badge>}
          {/* NO BADGE FOR AN UNSET HEALTH, and that is not an omission: a
              fresh goal nobody has judged is not "on track", and rendering
              it as anything would be the screen making the judgement. */}
          {health && <Badge tone={health.tone}>{health.label}</Badge>}
          {goal.archived && <Badge outline>Archived</Badge>}
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

      {/* THE GOAL'S OWN BAR IS THE MEAN OF ITS TARGETS', unweighted — so two
          targets, one enormous and one trivial, read as half done when the
          trivial one lands, and the reader can see exactly which it was. A
          goal with no targets gets NO bar, because "nothing has happened" and
          "there is nothing to measure" are different facts. */}
      {goal.progress === undefined ? (
        <p className="t-caption faint">
          No targets, so there is nothing to measure — which is not the same as nothing having
          happened.
        </p>
      ) : (
        <Meter
          fullMeans="achieved"
          used={Math.round(goal.progress * 100)}
          max={100}
          ariaLabel={`${goal.name} — overall progress`}
          label="Overall"
          right={`${Math.round(goal.progress * 100)}%`}
        />
      )}

      {(goal.targets ?? []).map((target) => (
        <TargetMeter key={target.id} target={target} />
      ))}
    </Panel>
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
 * One goal.
 *
 * `work_goals` has taken an `id` since it existed and `#/goals/{id}` fell
 * through to the list, because the route dispatch discarded `route.path[1]` —
 * so every link to a goal, from a target, from a check-in or from a colleague,
 * landed on every goal.
 *
 * The CHECK-IN FEED is the part a list cannot show: `updates[]` is the only
 * part of a goal a person writes in prose, each one carrying the health they
 * declared at the time, and it was served by the engine and declared by
 * nothing on this side.
 */
export function Goal({ id }: { id: string }) {
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const { data, loading, error } = useQuery("work_goals", { id }, { pollMs: 60_000 });
  const goal = data?.goals?.[0];

  return (
    <>
      <PageActions>
        {goal?.health && HEALTH[goal.health] ? (
          <Badge tone={HEALTH[goal.health]!.tone}>{HEALTH[goal.health]!.label}</Badge>
        ) : undefined}
      </PageActions>
      <PageNote>
        {goal?.description ||
          "A goal is the tier above projects: what the company is trying to move, and what says whether it moved."}
      </PageNote>
      {loading && !goal && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}

        empty={
          goal
            ? undefined
            : {
                title: "No goal with that id",
                hint: "A goal is addressed by its uuid. It may have been removed, or this node's copy of the log may not have reached it yet.",
              }
        }
      >
        {goal && (
          <>
            <GoalPanel goal={goal} now={now} onOwner={() => {}} seatName={seatName} />

            {/* THE CHECK-IN HISTORY. Health is set by a PERSON and never
                inferred, so each entry is somebody's judgement at a moment,
                with the reasoning they gave for it — newest first here,
                because a reader wants the current view and then how it got
                there. The engine appends, so the list itself is oldest-first. */}
            <Panel
              title="Check-ins"
              icon="activity"
              count={goal.updates?.length ?? 0}
              padding="none"
              subtitle="the only part of a goal a person writes"
            >
              {goal.updates?.length ? (
                <div className="list">
                  {[...goal.updates].reverse().map((update, i) => (
                    <div key={`${update.at}-${i}`} className="thread-entry">
                      <div className="row gap-1">
                        <strong className="t-cell">{seatName(update.author)}</strong>
                        {update.health && HEALTH[update.health] && (
                          <Badge tone={HEALTH[update.health]!.tone}>
                            {HEALTH[update.health]!.label}
                          </Badge>
                        )}
                        <span className="spacer" />
                        <span className="t-caption" title={update.at}>
                          {relTime(update.at, now)}
                        </span>
                      </div>
                      {update.text && <p className="t-caption">{update.text}</p>}
                    </div>
                  ))}
                </div>
              ) : (
                <div className="thread-entry t-caption faint">
                  Nobody has checked in on this goal. A check-in is a person declaring its health
                  and saying why — the engine never infers one from progress.
                </div>
              )}
            </Panel>
          </>
        )}
      </QueryState>
    </>
  );
}
