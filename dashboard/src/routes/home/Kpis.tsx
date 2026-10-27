/**
 * The five figures that open the landing screen — true whatever the queue
 * holds, which is what stops a healthy company rendering as a blank page.
 *
 * EACH TILE WAITS FOR ITS OWN ANSWER and says so (the kit's loading line and
 * `aria-busy`), and a tile whose answer was refused says the refusal: a
 * number that has not arrived is a different fact from a zero, and a dash
 * that looks like a measured nothing is the one thing a tile must not draw.
 */

import type { Service, ServicesRun } from "~/app/nav.ts";
import { Fragment } from "react";
import {
  ButtonLink,
  EMPTY_VALUE,
  Meter,
  SegmentedMeter,
  Sparkline,
  StatCard,
  type MeterState,
} from "@crewlethq/ui";
import { ArrowRightGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { decisionsHref } from "~/components/DecisionRow.tsx";
import { fmtCount } from "~/lib/format.ts";
import { useOrgBudget, useOrg } from "~/lib/store-hooks.ts";
import { mayChangeConfig } from "~/lib/useWriteAccess.ts";
import { WORKLET, accessNames } from "~/lib/access.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { TokenSeries, WorkFlowAnswer } from "~/protocol/index.ts";
import {
  budgetCaption,
  completedIn,
  crewParts,
  deltaWords,
  inProgressParts,
  troubleParts,
  waitedFor,
  weekWindow,
  type Crew,
  type HomeRange,
  type Waiting,
} from "./model.ts";
import { remedyDestination } from "~/lib/remedy.ts";

/**
 * The five figures, in the order the row draws them by default. A board's
 * Figure widget names one of them by this id, so an id is never renamed.
 */
export const KPI_IDS = ["agents", "waiting", "progress", "completed", "tokens"] as const;
export type KpiId = (typeof KPI_IDS)[number];

/**
 * The figures a service built into Crewlet answers, and which: tasks in
 * progress and tasks completed are Worklet's flow (`work_flow`). Where
 * the company does not run it they are not drawn, and the rest fill the row.
 * What waits on the reader's decision is not among them: the seats stopped on
 * a spent budget and the coding runs parked on a question arrive whatever
 * runs the tracker.
 */
export const KPI_SERVICES: Readonly<Partial<Record<KpiId, Service>>> = {
  progress: "tracker",
  completed: "tracker",
};

/** The figures a company running `runs` is shown, in the row's order. */
export function kpisShown(figures: readonly KpiId[], runs: ServicesRun): KpiId[] {
  return figures.filter((id) => {
    const service = KPI_SERVICES[id];
    return service === undefined || runs[service];
  });
}

/** What each figure is called where it is offered on its own. */
export const KPI_LABELS: Record<KpiId, string> = {
  agents: "Agents working now",
  waiting: "Waiting on your decision",
  progress: "Tasks in progress",
  completed: "Completed",
  tokens: "Tokens",
};

/** How many days of the series the in-progress sparkline draws, and how far
 *  back its delta compares: a week. */
const WEEK = 7;

export function Kpis({
  figures = KPI_IDS,
  range,
  crew,
  viewer,
  waiting,
  waitingError,
  flow,
  spend,
  now,
}: {
  /** The figures to draw, in this order: all five unless a widget names fewer. */
  figures?: readonly KpiId[];
  range: HomeRange;
  crew: Crew;
  viewer: ViewerState;
  waiting: Waiting | null;
  waitingError: QueryErrorCode | null;
  flow: QueryResult<WorkFlowAnswer>;
  spend: QueryResult<TokenSeries>;
  now: number;
}) {
  const tile = (id: KpiId) => {
    switch (id) {
      case "agents":
        return <AgentsTile crew={crew} />;
      case "waiting":
        return <WaitingTile viewer={viewer} waiting={waiting} error={waitingError} now={now} />;
      case "progress":
        return <InProgressTile flow={flow} locked={workletLocked(viewer)} />;
      case "completed":
        return <CompletedTile flow={flow} range={range} locked={workletLocked(viewer)} />;
      case "tokens":
        // WHO MAY SET A BUDGET is whoever may write the company's configuration
        // (Admin, the access a ceiling is written under, and on a managed
        // document one of its writers), never a kind of credential.
        return <TokensTile spend={spend} range={range} mayConfigure={mayChangeConfig(viewer)} />;
    }
  };
  // THE ROW'S COLUMNS ARE ITS FIGURES', so one figure on its own fills its
  // cell rather than a fifth of it (`data-count`, `screens.css`).
  return (
    <section
      className="home-kpis"
      data-count={figures.length}
      aria-label={figures.length === 1 ? KPI_LABELS[figures[0]!] : "The company at a glance"}
    >
      {figures.map((id) => (
        <Fragment key={id}>{tile(id)}</Fragment>
      ))}
    </section>
  );
}

/** Seats working now, of every agent seat, with the state bar. */
function AgentsTile({ crew }: { crew: Crew }) {
  return (
    <StatCard
      label="Agents working now"
      value={crew.working.toLocaleString()}
      unit={`/ ${crew.total.toLocaleString()}`}
      trend={
        crew.total > 0 ? (
          <SegmentedMeter
            decorative
            total={crew.total}
            remainderLabel="idle"
            segments={[
              { id: "working", value: crew.working, tone: "info", label: "working" },
              { id: "needs", value: crew.needs, tone: "warning", label: "waiting" },
              { id: "stopped", value: crew.stopped, tone: "danger", label: "paused or stopped" },
            ]}
          />
        ) : undefined
      }
      sub={<Parts parts={crewParts(crew)} />}
    />
  );
}

/** What waits on the reader's decision, the oldest of it, and the way in. */
function WaitingTile({
  viewer,
  waiting,
  error,
  now,
}: {
  viewer: ViewerState;
  waiting: Waiting | null;
  error: QueryErrorCode | null;
  now: number;
}) {
  // THE INBOX IS WORKLET'S, so the way there is offered only to a reader
  // who can open it; an asked person without Worklet answers on Home and on
  // the task itself.
  const review = viewer.worklet ? (
    <ButtonLink
      size="small"
      variant="secondary"
      href={decisionsHref()}
      trailingIcon={<ArrowRightGlyph size="sm" />}
    >
      Review
    </ButtonLink>
  ) : undefined;
  // NOBODY TO WAIT ON: an anonymous reader has no record and no decisions,
  // and saying 0 would be a claim about a person nobody named. An UNBOUND
  // reader is somebody — their record is their login, and what they asked or
  // were asked reaches it — so they are counted like anybody.
  if (!viewer.owner) {
    return (
      <StatCard
        label="Waiting on your decision"
        value={EMPTY_VALUE}
        loading={viewer.loading}
        sub={viewer.loading ? undefined : "Nobody is signed in"}
      />
    );
  }
  if (error) {
    return (
      <StatCard
        label="Waiting on your decision"
        value={EMPTY_VALUE}
        sub="Could not be read"
        subTone="danger"
      />
    );
  }
  if (!waiting) {
    return <StatCard label="Waiting on your decision" value={EMPTY_VALUE} loading />;
  }
  return (
    <StatCard
      label="Waiting on your decision"
      value={`${waiting.count.toLocaleString()}${waiting.floor ? "+" : ""}`}
      trend={review}
      sub={
        // NO SECOND "NOTHING": the figure says 0, and the greeting says it in
        // words. The line is for what the figure cannot carry.
        waiting.count > 0 && waiting.oldestAt
          ? `Oldest waiting ${waitedFor(waiting.oldestAt, now)}`
          : undefined
      }
      subTone={waiting.count > 0 ? "warning" : undefined}
    />
  );
}

/**
 * Whether Worklet's tiles are closed to this reader: once the viewer has
 * said they hold no Worklet, never on a guess while it has not.
 */
function workletLocked(viewer: ViewerState): boolean {
  return !viewer.loading && !viewer.worklet;
}

/** Open work somebody has started, its change over a week, and what is stuck. */
function InProgressTile({ flow, locked }: { flow: QueryResult<WorkFlowAnswer>; locked: boolean }) {
  const data = flow.data;
  if (locked) return <LockedTile label="Tasks in progress" />;
  if (flow.error) return <RefusedTile label="Tasks in progress" />;
  if (!data?.now) return <StatCard label="Tasks in progress" value={EMPTY_VALUE} loading />;
  const points = data.points ?? [];
  const current = data.now.active;
  const earlier = points.length > WEEK ? points[points.length - 1 - WEEK]!.active : null;
  const delta = deltaWords(current, earlier, false);
  const trouble = troubleParts(data.now);
  return (
    <StatCard
      label="Tasks in progress"
      value={current.toLocaleString()}
      trend={<Sparkline values={points.slice(-14).map((p) => p.active)} current />}
      delta={delta ? { value: delta, polarity: "neutral" } : undefined}
      sub={<Parts parts={inProgressParts(delta !== null, trouble)} />}
    />
  );
}

/** Work delivered in the window, against the window before it. */
function CompletedTile({
  flow,
  range,
  locked,
}: {
  flow: QueryResult<WorkFlowAnswer>;
  range: HomeRange;
  locked: boolean;
}) {
  const label = `Completed · ${range.words}`;
  const data = flow.data;
  if (locked) return <LockedTile label={label} />;
  if (flow.error) return <RefusedTile label={label} />;
  if (!data?.points) return <StatCard label={label} value={EMPTY_VALUE} loading />;
  const { current, previous } = completedIn(data.points, range.days);
  const delta = deltaWords(current, previous, true);
  const shown = Math.max(range.days, 14);
  return (
    <StatCard
      label={label}
      value={current.toLocaleString()}
      trend={<Sparkline values={data.points.slice(-shown).map((p) => p.completed)} current />}
      delta={
        delta
          ? {
              value: delta,
              polarity: delta.startsWith("+") ? "good" : delta.startsWith("−") ? "bad" : "neutral",
            }
          : undefined
      }
      sub={delta ? `vs ${range.previous}` : undefined}
    />
  );
}

/**
 * Tokens over the window, and the week's budget — labelled as the week's,
 * apart from the rolling window.
 *
 * NO DELTA, as the approved tile draws it: its second line is the budget's,
 * and a bare "−88%" in front of "No weekly budget" read as a claim about the
 * budget. The window-on-window change is Spend's, where it has its own axis.
 *
 * THE WAY TO SET A BUDGET IS OFFERED TO WHOEVER CAN SET ONE: a reader who may
 * change the company's configuration (`mayChangeConfig` — an admin, and on a
 * managed document one of its writers) gets "No
 * weekly budget" as the link into Company › Budgets; anybody else gets the fact,
 * since the screen behind the link would only refuse them. The weekly budget
 * is the COMPANY's ceiling, so no lead path reaches it.
 *
 * AND ONLY ONCE IT IS A FACT. Before the engine's first `budget` report the
 * slice is `null` — nobody has read the counter — and the line says that
 * rather than "No weekly budget", which a capped company would otherwise be
 * told for the first seconds after every engine start.
 */
function TokensTile({
  spend,
  range,
  mayConfigure,
}: {
  spend: QueryResult<TokenSeries>;
  range: HomeRange;
  /**
   * Whether the reader may set a ceiling: `mayChangeConfig` — they hold
   * Admin and, on a managed document, are one of its writers.
   */
  mayConfigure: boolean;
}) {
  const label = `Tokens · ${range.words}`;
  const budget = useOrgBudget();
  const org = useOrg();
  const week = weekWindow(budget?.org?.windows);
  if (spend.error) return <RefusedTile label={label} />;
  if (!spend.data?.totals) return <StatCard label={label} value={EMPTY_VALUE} loading />;
  const total = spend.data.totals.total_tokens;
  return (
    <StatCard
      label={label}
      value={fmtCount(total)}
      trend={
        week ? (
          <Meter
            hideLabel
            label="This week's token budget"
            value={week.used}
            max={week.limit ?? 0}
            state={week.state as MeterState}
            valueText={`${week.used.toLocaleString()} of ${(week.limit ?? 0).toLocaleString()} tokens this week`}
          />
        ) : undefined
      }
      sub={
        week ? (
          week.state === "refusing" && mayConfigure ? (
            // A SPENT WEEK IS RAISED WHERE ITS CEILING IS, by a reader who
            // may; the same link table every budget refusal opens through.
            <a className="t-link" href={href(companyBudget.path, { ...companyBudget.query })}>
              <Parts parts={budgetCaption(week, org?.timezone ?? budget?.timezone)} />
            </a>
          ) : (
            <Parts parts={budgetCaption(week, org?.timezone ?? budget?.timezone)} />
          )
        ) : budget === null ? (
          "Weekly budget not reported yet"
        ) : mayConfigure ? (
          <a className="t-link" href={href(companyBudget.path, { ...companyBudget.query })}>
            No weekly budget
          </a>
        ) : (
          "No weekly budget"
        )
      }
      subTone={
        week?.state === "refusing" ? "danger" : week?.state === "near" ? "warning" : undefined
      }
    />
  );
}

/**
 * A second line of whole facts: each part unbroken, the separator leading the
 * part it introduces, so a line too long for its tile breaks between facts —
 * see `joinParts` in `model.ts`.
 */
/** Where the company's ceilings are raised: the link table's own destination. */
const companyBudget = remedyDestination({ setting: "company_token_budget" });

function Parts({ parts }: { parts: readonly string[] }) {
  if (parts.length === 0) return null;
  return (
    <>
      {parts.map((part, i) => (
        // THE SPACE IS OUTSIDE THE PART, where it is a place to break.
        <Fragment key={i}>
          {i > 0 && " "}
          <span className="home-kpi-part">{i > 0 ? `· ${part}` : part}</span>
        </Fragment>
      ))}
    </>
  );
}

/** A tile whose answer the engine refused: it says so rather than a number. */
function RefusedTile({ label }: { label: string }) {
  return <StatCard label={label} value={EMPTY_VALUE} sub="Could not be read" subTone="danger" />;
}

/**
 * A tile whose figure is Worklet's, for a reader without Worklet: the
 * question is never asked, and the tile says what would open it rather than
 * waiting for an answer that will never come.
 */
function LockedTile({ label }: { label: string }) {
  return (
    <StatCard label={label} value={EMPTY_VALUE} sub={`Needs ${accessNames([WORKLET])} access`} />
  );
}
