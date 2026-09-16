/**
 * Goals, the tier above projects.
 *
 * # The percentage is not stored anywhere
 *
 * A goal is at what its TARGETS say, and the engine computes that on every
 * read from the rows the read already holds. A stored number would be a second
 * answer to a question that already has one, and the two drift the moment a
 * task in a target closes without anybody editing the goal, which is the
 * ordinary case rather than the exotic one. So this screen renders a number
 * that is always as fresh as the work behind it.
 *
 * # Health is a person's judgement, and progress is not
 *
 * The targets say what has MOVED. Only an owner knows whether the movement is
 * on track, which is why health is a field somebody sets rather than a colour
 * derived from the bar: a goal at 90% with a week left and a goal at 90% with
 * a day left are different situations, and no arithmetic separates them.
 *
 * That is also why every bar here is drawn in the ordinary reading rather than
 * left to the design system's own derivation. A meter escalates to warning
 * near its ceiling and to danger at it, which is right for a budget and
 * exactly wrong here: a goal that has reached its target is finished, not on
 * fire, and a bar that reddens as the work lands would be the screen making
 * the judgement this paragraph says it must not make.
 *
 * # Read-only, like the board
 *
 * Nothing here writes. A goal is set through the operator MCP surface, which
 * is attributed to a credential, where a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useParam } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import {
  Card,
  EmptyValue,
  FilterChip,
  Inline,
  Meter,
  PageHeader,
  Select,
  Skeleton,
  Stack,
  StatCard,
  StatGroup,
  Tag,
  Toolbar,
  RelativeTime,
  useNow,
  VisuallyHidden,
} from "@crewlethq/ui";
import { CheckGlyph, TargetGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import type { Tone } from "@crewlethq/ui";
import type { WorkGoal, WorkGoalTarget } from "~/protocol/index.ts";

/** The health values, and their tone. A closed set, so one the engine adds
 *  later renders as itself rather than vanishing. */
const HEALTH: Record<string, { label: string; tone: Tone }> = {
  on_track: { label: "On track", tone: "success" },
  at_risk: { label: "At risk", tone: "warning" },
  off_track: { label: "Off track", tone: "danger" },
  done: { label: "Done", tone: "success" },
};

export function Goals() {
  const now = useNow();
  const [group, setGroup] = useParam("group", "");
  const [owner, setOwner] = useParam("owner", "");
  const [archived, setArchived] = useParam("archived", "");

  const params: Record<string, unknown> = {};
  if (group) params.group = group;
  if (owner) params.owner = owner;
  if (archived) params.archived = true;

  // A goal changes when somebody edits it or when work under it moves, and
  // neither is pushed to this socket, so a poll is correct. Sixty seconds: a
  // goal is a quarter's question, not a minute's.
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
  const showArchived = archived === "true";

  return (
    <>
      <PageHeader
        title="Goals"
        description="The tier above projects: an outcome, who owns it, and what its targets say. The percentage is computed from the work every time it is read, so nothing stores one and nothing can disagree with the board."
        badges={<Tag appearance="outline">{plural(goals.length, "goal")}</Tag>}
      />

      <Toolbar label="Goal filters" mode="group" sticky>
        <Select
          ariaLabel="Group"
          value={group}
          active={group !== ""}
          width="auto"
          options={[
            // THE CLEAR IS A ROW IN THE LIST rather than a placeholder the
            // control falls back to: a placeholder cannot be chosen, so a
            // reader who narrowed to one group would have no way back to all
            // of them from the control they narrowed with.
            { value: "", label: "Every group" },
            ...groups.map((name) => ({ value: name, label: name })),
          ]}
          onChange={(value) => setGroup(String(value))}
        />
        <FilterChip
          pressed={showArchived}
          onPressedChange={(on) => setArchived(on ? "true" : "")}
          title="Include archived goals"
        >
          Archived
        </FilterChip>
        {owner && (
          <FilterChip pressed onPressedChange={() => setOwner("")} title="Clear this filter">
            {owner}
          </FilterChip>
        )}
      </Toolbar>

      {!loading && !error && goals.length > 0 && (
        <StatGroup columns={3}>
          <StatCard
            icon={<TargetGlyph />}
            label="Goals"
            value={goals.length}
            // WHAT THE LIST IS, not what it usually is. The archived filter is
            // one press away and the previous wording said "not archived"
            // whichever way it was set, so the tile contradicted the chip.
            sub={showArchived ? "archived goals included" : "not archived"}
          />
          <StatCard
            icon={<WarningGlyph />}
            label="At risk or off track"
            value={atRisk.length}
            sub={atRisk.length ? atRisk.map((g) => g.name).join(", ") : "nobody has raised one"}
            // THE ONE TILE HERE THAT IS AN OUTCOME, and only while it has
            // something to report. A count of nothing is not a state.
            {...(atRisk.length ? { tone: "warning" as const } : {})}
          />
          <StatCard
            icon={<CheckGlyph />}
            label="Targets"
            value={goals.reduce((n, g) => n + (g.targets?.length ?? 0), 0)}
            sub="across every goal on screen"
          />
        </StatGroup>
      )}

      {loading && !goals.length && (
        <Skeleton label="Loading the company's goals" variant="text" rows={4} />
      )}

      <QueryState
        error={error}
        loading={loading}
        empty={
          goals.length
            ? undefined
            : {
                title: "No goals are set",
                hint: "A goal is an outcome with targets under it: a project, a set of tasks, a number. Set one through your assistant's Crewlet tools, or widen these filters.",
              }
        }
      >
        {goals.map((goal) => (
          <GoalPanel key={goal.id} goal={goal} now={now} owner={owner} onOwner={setOwner} />
        ))}
      </QueryState>
    </>
  );
}

