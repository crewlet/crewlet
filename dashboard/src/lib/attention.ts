/**
 * What needs a person.
 *
 * Every one of these conditions was already known to the dashboard and each
 * lived in a different screen: an executor parked on a question was a badge
 * on the board (and one whose box had been reclaimed appeared NOWHERE at
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

import type { AgentRow, BudgetWindow, OrgBudget, ExecutorRow } from "~/protocol/index.ts";
import { entryLimits, limitDetail, limitTitle, type EntryLimit } from "./providerLimit.ts";
import { PERIOD_ADJECTIVE, waitedOn } from "~/lib/budget.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { McpAlarm } from "~/contract/mcp.ts";
import type { AtlasReviewItem } from "~/contract/atlas.ts";
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
import { fmtAt, possessive } from "./format.ts";
import { callsOf, turnById } from "./seatturn.ts";
import type { Remedy } from "~/protocol/remedy.ts";
import { andList } from "./words.ts";
import { inboxRowKey, type InboxRowKey } from "~/app/routes.ts";
import type { AdvisorNotice, AdvisorRedirectMoved } from "~/protocol/advisor.ts";
import type { AdvisorNoticesAnswer } from "~/protocol/advisorFindings.ts";
import { alarmHome, alarmTitle, alarmsLink, drawnOnBackups } from "./alarms.ts";
import { ALARM } from "./mcpAlarmWords.ts";
import { HELD_CLASS, heldItem, heldReason, heldSetting } from "./heldConversation.ts";
import { OAUTH_REDIRECT_MOVED, PAUSED_SEAT_CLASSES } from "~/contract/advisor.ts";
import { unreadSentences, unreadTitle } from "./advisorUnread.ts";

/** A tool server alarm's tone as the queue's severity. */
const SEVERITY_OF_TONE = { danger: "critical", warning: "caution", info: "info" } as const;

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
export type Subject =
  "engine" | "budget" | "run" | "seat" | "round" | "tools" | "setup" | "alarms" | "holds" | "atlas";

/** Each subject as the reader's own words, in the order the clause lists them. */
export const SUBJECTS: Record<Subject, string> = {
  engine: "the engine and this node's link to it",
  budget: "the company's and every seat's token budget",
  run: "every executor waiting on an answer",
  seat: "every seat's own state",
  round: "every round still in flight",
  tools: "every MCP server the agents call",
  setup: "the company's setup",
  alarms: "every alarm the engine raised",
  holds: "every conversation the engine holds",
  atlas: "what you set in the atlas",
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
 *  - `live`: Live › Now running, BESIDE the rounds and executors it is about
 *    — a quiet round is marked on its own running-turn row (`LiveTurnRow`),
 *    and a parked executor is a row of Waiting on a person with its Answer. A
 *    round that has not moved is watched, not decided, and an executor parked
 *    on a question already reaches the person it asks through their own
 *    decisions (`decisions`), so the company-wide list of them is live.
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
  tools: "seat",
  setup: "seat",
  alarms: "seat",
  holds: "seat",
  atlas: "seat",
};

/**
 * The subjects shown in one place, as the clause its quiet state says, less
 * the ones this reader is not shown (`unwatched`): a reader the MCP section
 * does not open for is never asked its alarms, and a quiet band claiming to
 * have watched them would claim a check nobody made.
 */
