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
 * seats, a draining node, nobody signed in and a round stalled for eleven
 * minutes were all inside a silence that claimed to have measured them.
 */

import type { AgentRow, BudgetWindow, OrgBudget, SandboxRun } from "~/protocol/index.ts";
import { entryLimits, limitDetail, limitTitle, type EntryLimit } from "./providerLimit.ts";
import { PERIOD_ADJECTIVE, waitedOn } from "~/lib/budget.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import {
  failureWords,
  roundLabel,
  shownStateOf,
  staleness,
  stateWord,
  stoppedLine,
  type NameOf,
} from "./seats.ts";
import { QUESTION_WITHHELD, questionWithheld } from "./access.ts";
import { fmtDateTime } from "./format.ts";
import type { Remedy } from "~/protocol/remedy.ts";

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
 *  - `seat`: the Inbox's "Needs you" group and Home's count of
 *    conditions that need a look. A person decides these — raise a ceiling,
 *    resume a paused seat, look at one that failed.
 *  - `engine`: the sidebar's engine row and Home's status sentence, which
 *    are what a reader sees on every screen; an engine condition is a fact
 *    about the product they are looking at, not an item in anybody's queue.
 *  - `live`: Live › Now running, BESIDE the rounds and runs it is about — a
 *    quiet round is marked on its own running-turn row (`LiveTurnRow`), and a
 *    parked run is a row of Waiting on a person with its Answer. A round that
 *    has not moved is watched, not decided, and a coding run parked on a
 *    question already reaches the person it asks through their own decisions
 *    (`decisions`), so the company-wide list of them is live.
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

/** The one seat a limit holds, where it holds one, as a row's `who`. */
function whoOfLimit(limit: EntryLimit): { who?: string } {
  const seats = [...limit.parked, ...limit.carried];
  const only = seats.length === 1 ? seats[0] : undefined;
  return only ? { who: String(only.handle ?? only.id) } : {};
}

/** The id of a seat's own condition (stopped, or its last turn failed): the
 *  seat's, whichever it is in, so its row keeps its place and its key. */
export function seatStateId(agentID: string): string {
  return `seat-state-${agentID}`;
}

