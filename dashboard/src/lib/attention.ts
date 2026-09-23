/**
 * What needs a person.
 *
 * Every one of these conditions was already known to the dashboard and each
 * lived in a different screen: a sandbox run parked on a question was a badge
 * on the board (and a run whose box had been reclaimed appeared NOWHERE at
 * all), a seat the engine stopped was a card among the healthy ones, a budget
 * refusing charges was a bar on one seat's page, and an engine with no active
 * configuration — refusing every inbound webhook — was a line in a popover.
 *
 * They are one list because they are one question, and it is the question an
 * operator opens this page with. Ordered by what it costs to ignore, and every
 * item carries where to go.
 *
 * WHAT IT WATCHES IS A DECLARATION, NOT PROSE ON A SCREEN. The inbox's quiet
 * band has to say what "nothing needs a decision" was measured over — an empty
 * band with no scope reads exactly like a band nobody wired up. It said "No
 * seat is stopped, no run is parked on a question, and no budget is refusing",
 * which is three of the twelve conditions below written as a closed sentence,
 * so an engine with no active company configuration, a node shedding its
 * seats, a draining node, a refused token and a round stalled for eleven
 * minutes were all inside a silence that claimed to have measured them.
 */

import type {
  AgentRow,
  EngineHealth,
  OrgBudget,
  SandboxEntry,
  SandboxRun,
} from "~/protocol/index.ts";
import type { MarkName } from "~/ui/glyph.tsx";
import { roundLabel, runState, staleness } from "./seats.ts";

export type Severity = "critical" | "caution" | "info";

/**
 * WHAT A CONDITION IS ABOUT — the vocabulary the quiet band draws.
 *
 * A SUBJECT rather than one entry per condition, for two reasons. Twelve
 * clauses is not a sentence: the band's caption is read at a glance or not at
 * all. And a subject is what survives — a thirteenth budget condition is
 * already covered by "the token budgets", so the sentence only changes when the
 * KIND of thing being watched changes, which is the only change a reader needs
 * told.
 *
 * It is two-sided by construction. `subject` is required on [Attention], so a
 * push site that names none is a type error; [SUBJECTS] is total over the
 * union, so a new subject is a type error until it has the phrase a reader
 * sees. Neither half is a convention anybody has to remember.
 */
export type Subject = "engine" | "budget" | "run" | "seat";

/** Each subject as the reader's own words, in the order the clause lists them. */
export const SUBJECTS: Record<Subject, string> = {
  engine: "the engine and this node's link to it",
  budget: "the company's and every seat's token budget",
  run: "every coding run waiting on an answer",
  seat: "every seat's own state",
};

function joinClauses(parts: readonly string[]): string {
  if (parts.length < 3) return parts.join(" and ");
  // THE OXFORD COMMA IS LOAD-BEARING HERE: the clauses carry their own "and"
  // ("the company's and every seat's token budget"), so without it the last two
  // run together and read as one item.
  return `${parts.slice(0, -1).join(", ")}, and ${parts.at(-1) ?? ""}`;
}

/**
 * The subjects as one English clause — the sentence the inbox drops in.
 *
 * JOINED HERE, beside the words it joins. `Intl.ListFormat` is the obvious
 * alternative and is wrong for this string: it joins in the BROWSER's locale,
 * so a reader on a Japanese one would get "、" between clauses that are
 * themselves hardcoded English, and the assertion that the band names every
 * subject would pass or fail on the machine's locale.
 */
export const WATCHED: string = joinClauses(Object.values(SUBJECTS));

export interface Attention {
  id: string;
  severity: Severity;
  /** What this is about. [SUBJECTS] is what the quiet band draws from it. */
  subject: Subject;
  icon: MarkName;
  /** What happened, in the fewest words that are still true. */
  title: string;
  /** What it costs to leave it, or what to do about it. */
  detail: string;
  /** Where the answer is. */
  path?: string[];
  query?: Record<string, string>;
  /** The instant this became true, for ordering within a severity. */
  at?: string;
  who?: string;
}

