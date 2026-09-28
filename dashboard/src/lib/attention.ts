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

import type { AgentRow, BudgetWindow, OrgBudget, SandboxRun } from "~/protocol/index.ts";
import { PERIOD_ADJECTIVE, waitedOn } from "~/lib/budget.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { activityOf, roundLabel, staleness, stoppedLine, type NameOf } from "./seats.ts";

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
export type Subject = "engine" | "budget" | "run" | "seat" | "round";

/** Each subject as the reader's own words, in the order the clause lists them. */
export const SUBJECTS: Record<Subject, string> = {
  engine: "the engine and this node's link to it",
  budget: "the company's and every seat's token budget",
  run: "every coding run waiting on an answer",
  seat: "every seat's own state",
  round: "every round still in flight",
};

/**
 * WHERE A CONDITION IS SHOWN — one home per subject, so no condition is drawn
 * twice and none is drawn nowhere.
 *
 *  - `seat`: the Inbox's "Needs a decision" group and Home's count of
 *    conditions that need a look. A person decides these — raise a ceiling,
 *    resume a paused seat, look at one that failed.
 *  - `engine`: the sidebar's health card and Home's status sentence, which
 *    are what a reader sees on every screen; an engine condition is a fact
 *    about the product they are looking at, not an item in anybody's queue.
 *  - `live`: Live › Now running, beside the rounds and runs it is about. A
 *    round that has not moved is watched, not decided, and a coding run
 *    parked on a question already reaches the person it asks through their
 *    own decisions (`decisions`), so the company-wide list of them is live.
 *
 * TOTAL OVER [Subject], so a new subject is a type error until it has a home.
 */
export type Where = "seat" | "engine" | "live";

export const WHERE_OF: Record<Subject, Where> = {
  engine: "engine",
  budget: "seat",
  run: "live",
  seat: "seat",
  round: "live",
};

/** The subjects shown in one place, as the clause its quiet state says. */
export function watchedIn(where: Where): string {
  return joinClauses(
    (Object.keys(SUBJECTS) as Subject[])
      .filter((subject) => WHERE_OF[subject] === where)
      .map((subject) => SUBJECTS[subject]),
  );
}

/**
 * The conditions a PERSON decides — the Inbox's "Needs a decision" rows beside
 * the asks and runs, and what Home counts as needing a look.
 *
 * A SEAT REFUSING ON ITS OWN BUDGET IS LEFT OUT, because the seat the engine
 * stopped for it is already a decision row of its own
 * (`components/DecisionRow.tsx`'s `seatConditionsOf`) that names the window
 * and offers the two ways out; listing the refusal as well would be the one
 * stop twice.
 */
export function conditionsToDecide(queue: readonly Attention[]): Attention[] {
  return queue.filter((a) => WHERE_OF[a.subject] === "seat" && !a.id.startsWith("seat-budget-"));
}

/** A condition's row in the Inbox, as `row=` names it. */
export function conditionKey(id: string): string {
  return `condition:${id}`;
}

function joinClauses(parts: readonly string[]): string {
  if (parts.length < 3) return parts.join(" and ");
  // THE OXFORD COMMA IS LOAD-BEARING HERE: the clauses carry their own "and"
  // ("the company's and every seat's token budget"), so without it the last two
  // run together and read as one item.
  return `${parts.slice(0, -1).join(", ")}, and ${parts.at(-1) ?? ""}`;
}

/*
 * THE CLAUSES ARE JOINED HERE, beside the words they join. `Intl.ListFormat` is the obvious
 * alternative and is wrong for this string: it joins in the BROWSER's locale,
 * so a reader on a Japanese one would get "、" between clauses that are
 * themselves hardcoded English, and the assertion that the band names every
 * subject would pass or fail on the machine's locale.
 */