/**
 * The conditions a PERSON decides: the Inbox's "Needs you" rows beside
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
  /**
   * The engine's own message behind the condition, where it recorded one: a
   * failed turn's raw error. NEVER drawn as the title or the detail, only
   * behind a "Technical details" disclosure, for whoever is fixing it.
   */
  technical?: string;
  /** Where the answer is. */
  path?: string[];
  query?: Record<string, string>;
  /**
   * Where the limit behind the condition is raised, for a condition a limit
   * caused (a spent budget): drawn in place of `path`, behind the gate of the
   * editor it opens (`components/LimitRemedy.tsx`).
   */
  remedy?: Remedy;
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
  /** `null` before the first report: nothing to judge, so nothing is raised. */
  budget: OrgBudget | null;
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
      // THE SESSION IS THE BROWSER'S ONE CREDENTIAL. The refusal is of the
      // cookie this browser holds, or of none, so the repair is signing in,
      // an API token included: the sign-in screen exchanges one for a session
      // rather than keeping it anywhere.
      title: "Nobody is signed in on this browser",
      detail: "Sign in to read or change anything, as a person or with an API token.",
    });
  } else if (!connected) {
    out.push({
      id: "offline",
      severity: "critical",
      subject: "engine",
      icon: "power",
      title: "No connection to the engine",
      detail: "Showing the last state received.",
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
      icon: "settings",
      title: "No company configuration is active",
      detail: "Import a company to start.",
      path: ["settings", "config"],
    });
  }
  if (engine?.posture && ["shed", "stuck", "isolated"].includes(engine.posture)) {
    // A SHED IS THE POSTURE THAT RELEASES THIS NODE'S SEATS, so its row says
    // so. `stuck` and `isolated` keep naming the posture: an isolated node is
    // still serving the configuration it has.
    const shed = engine.posture === "shed";
    out.push({
      id: `posture-${engine.posture}`,
      severity: "critical",
      subject: "engine",
      icon: "server",
      title: shed
        ? "This node released its seats"
        : `This node's control-plane posture is "${engine.posture}"`,
      detail: shed
        ? "It can’t reach its configuration."
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
      detail: `${engine.in_flight ?? 0} turns still running.`,
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
      detail: `${refusalWords(orgRefusing)} Raise the company limit, or wait until ${fmtDateTime(orgRefusing.resets_at)}.`,
      // THE CEILING ITSELF, where it is raised: Token spend says what was
      // spent, and has nothing on it that clears this.
      remedy: { setting: "company_token_budget" },
      at: orgRefusing.refused_at,
    });
  } else if (orgNear) {
    out.push({
      id: "org-budget-near",
      severity: "caution",
      subject: "budget",
      icon: "coins",
      title: `The company's ${PERIOD_ADJECTIVE[orgNear.period]} token budget is nearly spent`,
      detail: `${spentWords(orgNear)} Raise the company limit, or wait until ${fmtDateTime(orgNear.resets_at)}.`,
      remedy: { setting: "company_token_budget" },
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

  // --- models at their usage limit ------------------------------------------
  // ONE ROW PER KIND OF LIMIT, keyed by it, for as long as the limit lasts:
  // what the seats it holds up are waiting for, and until when
  // (providerLimit.ts). The seats themselves draw no row of their own for it
  // below.
  for (const limit of entryLimits(agents)) {
    out.push({
      id: `provider-limit-${limit.key}`,
      severity: limit.parked.length > 0 ? "critical" : "caution",
      subject: "seat",
      icon: "clock",
      title: limitTitle(limit),
      detail: limitDetail(limit, now),
      // THE COMPANY'S MODELS, where a fallback is given or a plan changed,
      // behind the same gate as the screen it opens (lib/remedy.ts).
      remedy: { setting: "model_providers" },
      at: limit.since,
      ...whoOfLimit(limit),
    });
  }

  // --- seats ---------------------------------------------------------------
  for (const agent of agents) {
    const state = shownStateOf(agent);
    const seatPath = ["company", "org", "seats", String(agent.handle ?? agent.id)];
    const who = String(agent.handle ?? agent.role);
    // A PAUSED OR STOPPED SEAT, in the seat vocabulary's own word (`stateWord`):
    // "is paused" for a person's decision, "is stopped" for a fault, for the
    // engine's reason. The budget's stop has its own row above, which names
    // the window and when it resets, so it is not said twice. A stop whose
    // cause also left a failed turn is ONE row: the stop is what a reader acts
    // on, and the failure is why.
    // A SEAT HELD BY ITS MODELS' LIMIT is its entry's row above, which says
    // until when: its stop, and the failed turn the limit left on it, are
    // not said twice, once per seat.
    const limited = agent.provider_limit != null;
    if (
      (state === "paused" || state === "stopped") &&
      agent.stopped_reason !== "budget" &&
      !(limited && agent.stopped_reason === "provider")
    ) {
      out.push({
        // THE SEAT'S OWN ID, whichever condition it is in: a seat moving
        // between "stopped" and "last turn failed" is one row whose words
        // change, never one row leaving and another arriving, which a list
        // keyed on the id would draw as a remount.
        id: seatStateId(agent.agent_id),
        severity: state === "stopped" && agent.last_error ? "critical" : "caution",
        subject: "seat",
        icon: "pause",
        title: `${agent.role} is ${stateWord(state).toLowerCase()}`,
        detail: `The seat cannot take work: ${stoppedLine(agent, nameOf)}.`,
        path: seatPath,
        // WHEN A PERSON PAUSED IT, where one did: the instant the condition
        // began, which is what orders it among the rest and what its row's
        // age says.
        ...(agent.paused?.at ? { at: agent.paused.at } : {}),
        ...(agent.last_error?.detail ? { technical: agent.last_error.detail } : {}),
        who,
      });
      continue;
    }
    // A FAILED LAST TURN IS NOT A STOP. The engine keeps `last_error` on a
    // seat that went on to idle or work again, so the row says what happened
    // to the turn and never calls the seat stopped (the roster files the same
    // seat with the idle ones, `People.tsx`).
    if (agent.last_error && !limited) {
      out.push({
        id: seatStateId(agent.agent_id),
        severity: "critical",
        subject: "seat",
        icon: "triangle-alert",
        title: `${agent.role}'s last turn failed`,
        // THE KIND IN PLAIN WORDS, never the machine code or the engine's
        // raw message, which rides along as `technical`.
        detail: failureWords(agent.last_error.kind).sentence,
        ...(agent.last_error.detail ? { technical: agent.last_error.detail } : {}),
        path: seatPath,
        at: agent.last_error.at,
        who,
      });
      continue;
    }
    // A live call that has not moved is the condition a spinning row hides.
    const call = agent.live_call;
    if (call?.in_progress) {
      const how = staleness(call.updated_at, now, agent.turn?.stage);
      if (how) {
        out.push({
          id: `stale-${agent.agent_id}-${call.turn_id}`,
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
          detail: `${call.phase} · ${roundLabel(call).text}`,
          // THE OVERVIEW, where the seat's current turn is drawn round by
          // round — the profile's default tab, so the path alone opens it.
          path: ["company", "org", "seats", String(agent.handle ?? agent.id)],
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
        id: `seat-budget-${agent.agent_id}`,
        severity: "caution",
        subject: "budget",
        icon: "coins",
        title: `${agent.role}'s ${PERIOD_ADJECTIVE[refusing.period]} token budget is refusing charges`,
        detail: `${tokensOf(refusing)} spent. Raise the limit, or wait until ${fmtDateTime(refusing.resets_at)}.`,
        // THE SEAT'S OWN CEILING, where it is raised: its row on Budgets.
        // A seat a remedy cannot name (no handle) keeps its profile's
        // Settings tab, where the ceiling is read beside its meter.
        ...(agent.handle
          ? { remedy: { setting: "seat_token_budget" as const, seat: agent.handle } }
          : { path: ["company", "org", "seats", agent.id], query: { tab: "settings" } }),
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
  const asked =
    (run.question ?? "").trim() ||
    (questionWithheld(run)
      ? `${QUESTION_WITHHELD}.`
      : "A coding run needs your answer to continue.");
  if (run.status === "reseed") return `${asked} Answering restarts the work.`;
  const ttl = run.pause_ttl_seconds;
  const parked = run.paused_at ? Date.parse(run.paused_at) : NaN;
  if (!(ttl > 0) || Number.isNaN(parked)) return asked;
  const left = parked + ttl * 1_000 - now;
  if (left <= 0) return `${asked} Answer soon or the work restarts.`;
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

/** A window's spend against its ceiling: "1,200 of 5,000 tokens". */
function tokensOf(w: BudgetWindow): string {
  return `${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens`;
}

/** A refusing window in words: the gate's own stamp where it has one, and the
 *  arithmetic where the window is full but no charge has been turned away yet. */
function refusalWords(w: BudgetWindow): string {
  const spent = `${tokensOf(w)} in ${w.window}.`;
  return w.refused_at
    ? `Turns are being declined at the budget gate; last refusal ${fmtDateTime(w.refused_at)}. ${spent}`
    : `No further charge fits, so turns are being declined at the gate. ${spent}`;
}

/** A near window's spend in words. */
function spentWords(w: BudgetWindow): string {
  return `${tokensOf(w)} in ${w.window} are spent.`;
}