export interface AttentionInput {
  agents: AgentRow[];
  /**
   * The live projection, which is what a seat is doing NOW.
   *
   * It is the right source for that and the wrong one for anything that
   * waits: the projection sweeps a sandbox entry at `sandboxEntryMaxAge` (12
   * hours), so a run parked on a question leaves this list while still
   * waiting — see `runs`.
   */
  sandboxes: SandboxEntry[];
  /**
   * The DURABLE coding-run rows, which is what is still waiting.
   *
   * THE SWEEP IS THE WHOLE REASON THIS IS A SECOND INPUT. A run parked on a
   * question is the longest-lived thing in this list by construction — it is
   * waiting for a person, who is by definition not there — and it was read
   * from the projection, which drops it after twelve hours. So the queue lost
   * the item exactly when it had been ignored long enough to matter, and the
   * dashboard reported a quiet company.
   */
  runs: SandboxRun[];
  budget: OrgBudget;
  engine: EngineHealth | null;
  connected: boolean;
  authRejected: boolean;
  now: number;
}

const ORDER: Record<Severity, number> = { critical: 0, caution: 1, info: 2 };

export function attentionQueue(input: AttentionInput): Attention[] {
  const out: Attention[] = [];
  const { agents, sandboxes, runs, budget, engine, connected, authRejected, now } = input;

  // --- the engine itself ---------------------------------------------------
  if (authRejected) {
    out.push({
      id: "auth",
      severity: "critical",
      subject: "engine",
      icon: "key",
      title: "The engine refused this browser's token",
      detail:
        "Reads and writes are both blocked. Set a token matching one of the api.auth.tokens entries.",
    });
  } else if (!connected) {
    out.push({
      id: "offline",
      severity: "critical",
      subject: "engine",
      icon: "power_settings_new",
      title: "No connection to the engine",
      detail:
        "The page is showing the last state it received and polling a REST snapshot until the socket returns.",
    });
  }

  // An engine with no active company revision looks exactly like a healthy
  // idle one, and refuses every inbound webhook. This is the whole reason the
  // engine carries a `configured` flag.
  if (engine && engine.configured === false) {
    out.push({
      id: "unconfigured",
      severity: "critical",
      subject: "engine",
      icon: "tune",
      title: "No company configuration is active",
      detail:
        "The engine is running with nothing to run: no seats are spawned, and every inbound webhook is refused with a 503 its sender will retry. Import a company revision.",
      path: ["admin", "config"],
    });
  }
  if (engine?.posture && ["shed", "stuck", "isolated"].includes(engine.posture)) {
    out.push({
      id: `posture-${engine.posture}`,
      severity: "critical",
      subject: "engine",
      icon: "dns",
      title: `This node's control-plane posture is "${engine.posture}"`,
      detail:
        engine.posture === "shed"
          ? "It has released its seats because it could not reach the configuration it is supposed to run."
          : "It cannot converge on the fleet's active configuration.",
      path: ["admin", "fleet"],
    });
  }
  if (engine?.shutting_down) {
    out.push({
      id: "draining",
      severity: "caution",
      subject: "engine",
      icon: "power_settings_new",
      title: "This node is draining",
      detail: `${engine.in_flight ?? 0} turn(s) still in flight. Seats are released as each finishes.`,
      path: ["admin", "fleet"],
    });
  }

  // --- budgets -------------------------------------------------------------
  //
  // THE REFUSAL FIRST, THE METER AS A BACKSTOP. `refused_at` is the gate's own
  // record of turning a charge away, kept in the shared counter beside the
  // spend, so it is fleet-wide and every node reports the same one. It is what
  // "exhausted" actually means: a refused charge increments NOTHING, so a
  // company charged in rounds sits just short of its cap for ever and
  // `used >= max` never comes true. That test is kept beside it because it is
  // sufficient where it does fire — at or past the cap no further charge can
  // be accepted — and it covers the moment before the first refusal is stamped.
  const org = budget?.org;
  if (org?.refused_at) {
    out.push({
      id: "org-budget",
      severity: "critical",
      subject: "budget",
      icon: "token",
      title: "The company token budget is refusing charges",
      detail: `Turns are being declined at the budget gate. Last refusal ${org.refused_at}. Raise token_budget or reset the counter.`,
      path: ["cost"],
      at: org.refused_at,
    });
  } else if (org && org.max > 0 && org.used >= org.max) {
    out.push({
      id: "org-budget",
      severity: "critical",
      subject: "budget",
      icon: "token",
      title: "The company token budget is spent",
      detail: `${org.used.toLocaleString()} of ${org.max.toLocaleString()} tokens. No further charge can be accepted, so turns are being declined at the gate.`,
      path: ["cost"],
    });
  } else if (org && org.max > 0 && org.used / org.max >= 0.9) {
    out.push({
      id: "org-budget-near",
      severity: "caution",
      subject: "budget",
      icon: "token",
      title: "The company token budget is nearly spent",
      detail: `${Math.round((org.used / org.max) * 100)}% of the company's token budget is spent. Raise token_budget or reset the counter.`,
      path: ["cost"],
    });
  }

  // --- coding runs ---------------------------------------------------------
  // FROM THE DURABLE ROWS, not from the projection: see `runs` above.
  for (const run of runs) {
    // THE ENGINE'S OWN WORDS. `awaiting_input` is not one of them, so this
    // condition never fired and a run parked on a question reached the queue
    // through no path at all. `reseed` is the same fact one step worse — the
    // box was reaped past its pause TTL and only the question survives.
    if (run.status !== "awaiting_clarification" && run.status !== "reseed") continue;
    out.push({
      id: `sandbox-${run.turn_id}`,
      severity: "caution",
      subject: "run",
      icon: "help",
      title: `${run.role || run.agent_handle} is waiting on an answer`,
      detail: waitingDetail(run, now),
      // THE RUN'S OWN PATH. It was `#/activity/runs?run=`, which the runs
      // screen stopped reading when a run became an object with an address.
      path: ["activity", "runs", run.turn_id],
      // WHEN IT PARKED, not when it started: what this row is about is how
      // long somebody has been waited on, and a run that worked for an hour
      // before asking has been waiting for none of it.
      at: run.paused_at || run.started_at,
      who: run.agent_handle,
    });
  }

  // --- seats ---------------------------------------------------------------
  for (const agent of agents) {
    const state = runState(agent, sandboxes);
    if (agent.last_error) {
      out.push({
        id: `error-${agent.role}`,
        severity: "critical",
        subject: "seat",
        icon: "warning",
        title: `${agent.role} stopped: ${agent.last_error.kind || "error"}`,
        detail: agent.last_error.message || "The seat stopped and has not done work since.",
        path: ["company", "people", String(agent.handle ?? agent.id)],
        at: agent.last_error.at,
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    if (state === "afk") {
      out.push({
        id: `afk-${agent.role}`,
        severity: "caution",
        subject: "seat",
        icon: "pause",
        title: `${agent.role} is AFK`,
        detail: agent.afk_reason
          ? `The engine paused it: ${agent.afk_reason}.`
          : "The engine paused this seat.",
        path: ["company", "people", String(agent.handle ?? agent.id)],
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    // A live call that has not moved is the condition a spinning row hides.
    const call = agent.live_call;
    if (call?.in_progress) {
      const how = staleness(call.updated_at, now);
      if (how) {
        out.push({
          id: `stale-${agent.role}-${call.turn_id}`,
          severity: how === "stalled" ? "critical" : "caution",
          subject: "seat",
          icon: "schedule",
          title:
            how === "stalled"
              ? `${agent.role} has been on one round for over 10 minutes`
              : `${agent.role} has been on one round for over 2 minutes`,
          // THE SAME READING AS THE CARD THIS ROW LINKS TO, from the same
          // helper. It printed the raw zero-based counter, so the queue named a
          // round one lower than the seat page it lands on, and "?" for the
          // opening frame — which is the case this row exists for: a first
          // model round that never came back.
          detail: `${call.phase} · ${roundLabel(call.round_num).text} — no update since ${call.updated_at}.`,
          path: ["company", "people", String(agent.handle ?? agent.id)],
          query: { tab: "turns" },
          at: call.updated_at,
          who: String(agent.handle ?? agent.role),
        });
      }
    }
    // THE SAME PAIR AS THE COMPANY ROW ABOVE: the gate's own refusal stamp
    // first, the meter as the backstop it is sufficient for.
    const meter = agent.budget;
    if (meter?.refused_at) {
      out.push({
        id: `seat-budget-${agent.role}`,
        severity: "caution",
        subject: "budget",
        icon: "token",
        title: `${agent.role}'s token budget is refusing charges`,
        detail: `This seat's turns are being declined at the budget gate. Last refusal ${meter.refused_at}.`,
        path: ["company", "people", String(agent.handle ?? agent.id)],
        query: { tab: "cost" },
        at: meter.refused_at,
      });
    } else if (meter && meter.max > 0 && meter.used >= meter.max) {
      out.push({
        id: `seat-budget-${agent.role}`,
        severity: "caution",
        subject: "budget",
        icon: "token",
        title: `${agent.role}'s token budget is spent`,
        detail: `${meter.used.toLocaleString()} of ${meter.max.toLocaleString()} tokens. This seat's turns are being declined at the gate.`,
        path: ["company", "people", String(agent.handle ?? agent.id)],
        query: { tab: "cost" },
      });
    }
  }

  return out.sort((a, b) => {
    const d = ORDER[a.severity] - ORDER[b.severity];
    if (d !== 0) return d;
    // Newest first inside a severity: the thing that just broke is the thing
    // being looked for.
    const at = a.at ? Date.parse(a.at) : 0;
    const bt = b.at ? Date.parse(b.at) : 0;
    if (at !== bt) return bt - at;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

/**
 * What a parked run's row says, with its deadline where there is one.
 *
 * THE DEADLINE IS THE COST OF IGNORING IT, which is what this whole list is
 * ordered by: a paused box is reclaimed at `pause_ttl_seconds` and the run
 * comes back as `reseed` — the question survives, the working tree does not.
 * A row that said only "waiting on an answer" gave a reader no reason to
 * answer today rather than tomorrow.
 *
 * A RUN WITH NO TTL IS NOT GIVEN A FALSE ONE. Zero means the box is not
 * reclaimed on a timer (see the `pause_ttl_seconds` zero-value rule), so the
 * sentence simply ends.
 */
function waitingDetail(run: SandboxRun, now: number): string {
  const asked = run.question || "A coding run paused on a clarification and cannot continue.";
  if (run.status === "reseed") {
    return `${asked} The box was already reclaimed, so answering restarts the work from the question.`;
  }
  const ttl = run.pause_ttl_seconds;
  const parked = run.paused_at ? Date.parse(run.paused_at) : NaN;
  if (!(ttl > 0) || Number.isNaN(parked)) return asked;
  const left = parked + ttl * 1_000 - now;
  if (left <= 0) return `${asked} Its box is past its pause window and may be reclaimed.`;
  // ONE ROUNDED MINUTE TOTAL, split afterwards. Flooring the hours out of
  // `left` and rounding the remainder independently rounds the same value two
  // ways: any remainder at or past 59m30s carries into a sixtieth minute the
  // hour count never sees, and a box reclaimed in 2h 59m 45s read "2h 60m" —
  // for the thirty seconds before every hour boundary of a countdown that
  // reruns every second, on the one row whose whole job is saying how long
  // somebody has to answer. `fmtDuration` avoids it the same way.
  const total = Math.round(left / 60_000);
  const hours = Math.floor(total / 60);
  const minutes = total % 60;
  const when = hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`;
  return `${asked} Its box is reclaimed in ${when}.`;
}