export interface Attention {
  id: string;
  severity: Severity;
  /** What this is about. [SUBJECTS] is what the quiet band draws from it,
   *  and [WHERE_OF] where the row is shown. */
  subject: Subject;
  icon: GlyphName;
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
  /**
   * Every seat's row, carrying the ENGINE's word for what it is doing
   * (`activity`). The live sandbox projection used to be an input too, read
   * for a seat's run state — a second derivation of the one word the engine
   * now serves — and nothing here reads it any more.
   */
  agents: AgentRow[];
  /**
   * The DURABLE coding-run rows, which is what is still waiting.
   *
   * THE WAITING ROW NEEDS THE RECORD, not the live entry: how long a person
   * has left to answer is the run's pause window (`pause_ttl_seconds`)
   * counted from `paused_at`, and only the durable row
   * carries the window. The live entry says a run is waiting; the record says
   * for how much longer its box is held.
   */
  runs: SandboxRun[];
  budget: OrgBudget;
  engine: EngineHealth | null;
  connected: boolean;
  authRejected: boolean;
  now: number;
  /** A person's name by their handle, off the chart: who paused a seat is said by name. */
  nameOf: NameOf;
}

const ORDER: Record<Severity, number> = { critical: 0, caution: 1, info: 2 };

export function attentionQueue(input: AttentionInput): Attention[] {
  const out: Attention[] = [];
  const { agents, runs, budget, engine, connected, authRejected, now, nameOf } = input;

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
      icon: "power",
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
      icon: "sliders-vertical",
      title: "No company configuration is active",
      detail:
        "The engine is running with nothing to run: no seats are spawned, and every inbound webhook is refused with a 503 its sender will retry. Import a company revision.",
      path: ["settings", "config"],
    });
  }
  if (engine?.posture && ["shed", "stuck", "isolated"].includes(engine.posture)) {
    out.push({
      id: `posture-${engine.posture}`,
      severity: "critical",
      subject: "engine",
      icon: "server",
      title: `This node's control-plane posture is "${engine.posture}"`,
      detail:
        engine.posture === "shed"
          ? "It has released its seats because it could not reach the configuration it is supposed to run."
          : "It cannot converge on the fleet's active configuration.",
      path: ["settings", "nodes"],
    });
  }
  if (engine?.shutting_down) {
    out.push({
      id: "draining",
      severity: "caution",
      subject: "engine",
      icon: "power",
      title: "This node is draining",
      detail: `${engine.in_flight ?? 0} turn(s) still in flight. Seats are released as each finishes.`,
      path: ["settings", "nodes"],
    });
  }

  // --- budgets -------------------------------------------------------------
  //
  // THE ENGINE'S OWN STATE, and no threshold of ours. Every window the live
  // meter carries is judged where the counter is — `refusing` when the gate
  // has turned a charge away in it or it has no room for a single token,
  // `near` at the engine's near fraction — so this list, the Budgets screen
  // and the meters all say the same thing about one window. A refusal outranks
  // a near window; within each, the window NAMED is the one that turns over
  // last, which is when the company has room again without a ceiling raised.
  const org = budget?.org?.windows;
  const orgRefusing = waitedOn(org, "refusing");
  const orgNear = orgRefusing ? undefined : waitedOn(org, "near");
  if (orgRefusing) {
    out.push({
      id: "org-budget",
      severity: "critical",
      subject: "budget",
      icon: "coins",
      title: `The company's ${PERIOD_ADJECTIVE[orgRefusing.period]} token budget is refusing charges`,
      detail: `${refusalWords(orgRefusing)} Raise token_budget.${orgRefusing.period}, or wait for ${orgRefusing.window} to turn over at ${orgRefusing.resets_at}.`,
      path: ["spend"],
      at: orgRefusing.refused_at,
    });
  } else if (orgNear) {
    out.push({
      id: "org-budget-near",
      severity: "caution",
      subject: "budget",
      icon: "coins",
      title: `The company's ${PERIOD_ADJECTIVE[orgNear.period]} token budget is nearly spent`,
      detail: `${spentWords(orgNear)} Raise token_budget.${orgNear.period}, or wait for ${orgNear.window} to turn over at ${orgNear.resets_at}.`,
      path: ["spend"],
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
      icon: "circle-question-mark",
      title: `${run.role || run.agent_handle} is waiting on an answer`,
      detail: waitingDetail(run, now),
      // THE RUN'S OWN PATH. It was `#/live/runs?run=`, which the runs
      // screen stopped reading when a run became an object with an address.
      path: ["live", "runs", run.turn_id],
      // WHEN IT PARKED, not when it started: what this row is about is how
      // long somebody has been waited on, and a run that worked for an hour
      // before asking has been waiting for none of it.
      at: run.paused_at || run.started_at,
      who: run.agent_handle,
    });
  }

  // --- seats ---------------------------------------------------------------
  for (const agent of agents) {
    const state = activityOf(agent);
    if (agent.last_error) {
      out.push({
        id: `error-${agent.role}`,
        severity: "critical",
        subject: "seat",
        icon: "triangle-alert",
        title: `${agent.role} stopped: ${agent.last_error.kind || "error"}`,
        detail: agent.last_error.message || "The seat stopped and has not done work since.",
        path: ["agents", "seats", String(agent.handle ?? agent.id)],
        at: agent.last_error.at,
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    // A STOPPED SEAT, in the engine's word and for the engine's reason. The
    // budget's stop has its own row above, which names the window and when
    // it resets, so it is not said twice.
    if (state === "stopped" && agent.stopped_reason !== "budget") {
      out.push({
        id: `stopped-${agent.role}`,
        severity: "caution",
        subject: "seat",
        icon: "pause",
        title: `${agent.role} is stopped`,
        detail: `The seat cannot take work: ${stoppedLine(agent, nameOf)}.`,
        path: ["agents", "seats", String(agent.handle ?? agent.id)],
        // WHEN A PERSON PAUSED IT, where one did: the instant the condition
        // began, which is what orders it among the rest and what its row's
        // age says.
        ...(agent.paused?.at ? { at: agent.paused.at } : {}),
        who: String(agent.handle ?? agent.role),
      });
      continue;
    }
    // A live call that has not moved is the condition a spinning row hides.
    const call = agent.live_call;
    if (call?.in_progress) {
      const how = staleness(call.updated_at, now, agent.turn?.stage);
      if (how) {
        out.push({
          id: `stale-${agent.role}-${call.turn_id}`,
          severity: how === "stalled" ? "critical" : "caution",
          subject: "round",
          icon: "clock",
          title:
            how === "stalled"
              ? `${agent.role} has been on one round for over 10 minutes`
              : `${agent.role} has been on one round for over 2 minutes`,
          // THE SAME READING AS THE CARD THIS ROW LINKS TO, from the same
          // helper. It printed the raw zero-based counter, so the queue named a
          // round one lower than the seat page it lands on, and "?" for the
          // opening frame — which is the case this row exists for: a first
          // model round that never came back.
          detail: `${call.phase} · ${roundLabel(call).text} — no update since ${call.updated_at}.`,
          // THE OVERVIEW, where the seat's current turn is drawn round by
          // round — the profile's default tab, so the path alone opens it.
          path: ["agents", "seats", String(agent.handle ?? agent.id)],
          at: call.updated_at,
          who: String(agent.handle ?? agent.role),
        });
      }
    }
    // A SEAT THAT IS REFUSING, by the engine's state as above. A seat merely
    // near its own ceiling raises nothing: the company's row covers the one
    // an operator acts on before it binds, and a caution per seat would bury
    // it in a company of any size.
    const refusing = waitedOn(agent.budget?.windows, "refusing");
    if (refusing) {
      out.push({
        id: `seat-budget-${agent.role}`,
        severity: "caution",
        subject: "budget",
        icon: "coins",
        title: `${agent.role}'s ${PERIOD_ADJECTIVE[refusing.period]} token budget is refusing charges`,
        detail: `${refusalWords(refusing)} Raise the seat's token_budget.${refusing.period}, or wait for ${refusing.window} to turn over at ${refusing.resets_at}.`,
        // THE SETTINGS TAB, where the ceiling that refused is written beside
        // each window's live meter.
        path: ["agents", "seats", String(agent.handle ?? agent.id)],
        query: { tab: "settings" },
        at: refusing.refused_at,
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

/** A refusing window in words: the gate's own stamp where it has one, and the
 *  arithmetic where the window is full but no charge has been turned away yet. */
function refusalWords(w: BudgetWindow): string {
  const spent = `${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens in ${w.window}.`;
  return w.refused_at
    ? `Turns are being declined at the budget gate; last refusal ${w.refused_at}. ${spent}`
    : `No further charge fits, so turns are being declined at the gate. ${spent}`;
}

/** A near window's spend in words. */
function spentWords(w: BudgetWindow): string {
  return `${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens in ${w.window} are spent.`;
}
