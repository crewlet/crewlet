/**
 * Home — the landing screen: how the company is, what waits on the person
 * reading, and what the company did.
 *
 * # The first fold is the company, then the reader
 *
 * A queue-shaped home renders a healthy company as a blank page, and a reader
 * cannot tell that from a broken one. So the screen opens with five figures
 * that are true whatever the queue holds — who is working, what waits on you,
 * the work in progress, the work finished, the tokens spent — and under them
 * the decisions only this reader can make, answered IN PLACE: an option of a
 * structured ask is a button that sends the choice, a parked coding run is
 * answered by its turn. An empty decisions card then means something: nothing
 * needs you, on a company that is visibly running.
 *
 * # Four questions, and the rest is pushes
 *
 * `work_flow` (the tracker's history replayed backward from today's census),
 * `token_series` (the spend over the window, by team), `company_feed` (one merged feed with one cursor) and `decisions` (what
 * waits on this person). Who is working, the budget meter and the fleet are
 * the pushes every screen already holds, so nothing here polls for them.
 *
 * # Every sentence is the engine's, and none prints a zero
 *
 * The status line is the health push and the engine's own seat vocabulary;
 * the day is written on the COMPANY's clock, because the company's day is the
 * one every due date and budget window is cut on; the greeting is the
 * reader's own, because it is their morning. See `model.ts` for how each
 * clause reads for none, one and many.
 *
 * # Whose decisions, and the clock
 *
 * THE READER'S OWN, asked by their credential and enabled for anybody with a
 * record — a seat when the identity directory binds them to one, their login
 * when it does not (`owner`): an unbound reader is asked things and asks
 * things too. Nobody types whose decisions these are.
 *
 * NO PART OF THIS SCREEN TAKES THE SECOND: the date and the greeting read the
 * clock where they are written, and so does every row's age below. Held here,
 * the second drew all five tiles, both cards and the feed once a second.
 */

