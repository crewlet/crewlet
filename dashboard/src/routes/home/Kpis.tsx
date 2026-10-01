/**
 * The five figures that open the landing screen — true whatever the queue
 * holds, which is what stops a healthy company rendering as a blank page.
 *
 * EACH TILE WAITS FOR ITS OWN ANSWER and says so (the kit's loading line and
 * `aria-busy`), and a tile whose answer was refused says the refusal: a
 * number that has not arrived is a different fact from a zero, and a dash
 * that looks like a measured nothing is the one thing a tile must not draw.
 */

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

/** How many days of the series the in-progress sparkline draws, and how far
 *  back its delta compares: a week. */
const WEEK = 7;

export function Kpis({
  range,
  crew,
  viewer,
  waiting,
  waitingError,
  flow,
  spend,
  now,
}: {
  range: HomeRange;
  crew: Crew;
  viewer: ViewerState;
  waiting: Waiting | null;
  waitingError: QueryErrorCode | null;
  flow: QueryResult<WorkFlowAnswer>;
  spend: QueryResult<TokenSeries>;
  now: number;
}) {
  return (
    <section className="home-kpis" aria-label="The company at a glance">
      <AgentsTile crew={crew} />
      <WaitingTile viewer={viewer} waiting={waiting} error={waitingError} now={now} />
      <InProgressTile flow={flow} />
      <CompletedTile flow={flow} range={range} />
      <TokensTile spend={spend} range={range} operator={viewer.operator} />
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
              { id: "stopped", value: crew.stopped, tone: "danger", label: "stopped" },
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
  const review = (
    <ButtonLink
      size="small"
      variant="secondary"
      href={decisionsHref()}
      trailingIcon={<ArrowRightGlyph size="sm" />}
    >
      Review
    </ButtonLink>
  );
  // NOBODY TO WAIT ON: an anonymous reader or a token bound to no seat has no
  // decisions, and saying 0 would be a claim about a person nobody named.
  if (!viewer.handle) {
    return (
      <StatCard
        label="Waiting on your decision"
        value={EMPTY_VALUE}
        loading={viewer.loading}
        sub={viewer.loading ? undefined : "Not bound to a person"}
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
        waiting.count > 0 && waiting.oldestAt
          ? `Oldest waiting ${waitedFor(waiting.oldestAt, now)}`
          : "Nothing waiting on you"
      }
      subTone={waiting.count > 0 ? "warning" : undefined}
    />
  );
}

/** Open work somebody has started, its change over a week, and what is stuck. */
function InProgressTile({ flow }: { flow: QueryResult<WorkFlowAnswer> }) {
  const data = flow.data;
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
function CompletedTile({ flow, range }: { flow: QueryResult<WorkFlowAnswer>; range: HomeRange }) {
  const label = `Completed · ${range.words}`;
  const data = flow.data;
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
 * THE WAY TO SET A BUDGET IS OFFERED TO WHOEVER CAN SET ONE: an operator gets
 * "No weekly budget" as the link into Spend › Budgets; anybody else gets the
 * fact, since the screen behind the link would only refuse them.
 *
 * AND ONLY ONCE IT IS A FACT. Before the engine's first `budget` report the
 * slice is `null` — nobody has read the counter — and the line says that
 * rather than "No weekly budget", which a capped company would otherwise be
 * told for the first seconds after every engine start.
 */
function TokensTile({
  spend,
  range,
  operator,
}: {
  spend: QueryResult<TokenSeries>;
  range: HomeRange;
  operator: boolean;
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
          <Parts parts={budgetCaption(week, org?.timezone ?? budget?.timezone)} />
        ) : budget === null ? (
          "Weekly budget not reported yet"
        ) : operator ? (
          <a className="t-link" href={href(["spend", "budgets"])}>
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
