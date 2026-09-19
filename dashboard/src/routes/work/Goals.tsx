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
import {
  Card,
  EmptyState,
  EmptyValue,
  FilterChip,
  Select,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  GroupGlyph,
  TargetGlyph,
  TimelineGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, AND THE THREE THINGS THEIR `Meter` CANNOT DO. Every bar on this screen
// reads FULL AS ACHIEVED, and `meterTone` has the opposite polarity welded in
// (>= 100% is `danger`), so a finished goal would draw red. Their legend is
// also all-or-nothing — `hideLabel` hides the value text with the label — so
// the capacity-style bars elsewhere in this workspace cannot exist at all, and
// their meter keeps `role="meter"` with no scale, announcing "0 of 100" where
// nobody has said what the limit is. See the report.
import { Meter } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { href, useParam } from "~/app/router.tsx";
import { fmtDateTime, plural, relTime } from "~/lib/format.ts";
// THE WORKSPACE'S ONE DUE-DATE RENDERER — the same mark a board card, a list
// row, a table cell and a task's own fact line draw. A goal's deadline is read
// against the work under it, so a second spelling of it here is a second rule.
import { DueMark } from "~/components/work.tsx";
import { useNow } from "~/lib/clock.ts";
import type { WorkGoal, WorkGoalTarget } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ToolCallBlock } from "~/components/ToolCall.tsx";
import { healthLabel } from "~/lib/work.ts";

/**
 * The TONE each health value carries.
 *
 * The WORDS are `lib/work.ts`'s — a `health` delta renders the same four in a
 * sentence, and two copies of one vocabulary is how one of them stops matching.
 * A value with no tone wears no badge, which is what an unjudged goal is.
 */