import { useMemo } from "react";
import { SegmentedControl } from "@crewlethq/ui";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useWorkingNow } from "~/app/Shell.tsx";
import { NewTaskButton } from "~/components/NewTaskButton.tsx";
import { href, useParam } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { dateFormatter, toWall } from "~/lib/format.ts";
import { ClockText } from "~/app/frame/cells.tsx";
import { useAttention } from "~/lib/useAttention.ts";
import { useAgents, useConnection, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { Kpis } from "./Kpis.tsx";
import { Decisions } from "./Decisions.tsx";
import { seatConditionsOf, seatDecisionsFor } from "~/components/DecisionRow.tsx";
import { conditionKey, conditionsToDecide } from "~/lib/attention.ts";
import { LiveNow } from "./LiveNow.tsx";
import { CompletedChart } from "./CompletedChart.tsx";
import { TokensByTeam } from "./TokensByTeam.tsx";
import { Projects } from "./Projects.tsx";
import { Feed } from "./Feed.tsx";
import {
  DEFAULT_RANGE,
  HOME_RANGES,
  crewOf,
  flowPoints,
  rangeOf,
  statusSentence,
  waitingOf,
} from "./model.ts";

/** How often the decisions are asked again: a person answers one and another
 *  lands, and the push carries neither. */
const DECISIONS_POLL_MS = 30_000;

/** The series and the spend move on the scale of minutes. */
const SLOW_POLL_MS = 60_000;

export function Home() {
  // WHO IS WORKING, at the head of this page and no other (`useWorkingNow`).
  useWorkingNow();
  const org = useOrg();
  const viewer = useViewer();
  const agents = useAgents();
  const engine = useEngineHealth();
  const { connected, authRejected } = useConnection();
  const attention = useAttention();
  const [rangeValue, setRange] = useParam("range", DEFAULT_RANGE);
  const range = rangeOf(rangeValue);

  const decisions = useQuery("decisions", undefined, {
    enabled: viewer.owner !== "",
    pollMs: DECISIONS_POLL_MS,
  });
  const flow = useQuery(
    "work_flow",
    { bucket: "day", points: flowPoints(range) },
    { pollMs: SLOW_POLL_MS },
  );
  const spend = useQuery(
    "token_series",
    { days: range.days, group: "unit", bucket: "day" },
    { pollMs: SLOW_POLL_MS },
  );

  const crew = useMemo(() => crewOf(agents), [agents]);
  // THE STOPPED SEATS, SPLIT BY WHO CAN ACT: the ones this reader can raise
  // or hand on are decisions waiting on them; the rest are conditions they
  // can see and nothing more. See `seatDecisionsFor`.
  const seats = useMemo(() => seatDecisionsFor(seatConditionsOf(agents), viewer), [agents, viewer]);
  // WHAT WAITS ON THE READER: the engine's count of their asks and parked
  // runs, and the stopped seats they can act on. Unknown — nobody signed in,
  // or not answered yet — is null, never zero.
  const waiting = viewer.owner ? waitingOf(decisions.data, seats.mine) : null;
  // THE OTHER CONDITIONS: what the engine raised that a person decides and
  // that is not one of this reader's own decisions — a seat stopped for
  // another reason, a budget near its ceiling. Exactly the Inbox's condition
  // rows (`conditionsToDecide`): the engine's own rows are the sentence's
  // precedence and the health card's, a stalled round and the parked runs are
  // Live's, and a budget-stopped seat is one of the decisions.
  const raised = conditionsToDecide(attention);
  // A stopped seat counts here exactly when it is not counted as one of the
  // reader's decisions: every one while nothing waits is known, else the ones
  // they cannot act on.
  const conditions =
    raised.length + (waiting ? seats.others.length : seats.mine.length + seats.others.length);
  // WHERE THEY ARE LISTED: the Inbox's "Needs a decision" group, opened on the
  // one condition when there is exactly one.
  const conditionsHref =
    conditions === 1 && raised.length === 1
      ? href(["inbox"], { row: conditionKey(raised[0]!.id) })
      : href(["inbox"]);

  const sentence = statusSentence({
    company: org?.name ?? "",
    connected,
    authRejected,
    configured: engine?.configured,
    posture: engine?.posture,
    draining: engine?.shutting_down ?? false,
    nodes: engine?.nodes,
    waiting,
    conditions,
    conditionsHref,
    working: crew.working,
  });

  return (
    <div className="home">
      <PageActions>
        {/* THE ONE SHEET every "New task" opens — see `app/newTask.ts`. It
            stood in as the palette's "Create task" until the sheet landed. */}
        <NewTaskButton />
      </PageActions>

      <header className="home-head">
        <div className="home-head-text">
          <span className="home-date">
            <ClockText read={(now) => companyDay(now, org?.timezone)} />
          </span>
          <h2 className="home-greeting">
            <ClockText read={(now) => greeting(now, viewer.name)} />
          </h2>
          <p className="home-status">
            {sentence.map((run, i) =>
              run.strong ? (
                <strong key={i} className="home-status-figure">
                  {run.text}
                </strong>
              ) : run.href ? (
                <a key={i} className="prose-link" href={run.href}>
                  {run.text}
                </a>
              ) : (
                <span key={i}>{run.text}</span>
              ),
            )}
          </p>
        </div>
        <SegmentedControl
          label="Time range"
          semantics="radio"
          options={HOME_RANGES.map((r) => ({ value: r.value, label: r.label }))}
          value={range.value}
          onValueChange={(v) => setRange(v)}
        />
      </header>

      <Kpis
        range={range}
        crew={crew}
        viewer={viewer}
        waiting={waiting}
        waitingError={decisions.error}
        flow={flow}
        spend={spend}
      />

      <div className="home-row home-row-main">
        <Decisions
          viewer={viewer}
          answer={decisions.data}
          error={decisions.error}
          refusal={decisions.refusal}
          loading={decisions.loading}
          seatConditions={seats.mine}
        />
        <LiveNow agents={agents} />
      </div>

      <div className="home-row home-row-three">
        <CompletedChart flow={flow} />
        <TokensByTeam spend={spend} range={range} />
        <Projects />
      </div>

      <Feed />
    </div>
  );
}

/**
 * "Good morning, Jane" — in the READER's clock, which is whose morning it is,
 * and with the first word of their name. A reader nobody named is greeted
 * without one rather than as "there" or as a token id.
 *
 * THE READER'S CLOCK IS THE ZONE THEY CHOSE (`toWall`), the same one every
 * timestamp on the page is drawn in — not the browser's, which a reader who
 * set a zone has said is not theirs.
 */
export function greeting(now: number, name: string): string {
  const hour = Number(toWall(now).slice(11, 13));
  const part = hour < 5 ? "evening" : hour < 12 ? "morning" : hour < 18 ? "afternoon" : "evening";
  const first = name.trim().split(/\s+/)[0] ?? "";
  return first ? `Good ${part}, ${first}` : `Good ${part}`;
}

/**
 * The date on the company's clock: "Tuesday, September 22". The zone is the
 * one the engine resolved (`org.timezone`); before it arrives the browser's
 * own is used, which is the same day for almost every reader and never a
 * blank line.
 */
export function companyDay(now: number, zone: string | undefined): string {
  // THROUGH THE KEPT FORMATTERS (`lib/format.ts`), one per zone: built here,
  // the line built a formatter every time the clock asked it.
  const options: Intl.DateTimeFormatOptions = { weekday: "long", month: "long", day: "numeric" };
  try {
    return dateFormatter(undefined, { ...options, timeZone: zone || undefined }).format(now);
  } catch {
    // A ZONE THIS RUNTIME CANNOT FORMAT IN is not a reason to lose the line.
    return dateFormatter(undefined, options).format(now);
  }
}
