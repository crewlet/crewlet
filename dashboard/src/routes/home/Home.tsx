/**
 * Home — the landing screen: who is looking, how the company is, and what
 * needs a decision.
 *
 * # Why the landing screen is not the Inbox any more
 *
 * The Inbox was the landing screen, opened by a strip of company figures so
 * that a quiet queue would not read as a broken dashboard. Those are two
 * different questions — "how is the company" and "what reached me" — and
 * answering both on one screen made the second one's list start below the
 * fold. Home answers the first; the Inbox is the place a person acts.
 *
 * # The first fold is the company, not the absence of problems
 *
 * A queue-shaped home renders a healthy company as a blank page, and a reader
 * cannot tell that from a broken one. So the screen opens with the PULSE
 * STRIP — figures that are true whatever the queue holds — and the conditions
 * the engine raised sit under it. An empty band then means something: nothing
 * needs a decision, on a company that is visibly running. See `Pulse.tsx` for
 * the argument in full.
 *
 * # Every sentence is the engine's
 *
 * The status line is the health push — the fleet's size, whether a company is
 * configured, whether this browser is connected — and an engine condition
 * takes it over rather than sitting beside a sentence that says all is well.
 * The day is written on the COMPANY's clock (`org.timezone`), because the
 * company's day is the one every due date and budget window is cut on; the
 * greeting is the reader's own, because it is their morning.
 */

import { useMemo } from "react";
import { Card, Tag } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, nodeCountLabel, plural, relTime, toWall } from "~/lib/format.ts";
import { unfinished } from "~/lib/work.ts";
import { WATCHED, type Attention } from "~/lib/attention.ts";
import { useAttention } from "~/lib/useAttention.ts";
import { inboxFigure, useInboxCounts, type InboxCounts } from "~/lib/useInboxCounts.ts";
import { useAgents, useConnection, useEngineHealth, useOrg, useTokens } from "~/lib/store-hooks.ts";
import type { AgentRow, Rollup, WorkProjectRow, WorkloadRow } from "~/protocol/index.ts";
import type { EngineHealth } from "~/contract/health.ts";
import { Pulse, PULSE_GLYPHS, type PulseFact } from "./Pulse.tsx";

/** How many conditions the landing screen shows before "Open inbox". */
export const HOME_DECISIONS = 3;

export function Home() {
  const now = useNow();
  const org = useOrg();
  const viewer = useViewer();
  const agents = useAgents();
  const tokens = useTokens();
  const engine = useEngineHealth();
  const { connected, authRejected } = useConnection();
  const attention = useAttention();
  // THE ONE INBOX COUNT, the same reading the sidebar's badge and the Inbox's
  // own band draw, so the three can never disagree about what is waiting.
  const waiting = useInboxCounts();
  // THE TWO THE PULSE STRIP NEEDS AND NOTHING ELSE ON THIS SCREEN DOES. Both
  // are slow polls: an open count and a workload are facts about a fortnight,
  // and asking them at the socket's own cadence would be eight reads a minute
  // for a strip nobody is watching change.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });

  const facts = usePulse({
    agents,
    attention,
    engine,
    projects: projects.data?.projects,
    workload: workload.data?.rows,
    tokens,
  });

  const working = agents.filter((a) => a.activity === "working").length;
  const shown = attention.slice(0, HOME_DECISIONS);

  return (
    <>
      <header className="home-head">
        <span className="t-caption">{companyDay(now, org?.timezone)}</span>
        <h2 className="home-greeting">{greeting(now, viewer.name)}</h2>
        <p className="home-status">
          {statusLine({
            company: org?.name ?? "",
            connected,
            authRejected,
            configured: engine?.configured,
            nodes: engine?.nodes,
            decisions: attention.length,
            waiting,
            working,
          })}
        </p>
      </header>

      <Pulse facts={facts} />

      <Card padding="none">
        <Card.Header
          icon={<Mark name="triangle-alert" size="sm" />}
          count={attention.length}
          actions={
            <a className="t-link" href={href(["inbox"])}>
              Open inbox
            </a>
          }
        >
          <Card.Title>Needs a decision</Card.Title>
        </Card.Header>
        {shown.length === 0 ? (
          <div className="inbox-quiet home-quiet">
            <strong className="t-cell">Nothing needs a decision</strong>
            <span className="t-caption">Checked and clear: {WATCHED}.</span>
          </div>
        ) : (
          <div className="list">
            {shown.map((item) => (
              <DecisionLink key={item.id} item={item} now={now} />
            ))}
          </div>
        )}
      </Card>

      <PageNote>
        Agents reach you only for what their own authority cannot decide. Everything else is under
        Live.
      </PageNote>
    </>
  );
}