const HEALTH_TONE: Record<string, "success" | "warning" | "danger" | "neutral"> = {
  on_track: "success",
  at_risk: "warning",
  off_track: "danger",
  done: "success",
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
  const tone = HEALTH_TONE[goal.health ?? ""];
  if (!tone && !goal.archived) return undefined;
  return (
    <span className="row gap-1">
      {tone && <Tag variant={tone}>{healthLabel(goal.health ?? "")}</Tag>}
      {goal.archived && <Tag appearance="outline">Archived</Tag>}
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
/**
 * EXPORTED for the suite. The page and the rail both read this, so a fix
 * applied to the panel alone would leave the header still counting days — and
 * reaching it through either frame means a Router and a live query for the sake
 * of one span.
 */
export function goalFacts({
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
          <EmptyValue label="Nobody owns this goal" />
        ),
      // ONLY WHERE THERE IS ONE PLACE TO GO. A list of three names cannot be
      // one link, and linking the whole line to the first owner would send a
      // reader to somebody they did not click on.
      path: owners.length === 1 && first ? ["company", "people", first] : undefined,
    },
    // A GROUP IS IDENTITY, so it is a plain word rather than a toned badge —
    // colour in this product says what state a thing is in and nothing else.
    { label: "Group", value: goal.group || <EmptyValue label="This goal is in no group" /> },
    {
      label: "Due",
      // A DATE SOMEBODY CHOSE, DRAWN AS A DATE. This read "in 19d" beside a
      // board whose tasks read "Sep 19", and the date itself was on a `title` —
      // a hover, absent on touch — because `relTime` hands a future instant to
      // `inTime`, which never reaches an absolute date going FORWARD at all. A
      // reader cannot line a goal up against the work under it by subtracting
      // days from today.
      //
      // THE WORKSPACE'S ONE DUE RENDERER: the same mark a board card, a list
      // row, a table cell and a task's own fact line draw. A goal's deadline is
      // read against the work under it, so a second spelling of it here is a
      // second rule and the reader is the one who has to reconcile them.
      //
      // NO `overdue` FLAG, exactly as a task's own fact line passes none:
      // overdue is DERIVED by the engine against the company's day start and
      // rides on the rows that carry it, and a goal row carries no such field. A
      // browser re-deriving it here is how one screen calls a thing late and the
      // next does not.
      //
      // THE DASH STAYS for an unset date: the five facts are read in one order
      // wherever the object appears, so dropping the row would reorder the line
      // on exactly the goals nobody has dated.
      value: goal.due_at ? (
        <DueMark due={goal.due_at} now={now} />
      ) : (
        <EmptyValue label="No date is set on this goal" />
      ),
    },
    {
      label: "Progress",
      // ABSENT IS NOT ZERO, and here the difference is a goal somebody
      // escalates: "there is nothing to measure" and "nothing has happened"
      // are different facts, and a goal with no targets has the first.
      value:
        goal.progress === undefined ? (
          <EmptyValue label="No targets, so there is nothing to measure" />
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

  // THE TILE FOLDS TWO HEALTH VALUES, so its caption is the SPLIT rather than a
  // list of the goals in it. `sub` is ONE line — the design system draws it
  // `white-space: nowrap` with `text-overflow: ellipsis` — and a goal name is
  // founder prose the engine bounds at 256 characters (`tracker.MaxGoalName`),
  // so a join of them is cut mid-word and prints a goal nobody wrote: it shipped
  // reading "Console reads honestly under uncertainty, Console reads hon…".
  //
  // CAPPING THE LIST IS THE TEMPTING WRONG FIX — one 256-character name already
  // overflows a tile that is a third of a content column, so a cap moves the
  // threshold and keeps the mid-word cut. WHICH goals is the list below's
  // question, and every row there already wears its own health badge.
  //
  // Both halves print even at zero, so the caption's SHAPE does not move with
  // the data: "4 at risk, 0 off track" says the tile folds exactly two states,
  // where dropping the empty half would read like a list again.
  const atRisk = goals.filter((g) => g.health === "at_risk");
  const offTrack = goals.filter((g) => g.health === "off_track");
  const raised = atRisk.length + offTrack.length;

  return (
    <>
      <PageActions>{<Tag appearance="outline">{plural(goals.length, "goal")}</Tag>}</PageActions>
      <PageNote>
        The tier above projects: an outcome, who owns it, and what its targets say. The percentage
        is computed from the work every time it is read — nothing stores one, so nothing can
        disagree with the board.
      </PageNote>

      {!loading && !error && goals.length > 0 && (
        <StatGroup columns={3}>
          <StatCard
            icon={<TargetGlyph size="xs" />}
            label="Goals"
            value={goals.length}
            sub="not archived"
          />
          <StatCard
            icon={<WarningGlyph size="xs" />}
            label="At risk or off track"
            value={raised}
            sub={
              raised
                ? `${atRisk.length} at risk, ${offTrack.length} off track`
                : "nobody has raised one"
            }
          />
          <StatCard
            icon={<CheckGlyph size="xs" />}
            label="Targets"
            value={goals.reduce((n, g) => n + (g.targets?.length ?? 0), 0)}
            sub="across every goal on screen"
          />
        </StatGroup>
      )}

      <div className="toolbar">
        {/* THE "ANY" ROW IS AN OPTION RATHER THAN A PLACEHOLDER: their
            `placeholder` only labels the empty trigger, and a reader who has
            chosen a group needs a row to choose their way back out of it. */}
        <Select
          // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
          // `width: 100%` unless told otherwise, and its own doc says why that
          // is wrong here: "a filter row of full-width selects is one question
          // per line, which is not what a filter bar is".
          width="auto"
          value={group}
          onChange={(value) => setGroup(String(value))}
          ariaLabel="Group"
          active={group !== ""}
          options={[
            { value: "", label: "Every group" },
            ...groups.map((name) => ({ value: name, label: name })),
          ]}
        />
        <FilterChip
          pressed={archived === "true"}
          onClick={() => setArchived(archived ? "" : "true")}
          title="Include archived goals"
        >
          Archived
        </FilterChip>
        {owner && (
          <FilterChip pressed onClick={() => setOwner("")} title="Clear this filter">
            {index.byHandle.get(owner)?.name ?? owner}
          </FilterChip>
        )}
      </div>

      {loading && !goals.length && <Skeleton variant="text" rows={4} label="Loading goals" />}

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
    <Card>
      <Card.Header
        icon={<TargetGlyph size="sm" />}
        actions={
          <span className="row gap-2">
            {goal.group && <Tag appearance="outline">{goal.group}</Tag>}
            {goalFlags(goal)}
          </span>
        }
      >
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <div className="row gap-2 wrap" style={{ marginBottom: "var(--space-3)" }}>
        {goal.owners.map((handle) => (
          // THE CHIP SHOWS A NAME AND SENDS A HANDLE: the word is for the
          // reader and the value is the filter's. `FilterChip` rather than a
          // plain `Tag`, because this is what a filter toggle IS over there —
          // and it carries the button, the `aria-pressed` and the hit area
          // our own `Chip` had to spell out.
          <FilterChip
            key={handle}
            onClick={() => onOwner(handle)}
            title={`Only ${seatName(handle)}'s goals`}
          >
            {seatName(handle)}
          </FilterChip>
        ))}
        {/* THE SAME MARK THE FACT LINE AND A BOARD CARD'S FOOT DRAW. The word
            "due" beside the calendar mark would be a second spelling of what the
            mark already says, and this row IS a card foot: chips, then the
            compact date, exactly as `work-card-foot` has it. `DueMark` renders
            nothing for an absent date, so the guard goes with it. */}
        <DueMark due={goal.due_at} now={now} />
      </div>

      {goal.description && <p className="t-body">{goal.description}</p>}

      <GoalTargets goal={goal} />
    </Card>
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
      <p className="t-caption">
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
        // NOT A DASH. A target nothing has measured yet is an absence with a
        // reading, and the mark alone is announced as "dash" or skipped.
        right={<EmptyValue label="No progress measured yet" />}
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
    <Card padding="none">
      <Card.Header icon={<GroupGlyph size="sm" />} count={owners.length}>
        <Card.Title>Owners</Card.Title>
      </Card.Header>
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
        <EmptyState
          size="compact"
          icon={<GroupGlyph size="xl" />}
          title="Nobody owns this goal"
          description="A goal with no owner is a goal nobody declares the health of — the check-in below is a person's judgement, and there is nobody here to make it."
        />
      )}
    </Card>
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
      {loading && !goal && <Skeleton variant="text" rows={4} label="Loading the goal" />}
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

            <Card>
              <Card.Header
                icon={<CheckGlyph size="sm" />}
                count={goal.targets?.length ?? 0}
                subtitle="what says whether the outcome moved"
              >
                <Card.Title>Targets</Card.Title>
              </Card.Header>
              <GoalTargets goal={goal} />
            </Card>

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
    <Card padding="none">
      <Card.Header
        icon={<TimelineGlyph size="sm" />}
        // NO COUNT CHIP ON THE RAIL'S, because the panel deliberately shows
        // one of them: a chip reading 7 over a single entry is a panel that
        // looks like it failed to render the other six.
        count={latest ? undefined : updates.length}
        subtitle="the only part of a goal a person writes"
      >
        <Card.Title>{latest ? "Latest check-in" : "Check-ins"}</Card.Title>
      </Card.Header>
      {shown.length > 0 ? (
        <div className="list">
          {shown.map((update, i) => (
            <div key={`${update.at}-${i}`} className="thread-entry">
              <div className="row gap-1">
                <strong className="t-cell">{seatName(update.author)}</strong>
                {update.health && HEALTH_TONE[update.health] && (
                  <Tag variant={HEALTH_TONE[update.health]!}>{healthLabel(update.health)}</Tag>
                )}
                <span className="spacer" />
                {/* THE FORMATTER, never the raw stamp. This was the only
                    `title` in the dashboard holding an ISO string straight off
                    the wire, so a check-in's hover read
                    `2031-04-16T00:00:00Z` — UTC, in neither the reader's zone
                    nor their date format, on a screen where every other tooltip
                    honours both. */}
                <span className="t-caption" title={fmtDateTime(update.at)}>
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
            <div className="thread-entry t-caption">
              {plural(behind, "earlier check-in")} — the goal&rsquo;s own page has the history.
            </div>
          )}
        </div>
      ) : (
        <div className="thread-entry t-caption">
          Nobody has checked in on this goal. A check-in is a person declaring its health and saying
          why — the engine never infers one from progress.
        </div>
      )}
    </Card>
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
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the goal" />}
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
                <Card>
                  <Card.Header icon={<TargetGlyph size="sm" />}>
                    <Card.Title>Outcome</Card.Title>
                  </Card.Header>
                  {goal.description ? (
                    <p className="t-body measure">{goal.description}</p>
                  ) : (
                    <span className="muted">No description is written for this goal.</span>
                  )}
                </Card>

                <Card>
                  <Card.Header
                    icon={<CheckGlyph size="sm" />}
                    count={goal.targets?.length ?? 0}
                    subtitle="what says whether the outcome moved"
                  >
                    <Card.Title>Targets</Card.Title>
                  </Card.Header>
                  <GoalTargets goal={goal} />
                </Card>

                <GoalCheckIns goal={goal} seatName={seatName} now={now} latest />
              </div>
            </>
          ) : (
            // NOT AN EMPTY PANEL, and not a spinner that never resolves. A
            // goal id reaches this from a pasted URL and from a bookmark as
            // often as from a row, so the honest answer names the id that
            // resolved to nothing and says what its absence means.
            <EmptyState
              size="compact"
              icon={<TargetGlyph size="xl" />}
              title={`No goal with id “${id}”`}
              description={NO_GOAL_HINT}
            />
          ))}
      </QueryState>
    </>
  );
}