function GoalPanel({
  goal,
  now,
  owner,
  onOwner,
}: {
  goal: WorkGoal;
  now: number;
  /** The handle the list is narrowed to, so a chip can show it is the one. */
  owner: string;
  onOwner: (handle: string) => void;
}) {
  const health = HEALTH[goal.health ?? ""];
  const percent = goal.progress === undefined ? 0 : Math.round(goal.progress * 100);
  return (
    <Card as="section">
      <Card.Header
        icon={<TargetGlyph size="sm" />}
        actions={
          <>
            {goal.group && <Tag appearance="outline">{goal.group}</Tag>}
            {/* NO TAG FOR AN UNSET HEALTH, and that is not an omission: a
                fresh goal nobody has judged is not "on track", and rendering
                it as anything would be the screen making the judgement. */}
            {health && <Tag variant={health.tone}>{health.label}</Tag>}
            {goal.archived && <Tag appearance="outline">Archived</Tag>}
          </>
        }
      >
        <Card.Title>{goal.name}</Card.Title>
      </Card.Header>

      <Stack gap={3}>
        <Inline gap={2} wrap align="center">
          {goal.owners.map((handle) => (
            <FilterChip
              key={handle}
              pressed={owner === handle}
              onPressedChange={(on) => onOwner(on ? handle : "")}
              title={`Only ${handle}'s goals`}
            >
              {handle}
            </FilterChip>
          ))}
          {goal.due_at && (
            <span className="t-caption">
              due <RelativeTime value={goal.due_at} now={now} />
            </span>
          )}
        </Inline>

        {goal.description && <p className="t-body">{goal.description}</p>}

        {/* THE GOAL'S OWN BAR IS THE MEAN OF ITS TARGETS', unweighted, so two
            targets, one enormous and one trivial, read as half done when the
            trivial one lands, and the reader can see exactly which it was. A
            goal with no targets gets NO bar, because "nothing has happened"
            and "there is nothing to measure" are different facts. */}
        {goal.progress === undefined ? (
          <p className="t-caption">
            No targets, so there is nothing to measure, which is not the same as nothing having
            happened.
          </p>
        ) : (
          <Meter
            value={percent}
            max={100}
            label={
              <>
                Overall
                {/* WHICH GOAL, for a reader who lands on the bar rather than
                    on the card heading above it. The label is the meter's
                    accessible name, so this is the only place it can be said. */}
                <VisuallyHidden> progress towards {goal.name}</VisuallyHidden>
              </>
            }
            valueText={`${percent}% of this goal's targets`}
            hint={`${percent}%`}
            tone="brand"
          />
        )}

        {(goal.targets ?? []).map((target) => (
          <TargetMeter key={target.id} target={target} />
        ))}
      </Stack>
    </Card>
  );
}

function TargetMeter({ target }: { target: WorkGoalTarget }) {
  if (target.progress === undefined) {
    return (
      <Meter
        value={0}
        max={100}
        label={target.name}
        // NOT A READING OF ZERO. This target measures nothing at all, and a
        // bar drawn in the ordinary tone would say the work has not started.
        tone="neutral"
        valueText="nothing to measure"
        hint={<EmptyValue label="Nothing to measure" />}
      />
    );
  }
  const reading = targetRight(target);
  return (
    <Meter
      value={Math.round(target.progress * 100)}
      max={100}
      label={target.name}
      valueText={reading}
      // Progress, never a verdict. See the note at the top of this file.
      tone="brand"
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