/** One engine condition, as a way into the Inbox row that holds it. */
function DecisionLink({ item, now }: { item: Attention; now: number }) {
  return (
    <a className="inbox-row" href={href(["inbox"], { row: item.id })}>
      <span className="attention-icon" data-severity={item.severity}>
        <Mark name={item.icon} size="sm" />
      </span>
      <span className="col inbox-row-body">
        <strong className="t-cell truncate">{item.title}</strong>
        <span className="t-caption truncate">{item.detail}</span>
      </span>
      {item.who && <Tag appearance="outline">{item.who}</Tag>}
      {item.at && <span className="t-caption">{relTime(item.at, now)}</span>}
    </a>
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
  const options: Intl.DateTimeFormatOptions = { weekday: "long", month: "long", day: "numeric" };
  try {
    return new Intl.DateTimeFormat(undefined, { ...options, timeZone: zone || undefined }).format(
      now,
    );
  } catch {
    // A ZONE THIS RUNTIME CANNOT FORMAT IN is not a reason to lose the line.
    return new Intl.DateTimeFormat(undefined, options).format(now);
  }
}

/**
 * The one sentence under the greeting, and an engine condition takes it over.
 *
 * THE ORDER IS PRECEDENCE: a refused token and a lost connection are said
 * before anything the last push claimed, because what was pushed may be stale;
 * an engine running no company is said before a fleet size, because a fleet
 * running nothing is not "running".
 */
export function statusLine(input: {
  company: string;
  connected: boolean;
  authRejected: boolean;
  configured: boolean | undefined;
  nodes: number | undefined;
  decisions: number;
  /** The unread primary notices waiting on the viewer — `useInboxCounts`. */
  waiting: InboxCounts;
  working: number;
}): string {
  const { company, connected, authRejected, configured, nodes, decisions, waiting, working } =
    input;
  if (authRejected) return "The engine refused this browser's token — set one to read the company.";
  if (!connected) return "Not connected to the engine. What is shown is the last state it sent.";
  if (configured === false) return "No configuration is active, so no seat is running.";
  const who = company || "The company";
  const fleet =
    nodes === undefined
      ? `${who} is running (${nodeCountLabel(nodes)})`
      : `${who} is running on ${plural(nodes, "node")}`;
  const asks =
    decisions === 0
      ? "nothing needs a decision"
      : `${plural(decisions, "condition")} ${decisions === 1 ? "needs" : "need"} a decision`;
  const busy = `${plural(working, "agent")} ${working === 1 ? "is" : "are"} working right now`;
  // WHAT REACHED THE READER, said only when something did and only when the
  // engine answered: a count nobody asked for (no bound seat) or that has not
  // arrived is left out rather than written as zero, and a page the engine
  // had more than is the floor the badge also draws.
  const notices =
    waiting.waiting !== null && waiting.waiting > 0
      ? `${inboxFigure(waiting)} ${waiting.waiting === 1 && !waiting.capped ? "notice is" : "notices are"} waiting on you`
      : "";
  return notices
    ? `${fleet}. ${capitalise(asks)}, ${notices}, and ${busy}.`
    : `${fleet}. ${capitalise(asks)}, and ${busy}.`;
}

function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/**
 * The seven facts, assembled.
 *
 * SEPARATED FROM THE SCREEN because every one of them is a small honest
 * decision about what a number covers, and those are worth reading in one
 * place: which seat states count as working, that `open` is over the projects
 * this answer returned rather than over the company, and that a window the
 * engine chose is never labelled "today".
 */
function usePulse(input: {
  agents: AgentRow[];
  attention: Attention[];
  engine: EngineHealth | null;
  projects?: WorkProjectRow[];
  workload?: WorkloadRow[];
  tokens: Rollup | null;
}): PulseFact[] {
  const { agents, attention, engine, projects, workload, tokens } = input;
  return useMemo(() => {
    // THE ENGINE'S WORD: a seat whose coding run is running is already
    // `working` there, so nothing is folded in here.
    const working = agents.filter((a) => a.activity === "working").length;
    // A RUN WAITING FOR A PERSON, which the attention queue already derived
    // from the DURABLE rows: counting it a second way here would let the
    // headline and the queue beneath it disagree about who is waiting.
    const parked = attention.filter((a) => a.subject === "run").length;
    const open = projects ? projects.reduce((n, p) => n + unfinished(p.task_counts), 0) : null;
    const overdue = workload ? workload.reduce((n, r) => n + r.overdue, 0) : null;
    const blocked = workload ? workload.reduce((n, r) => n + r.blocked, 0) : null;
    // THE ENGINE'S ALARM TABLE, the one evaluation its gauge, its log lines
    // and the sidebar's health card all read — never a count of this
    // screen's own conditions under the same word. It was the critical
    // conditions once, so the strip said "0 alarms" beside a health card
    // saying five. An absent table has not been evaluated: a dash, not zero.
    const alarms = engine?.alarms ? engine.alarms.count : null;
    const facts: PulseFact[] = [
      {
        key: "seats",
        icon: PULSE_GLYPHS.seats,
        value: working,
        label: "working",
        title: "Agent seats running a turn or a coding run, as the engine reports them.",
        path: ["agents", "roster"],
      },
      {
        key: "parked",
        icon: PULSE_GLYPHS.parked,
        value: parked,
        label: "parked",
        title:
          "Coding runs stopped on a question, from the durable rows rather than the live push.",
        path: ["live", "runs"],
        tone: parked > 0 ? "caution" : undefined,
      },
      {
        key: "open",
        icon: PULSE_GLYPHS.open,
        value: open,
        label: "open",
        title: "Open work items, summed over the projects this answer returned.",
        path: ["work"],
      },
      {
        key: "overdue",
        icon: PULSE_GLYPHS.overdue,
        value: overdue,
        label: "overdue",
        title: "Open items past their due date, summed over every person with a workload.",
        path: ["work"],
        query: { due: "overdue" },
        tone: overdue ? "caution" : undefined,
      },
      {
        key: "blocked",
        icon: PULSE_GLYPHS.blocked,
        value: blocked,
        label: "blocked",
        title: "Open items waiting on another item, summed over every person with a workload.",
        path: ["work"],
        query: { blocked: "1" },
        tone: blocked ? "caution" : undefined,
      },
      {
        key: "tokens",
        icon: PULSE_GLYPHS.tokens,
        value: tokens ? tokens.totals.total_tokens : null,
        label: tokens?.totals.total_tokens === 1 ? "token" : "tokens",
        // NOT "today". The window is the engine's and this screen was not
        // given one, so the strip names the figure and puts the window it
        // actually covers where a reader can read it.
        title: tokens
          ? `Tokens over the pushed window, ${fmtDateTime(tokens.since)} to ${fmtDateTime(tokens.until)}.`
          : "Spend has not been pushed yet.",
        path: ["spend"],
      },
      {
        key: "alarms",
        icon: PULSE_GLYPHS.alarms,
        value: alarms,
        // COUNTED, so it agrees with its figure: "1 alarms" is a strip that
        // was written for one number and shown another.
        label: alarms === 1 ? "alarm" : "alarms",
        title: engine?.alarms
          ? "The engine's standing alarms on this node — the same evaluation as its gauge and its log lines."
          : "This node has not evaluated its alarms yet.",
        path: ["settings", "nodes"],
        tone: alarms ? "caution" : undefined,
      },
    ];
    return facts;
  }, [agents, attention, engine, projects, workload, tokens]);
}