export function watchedIn(where: Where, unwatched: readonly Subject[] = []): string {
  return andList(
    (Object.keys(SUBJECTS) as Subject[])
      .filter((subject) => WHERE_OF[subject] === where && !unwatched.includes(subject))
      .map((subject) => SUBJECTS[subject]),
    // THE SERIAL COMMA IS LOAD-BEARING HERE: the clauses carry their own
    // "and" ("the company's and every seat's token budget").
    { serialComma: true },
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
export function conditionKey(id: string): InboxRowKey {
  return inboxRowKey("condition", id);
}

/** Where a condition is answered, and what the button that opens it says. */
type Destination =
  | { path: string[]; query?: Record<string, string>; action: string }
  | { path?: undefined; query?: undefined; action?: undefined };

export type Attention = AttentionFacts & Destination;

interface AttentionFacts {
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
  /**
   * How the condition ends, in a sentence: on its own ("when the limit
   * lifts") or by a person ("once someone resumes the seat"). The pane says
   * it under the condition.
   */
  clears: string;
  /**
   * Where the limit behind the condition is raised, for a condition a limit
   * caused (a spent budget): drawn in place of `path`, behind the gate of the
   * editor it opens (`components/LimitRemedy.tsx`).
   */
  remedy?: Remedy;
  /** The instant this became true, for ordering within a severity. */
  at?: string;
  who?: string;
  /**
   * How many nodes a PLATFORM NOTICE stands on (none for a finding's, which
   * names no node): set exactly on the rows the advisor's notices raise, which
   * are the ones a reader may put off for a while (`lib/putOff.ts`), and how a
   * mark tells that the condition has spread since.
   */
  spread?: number;
  /**
   * The alarm kind a platform notice stands for, where one does (an alarm's
   * own notice, or a finding an alarm raises): how the engine panel says
   * which of the alarms it counts are in Needs you.
   */
  alarm?: string;
  /**
   * The redirect URI to add and the OAuth clients still registered without it,
   * on the `oauth_redirect_moved` notice's row alone: its pane draws them.
   */
  redirect?: AdvisorRedirectMoved;
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
   * The executors' DURABLE rows, which is what is still waiting.
   *
   * THE WAITING ROW NEEDS THE RECORD, not the live entry: how long a person
   * has left to answer is the run's pause window (`pause_ttl_seconds`)
   * counted from `paused_at`, and only the durable row
   * carries the window. The live entry says a run is waiting; the record says
   * for how much longer its box is held.
   */
  runs: ExecutorRow[];
  /** `null` before the first report: nothing to judge, so nothing is raised. */
  budget: OrgBudget | null;
  engine: EngineHealth | null;
  connected: boolean;
  authRejected: boolean;
  now: number;
  /** A person's name by their handle, off the chart: who paused a seat is said by name. */
  nameOf: NameOf;
  /**
   * The MCP alarms standing on the node answering `mcp_servers_status`, each
   * server with the gesture that fixes it: empty for a reader the MCP section
   * does not open for, who is never asked them. The engine judged each one;
   * this only puts it into words.
   */
  mcpAlarms: readonly McpAlarm[];
  /**
   * The platform notices the Advisor answers (`advisor_notices`): empty for a
   * reader it does not answer. The engine decided each one and when it ends,
   * and a finding's notice carries the finding's words and its fix; this only
   * words it.
   */
  notices: readonly AdvisorNoticesAnswer["notices"][number][];
  /** What the advisor could not read, for the same reader; empty when it read everything. */
  setupUnread: readonly string[];
  /**
   * What contradicts the reader's own word in the atlas: a fact or a relation
   * they set or confirmed that a newer report differs from (`atlas_review`
   * asked with `mine`). Empty for a reader who does not hold Atlas.
   */
  atlasDrift: readonly AtlasReviewItem[];
}

/** A value as a sentence quotes it. */
function quoted(value: { text: string } | undefined): string {
  return value ? `"${value.text}"` : "it";
}

/**
 * A fact or a relation the reader set, which a newer report now differs
 * from. The atlas shows both and settles neither, so the person who said it
 * is told: it is the one thing in Review that reaches an Inbox.
 */
function driftRow(item: AtlasReviewItem): Attention {
  const fact = item.fact;
  const other = fact?.others?.find((one) => !one.denied && one.value);
  return {
    id: `atlas-drift-${item.id}`,
    severity: "caution",
    subject: "atlas",
    icon: "map",
    title: `${item.entry.name}: a newer report differs from what you set`,
    detail: fact
      ? other
        ? `You set ${quoted(fact.value)}. A newer report says ${quoted(other.value)}. The atlas shows both until somebody decides.`
        : `You set ${quoted(fact.value)}, and a newer report says otherwise. The atlas shows both until somebody decides.`
      : "A newer report says otherwise about a relation you set. The atlas shows both until somebody decides.",
    path: ["company", "atlas", item.entry.id],
    query: { tab: "sources" },
    action: "Open the entry to decide",
    clears: "Clears once somebody keeps one of the two.",
    at: item.since,
  };
}

/**
 * WHERE A NOTICE IS FIXED, by the condition it is about, where that is one
 * gesture on one screen rather than the section that lists the alarm: the
 * backup a missing one asks for is taken from Backups & retention's own
 * dialog (`take=backup` opens it), and a bucket near its ceiling is freed or
 * moved on File storage, which shows its usage and its collector.
 */
const NOTICE_FIXES: Readonly<
  Record<string, { path: string[]; query?: Record<string, string>; action: string; clears: string }>
> = {
  backup_age: {
    path: ["settings", "backups"],
    query: { take: "backup" },
    action: "Take a backup",
    clears:
      "Clears on its own once a backup the engine verified covers every log. A copy taken outside the engine is not one it can see.",
  },
  objects_bucket_near_ceiling: {
    path: ["settings", "storage"],
    action: "Open File storage",
    clears:
      "Clears on its own once the bucket has a fifth of its room free: when the collector removes files nothing names, or the files move to a larger store.",
  },
};

/** A standing alarm's row: where it is fixed, its nodes and since when. */
function alarmRow(kind: string, n: AdvisorNotice): Attention {
  const home = alarmHome(kind);
  const fix = NOTICE_FIXES[kind];
  return {
    id: `alarm-${kind}`,
    severity: "critical",
    subject: "alarms",
    icon: "triangle-alert",
    title: alarmTitle({ kind, title: n.title }),
    detail: n.person || "The engine raised this alarm and it has not cleared on its own.",
    ...(n.nodes?.length ? { technical: `Standing on ${n.nodes.join(", ")}.` } : {}),
    ...(fix
      ? { path: fix.path, ...(fix.query ? { query: fix.query } : {}), action: fix.action }
      : drawnOnBackups(kind)
        ? { ...alarmsLink(), action: `Open ${home.label}` }
        : { path: home.path, action: `Open ${home.label}` }),
    clears: fix?.clears ?? "Clears when the alarm clears.",
    spread: n.nodes?.length ?? 0,
    alarm: kind,
    ...(n.since ? { at: n.since } : {}),
  };
}

const ORDER: Record<Severity, number> = { critical: 0, caution: 1, info: 2 };

const BUDGET_CLEARS = "Clears on its own when the window resets, or once the limit is raised.";

/**
 * What lifts a paused or stopped seat, and the button to where it is lifted:
 * a pause or a missing placement waits on a person, never on itself.
 */
function stopRemedy(agent: AgentRow): { action: string; clears: string } {
  switch (agent.stopped_reason) {
    case "paused":
      return {
        action: "Open the seat to resume it",
        clears: "Clears once someone resumes the seat.",
      };
    case "unplaced":
      return {
        action: "Open the seat",
        clears: "Clears once an admin places the seat on a node.",
      };
    case "provider":
      return { action: "Open the seat", clears: "Clears on its own when the limit lifts." };
    default:
      return { action: "Open the seat", clears: "Clears once the seat can run again." };
  }
}

export function attentionQueue(input: AttentionInput): Attention[] {
  const out: Attention[] = [];
  const { agents, runs, budget, engine, connected, authRejected, now, nameOf, mcpAlarms } = input;
  const { notices, setupUnread, atlasDrift } = input;

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
      clears: "Clears once someone signs in on this browser.",
    });
  } else if (!connected) {
    out.push({
      id: "offline",
      severity: "critical",
      subject: "engine",
      icon: "power",
      title: "No connection to the engine",
      detail: "Showing the last state received.",
      clears: "Clears on its own when the engine answers again.",
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
      action: "Open configuration",
      clears: "Clears once an admin activates a company configuration.",
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
      action: "Open nodes",
      clears: shed
        ? "Clears on its own when the node reaches its configuration again."
        : "Clears on its own when the node converges on the active configuration.",
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
      action: "Open nodes",
      clears: "Ends when its running turns finish and the node stops.",
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
      detail: `${refusalWords(orgRefusing, now)} Raise the company limit, or wait until ${fmtAt(orgRefusing.resets_at, now)}.`,
      clears: BUDGET_CLEARS,
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
      detail: `${spentWords(orgNear)} Raise the company limit, or wait until ${fmtAt(orgNear.resets_at, now)}.`,
      clears: BUDGET_CLEARS,
      remedy: { setting: "company_token_budget" },
    });
  }

  // --- executors -----------------------------------------------------------
  // FROM THE DURABLE ROWS, not from the projection: see `runs` above.
  for (const run of runs) {
    // THE ENGINE'S OWN WORDS. `awaiting_input` is not one of them, so this
    // condition never fired and a run parked on a question reached the queue
    // through no path at all. `reseed` is the same fact one step worse — the
    // box was reaped past its pause TTL and only the question survives.
    if (run.status !== "awaiting_clarification" && run.status !== "reseed") continue;
    // NOR ONE WHOSE ENDING IS DECIDED, whatever status it was ended in: it is
    // over, and no answer reaches it.
    if (run.ending) continue;
    out.push({
      id: `executor-${run.turn_id}`,
      severity: "caution",
      subject: "run",
      icon: "circle-question-mark",
      title: `${run.role || run.agent_handle} is waiting on an answer`,
      detail: waitingDetail(run, now),
      // THE RUN'S OWN PATH. It was a `?run=` query on the board's list,
      // which the board stopped reading when a run became an object with an
      // address.
      path: ["live", "executors", run.turn_id],
      action: "Open the run",
      clears: "Clears once someone answers its question.",
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
      clears: "Clears on its own when the limit lifts.",
      // THE COMPANY'S MODELS, where a fallback is given or a plan changed,
      // behind the same gate as the screen it opens (lib/remedy.ts).
      remedy: { setting: "model_providers" },
      at: limit.since,
      ...whoOfLimit(limit),
    });
  }

  // --- MCP servers -----------------------------------------------------------
  // ONE ROW PER SERVER AN ALARM STANDS ON, for as long as the engine's
  // evaluation holds it: the engine decided the span, the server and the
  // agent, and these are only its words. The row opens the server's page,
  // where its instances say the rest.
  for (const alarm of mcpAlarms) {
    const words = ALARM[alarm.kind];
    for (const entry of alarm.entries) {
      out.push({
        id: `mcp-${alarm.kind}-${entry.key}`,
        severity: SEVERITY_OF_TONE[words.tone],
        subject: "tools",
        icon: words.tone === "info" ? "clock" : "triangle-alert",
        title: words.title({ server: entry.key, who: entry.seat, node: "", count: 0 }),
        detail: alarm.person,
        // THE ENGINE'S MEASUREMENT, what each instance said included, behind
        // the disclosure for whoever is fixing it.
        technical: entry.detail,
        path: ["settings", "mcp-tools", "servers", entry.key],
        // AN ALARM THE ENGINE FIXES WITH A SIGN-IN OPENS IT, on the server's page.
        ...(entry.fix.kind === "sign_in" ? { query: { flow: "signin" } } : {}),
        action: entry.fix.kind === "sign_in" ? "Sign in to the server" : "Open the server",
        clears: "Clears when the engine next finds the server working.",
        ...(entry.since ? { at: entry.since } : {}),
        ...(entry.seat ? { who: entry.seat } : {}),
      });
    }
  }

  // THE FINDINGS DRAWN FOR SEATS THE ENGINE PAUSED, one per failure, each
  // naming the agents it holds and the fix: what says those pauses ([seats]).
  const pauseNotices: AdvisorNotice[] = [];

  // --- the platform notices -------------------------------------------------
  // WHAT THE ENGINE SAYS NEEDS AN ADMINISTRATOR, open for as long as it holds.
  // A finding's notice is the finding's own words, its fix previewed on the
  // advisor; an alarm's opens the section that draws it. An MCP alarm is
  // drawn per server above, so its notice is not drawn twice.
  for (const n of notices) {
    const at = n.since ? { at: n.since } : {};
    if (n.class === "alarm") {
      const kind = n.subject.name ?? "";
      if (kind === "" || kind.startsWith("mcp_")) continue;
      out.push(alarmRow(kind, n));
      continue;
    }
    // A CONVERSATION THE ENGINE HOLDS: nothing to decide, and the row says what
    // releases it, opening the item it is about where there is one.
    if (n.class === HELD_CLASS) {
      const seat = n.subject.seat ?? "";
      const item = heldItem(n);
      out.push({
        id: `held-${seat}-${n.subject.name ?? ""}`,
        severity: "caution",
        subject: "holds",
        icon: "pause",
        title: `Held by the engine: ${heldReason(n)}`,
        detail:
          "The engine stopped retrying this after 2 identical failures and is holding the " +
          "conversation. There is nothing for you to decide.",
        path: item ? ["worklet", item] : ["company", "org", "seats", seat],
        action: item ? "Open the item" : "Open the seat",
        clears: `Clears when somebody writes on the conversation or the seat's ${heldSetting(n)} change.`,
        ...(seat ? { who: seat } : {}),
        ...at,
      });
      continue;
    }
    // OAUTH CLIENTS A MOVED ENGINE ADDRESS LEFT BEHIND: the engine's words, and
    // the URI to add with each client, which the pane draws. It spreads as more
    // clients are left behind.
    if (n.class === OAUTH_REDIRECT_MOVED && n.redirect) {
      out.push({
        id: `notice-${OAUTH_REDIRECT_MOVED}`,
        severity: "caution",
        subject: "setup",
        icon: "unlink",
        title: n.title || "Add the new redirect URI to your OAuth clients",
        detail:
          n.person || "This engine's address changed, so each OAuth client below must list it.",
        clears: "Clears for each client once it signs in through the new address.",
        redirect: n.redirect,
        spread: n.redirect.clients.length,
        ...at,
      });
      continue;
    }
    const f = n.finding;
    // AN ALARM'S FINDING THIS ANSWER COULD NOT RAISE (its node has not evaluated
    // yet) is still said, in the alarm's words.
    if (!f) {
      if (n.alarm) out.push(alarmRow(n.alarm, n));
      continue;
    }
    if ((PAUSED_SEAT_CLASSES as readonly string[]).includes(n.class)) pauseNotices.push(n);
    // AN ALARM'S FINDING IS FIXED WHERE THE ALARM IS: the backup the
    // no-verified-backup finding asks for is taken on Backups & retention.
    const fix = n.alarm ? NOTICE_FIXES[n.alarm] : undefined;
    out.push({
      id: `setup-${f.id}`,
      severity: "critical",
      subject: "setup",
      icon: "list-checks",
      title: f.title,
      detail: f.fix,
      ...(fix
        ? { path: fix.path, ...(fix.query ? { query: fix.query } : {}), action: fix.action }
        : { path: ["advisor", "findings", f.id], action: "Open the finding" }),
      clears: fix?.clears ?? "Clears once the Advisor no longer finds it.",
      spread: n.nodes?.length ?? 0,
      ...(n.alarm ? { alarm: n.alarm } : {}),
      ...at,
    });
  }
  // A SOURCE THE ADVISOR COULD NOT READ IS SAID, so a blocker it would have
  // raised never leaves the queue without a word.
  if (setupUnread.length > 0) {
    out.push({
      id: "setup-unread",
      severity: "caution",
      subject: "setup",
      icon: "list-checks",
      title: unreadTitle(setupUnread),
      detail: unreadSentences(setupUnread).join(" "),
      path: ["advisor"],
      action: "Open the Advisor",
      clears: "Clears once the Advisor reads every source again.",
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
    // A SEAT THE ENGINE PAUSED FOR A FAILURE IS SAID BY THAT FAILURE'S
    // FINDING above, which names it and the fix: its own row beside it said
    // the one pause twice, the second offering a resume that fails the same
    // way. A finding about one seat names it in its subject; one about several
    // names every agent its failure holds, which is every seat the engine
    // paused for it.
    const saidByFinding =
      agent.paused?.by_kind === "system" &&
      pauseNotices.some((n) => n.subject.kind !== "seat" || n.subject.seat === agent.handle);
    if (
      (state === "paused" || state === "stopped") &&
      agent.stopped_reason !== "budget" &&
      !(limited && agent.stopped_reason === "provider")
    ) {
      if (saidByFinding) continue;
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
        ...stopRemedy(agent),
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
        title: `${possessive(agent.role)} last turn failed`,
        // THE KIND IN PLAIN WORDS, never the machine code or the engine's
        // raw message, which rides along as `technical`.
        detail: failureWords(agent.last_error.kind).sentence,
        ...(agent.last_error.detail ? { technical: agent.last_error.detail } : {}),
        path: seatPath,
        action: "Open the seat",
        clears: "Clears when the seat starts its next turn.",
        at: agent.last_error.at,
        who,
      });
      continue;
    }
    // A live call that has not moved is the condition a spinning row hides —
    // each turn's on its own, since a seat may run several.
    for (const call of callsOf(agent)) {
      if (!call.in_progress) continue;
      const how = staleness(call.updated_at, now, turnById(agent, call.turn_id)?.stage);
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
          action: "Open the seat",
          clears: "Clears when the round answers or the turn ends.",
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
        title: `${possessive(agent.role)} ${PERIOD_ADJECTIVE[refusing.period]} token budget is refusing charges`,
        detail: `${tokensOf(refusing)} spent. Raise the limit, or wait until ${fmtAt(refusing.resets_at, now)}.`,
        clears: BUDGET_CLEARS,
        // THE SEAT'S OWN CEILING, where it is raised: its row on Budgets.
        // A seat a remedy cannot name (no handle) keeps its profile's
        // Settings tab, where the ceiling is read beside its meter.
        ...(agent.handle
          ? { remedy: { setting: "seat_token_budget" as const, seat: agent.handle } }
          : {
              path: ["company", "org", "seats", agent.id],
              query: { tab: "settings" },
              action: "Open the seat's settings",
            }),
        at: refusing.refused_at,
      });
    }
  }

  // --- the atlas ------------------------------------------------------------
  // ONLY DRIFT ON WHAT THE READER SET reaches here. Everything else that
  // waits in the atlas waits in its own Review, for every holder together.
  for (const item of atlasDrift) out.push(driftRow(item));

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
function waitingDetail(run: ExecutorRow, now: number): string {
  const asked =
    (run.question ?? "").trim() ||
    (questionWithheld(run)
      ? `${QUESTION_WITHHELD}.`
      : "Its executor needs your answer to continue.");
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
function refusalWords(w: BudgetWindow, now: number): string {
  const spent = `${tokensOf(w)} in ${w.window}.`;
  return w.refused_at
    ? `Turns are being declined at the budget gate; last refusal ${fmtAt(w.refused_at, now)}. ${spent}`
    : `No further charge fits, so turns are being declined at the gate. ${spent}`;
}

/** A near window's spend in words. */
function spentWords(w: BudgetWindow): string {
  return `${tokensOf(w)} in ${w.window} are spent.`;
}
