/**
 * Scenario helpers: pure functions from one demo company to another, which a
 * topic composes into the states it wants a proposal looked at in.
 *
 * Every helper takes a company and returns a NEW one, never changing the one
 * it was given, so `compose(resumeAll, seatWorking("pm"))` is a scenario and
 * the base company is still the base company. A helper changes the inputs
 * the engine derives from (the document, a seat's live state, a task), never
 * a derived answer, so every screen that reads the change agrees about it.
 *
 *     import { compose, resumeAll, seatWorking, scenario } from "../../fixtures/scenarios.ts";
 *
 *     scenarios: [
 *       scenario("today", "As today", (c) => c),
 *       scenario("busy", "Both agents working", compose(resumeAll, seatWorking("pm", "FAIR-1"), seatWorking("engineer", "FAIR-3"))),
 *     ],
 */

import type { Scenario } from "../harness/defineTopic.ts";
import { copyCompany, documentOf, type DemoCompany } from "../harness/engine/company.ts";
import { validate } from "../harness/engine/config.ts";
import {
  agentRoles,
  handleOf,
  roster,
  scheduleRows,
  slugify,
  unitKeyOf,
} from "../harness/engine/derive.ts";
import { fixtureId } from "../harness/engine/ids.ts";
import {
  afterRevision,
  blockOf,
  reportFor,
  type DemoSurfaceState,
} from "../harness/engine/loop.ts";
import {
  PROVISION_SOURCE,
  provisioning,
  RECONCILE_LOOP,
  type ProvisionedKind,
} from "../harness/engine/provision.ts";
import { presentMachineToken, signInAsPerson, signInWithToken } from "../harness/engine/iam.ts";
import { actorOf, recordOwnerOf, viewerOf } from "../harness/engine/principal.ts";
import { seal, varSuffix } from "../harness/engine/refs.ts";
import { APP_NAMES } from "../harness/engine/vendors.ts";
import { idleOverlay, pause, resume } from "../harness/engine/seats.ts";
import { appendRevision } from "../harness/engine/revisions.ts";
import { fireRecords, ledgerOrder, type FireOutcome } from "../harness/engine/schedules.ts";
import { submit, type Submission } from "../harness/engine/setup.ts";
import { findItem } from "../harness/engine/work.ts";
import type { AVATAR_CHARACTERS, AVATAR_COLORS } from "~/contract/avatar.ts";
import type { StoppedReason } from "~/contract/wire.ts";
import type {
  ConfigRole,
  ConfigUnit,
  EventRecord,
  LiveCall,
  ScheduleSpec,
} from "~/protocol/types.ts";

export type { DemoCompany } from "../harness/engine/company.ts";
export type { FireOutcome } from "../harness/engine/schedules.ts";
export { fairsplit } from "./fairsplit.ts";

/** A Crewlet character an agent seat can be drawn as. */
export type AvatarCharacter = (typeof AVATAR_CHARACTERS)[number];
/** A colour an agent seat can be drawn in. */
export type AvatarColor = (typeof AVATAR_COLORS)[number];

/** A step a scenario is made of. */
export type Step = (company: DemoCompany) => DemoCompany;

/** A scenario, for a topic's `scenarios` list. */
export function scenario(id: string, label: string, apply: Step): Scenario {
  return { id, label, apply };
}

/** The steps, applied in order, as one. */
export function compose(...steps: Step[]): Step {
  return (company) => steps.reduce((c, step) => step(c), company);
}

/** A step written as a change to a private copy. */
function edit(change: (c: DemoCompany) => void): Step {
  return (company) => {
    const c = copyCompany(company);
    change(c);
    return c;
  };
}

/** The instant the company reads as now, as milliseconds. */
const nowOf = (c: DemoCompany) => Date.parse(c.now);

/** The company as it is: the default scenario. */
export const asToday: Step = (c) => c;

/** Every agent seat resumed: nobody paused, nothing stopped. */
export const resumeAll: Step = edit((c) => {
  for (const seat of Object.values(c.seats)) resume(seat);
});

/** One seat paused by the viewer, `minutesAgo` ago, for `reason`. */
export function pauseSeat(handle: string, reason = "", minutesAgo = 5): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`pauseSeat: there is no agent seat "${handle}".`);
    pause(seat, actorOf(c), new Date(nowOf(c) - minutesAgo * 60_000).toISOString(), reason, false);
  });
}

/**
 * One seat on a turn right now: running its execute phase on `itemKey`, a
 * tool call in flight, `secondsIn` seconds into the turn.
 */
export function seatWorking(handle: string, itemKey = "", secondsIn = 40): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`seatWorking: there is no agent seat "${handle}".`);
    const item = itemKey ? findItem(c, itemKey) : null;
    const started = new Date(nowOf(c) - secondsIn * 1000).toISOString();
    const turnId = fixtureId(`${handle}-live-turn`);
    const workItem = item
      ? { backend: "native", id: item.task.id, key: item.task.key, project: item.task.project }
      : null;
    const call: LiveCall = {
      turn_id: turnId,
      phase: "execute",
      iteration: 1,
      model: "claude-sonnet-5",
      trigger: item ? { type: "work_item_comment", item: item.task.key } : { type: "schedule" },
      prompt: "",
      prompt_messages: [],
      response: "",
      round_narration: [],
      input_tokens: 18_400,
      output_tokens: 1_240,
      total_tokens: 19_640,
      tool_executions: [],
      rounds: [],
      round_num: 2,
      rounds_used: 2,
      max_rounds: 12,
      round_started_at: new Date(nowOf(c) - 6_000).toISOString(),
      running_call: {
        round: 2,
        name: "search_knowledge",
        arguments: '{"q":"leftover cent rule"}',
        started_at: new Date(nowOf(c) - 2_000).toISOString(),
      },
      in_progress: true,
      updated_at: c.now,
      started_at: started,
      node: c.engine.health.node,
      work_item: workItem,
      versions: { prompt: 1, response: 1, narration: 1, executions: 1, rounds: 1 },
    };
    seat.overlay = {
      ...seat.overlay,
      activity: "working",
      stopped_reason: null,
      paused: null,
      current_phase: "execute",
      current_iteration: 1,
      turn: {
        turn_id: turnId,
        work_item: workItem,
        started_at: started,
        stage: "phase",
        node: c.engine.health.node,
      },
      live_call: call,
      live_call_seq: 1,
    };
  });
}

/**
 * One seat waiting on the viewer: its coding run parked on `question`, which
 * reaches the viewer's inbox as a decision.
 */
export function seatNeedsYou(handle: string, question: string, itemKey = ""): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`seatNeedsYou: there is no agent seat "${handle}".`);
    const item = itemKey ? findItem(c, itemKey) : null;
    const turnId = fixtureId(`${handle}-parked-turn`);
    const at = new Date(nowOf(c) - 12 * 60_000).toISOString();
    const workItem = item
      ? { backend: "native", id: item.task.id, key: item.task.key, project: item.task.project }
      : null;
    seat.overlay = {
      ...seat.overlay,
      activity: "needs",
      stopped_reason: null,
      paused: null,
      turn: {
        turn_id: turnId,
        work_item: workItem,
        started_at: at,
        stage: "parked",
        node: c.engine.health.node,
      },
    };
    const row = roster(documentOf(c)).find((r) => r.handle === handle);
    c.runs.live.push({
      turn_id: turnId,
      role: row?.role ?? handle,
      agent_handle: handle,
      agent_id: row?.agent_id ?? "",
      coding_agent: "claude-code",
      sandbox_id: fixtureId(`${handle}-box`).slice(0, 12),
      task: item ? `Implement ${item.task.key}: ${item.task.title}` : "A coding run",
      status: "awaiting_clarification",
      started_at: at,
      question,
      audience: viewerOf(c).name,
      work_item: workItem,
      owner: c.engine.health.node,
      paused_at: at,
    });
    c.runs.records.push({
      turn_id: turnId,
      launch_id: fixtureId(`${handle}-launch`),
      agent_handle: handle,
      role: row?.role ?? handle,
      status: "awaiting_clarification",
      coding_agent: "claude-code",
      placement: "direct",
      task_description: item ? `Implement ${item.task.key}: ${item.task.title}` : "A coding run",
      question,
      audience: viewerOf(c).name,
      audience_handles: [viewerOf(c).handle],
      branch: item ? `fair/${item.task.key.toLowerCase()}` : "work",
      trace_id: fixtureId(`${handle}-parked-trace`),
      owner: c.engine.health.node,
      box_exists: true,
      paused_at: at,
      pause_ttl_seconds: 6 * 3600,
      started_at: at,
      updated_at: at,
      answerable_in_chat: false,
      work_item: workItem,
    });
  });
}

/** One seat stopped on its spent daily budget. */
export function seatOutOfBudget(handle: string): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`seatOutOfBudget: there is no agent seat "${handle}".`);
    seat.overlay = { ...seat.overlay, activity: "stopped", stopped_reason: "budget", paused: null };
    const day = new Intl.DateTimeFormat("en-CA", {
      timeZone: String(documentOf(c).timezone ?? "UTC"),
    }).format(new Date(c.now));
    c.spend.daily.push({
      day,
      handle,
      phase: "execute",
      model: "claude-sonnet-5",
      provider: "default",
      input: 3_600_000,
      output: 400_000,
      cache_read: 1_900_000,
      calls: 140,
      turns: 31,
      failed: 0,
    });
  });
}

/** Every open question to the viewer answered, and every notice read. */
export const inboxZero: Step = edit((c) => {
  const me = viewerOf(c).handle;
  const who = actorOf(c);
  for (const notice of c.work.inbox[recordOwnerOf(c.principal)] ?? []) notice.read = true;
  for (const item of c.work.items) {
    for (const ask of item.comments.filter((x) => me !== "" && x.ask === me)) {
      if (item.comments.some((x) => x.answers === ask.id)) continue;
      item.comments.push({
        id: fixtureId(`answer-${ask.id}`),
        task: item.task.id,
        author: who.name,
        author_kind: who.kind,
        body:
          ask.decision?.options.find((o) => o.id === ask.decision?.recommended)?.label ??
          "Yes, go ahead.",
        answers: ask.id,
        ...(ask.decision?.recommended ? { choice: ask.decision.recommended } : {}),
        created_at: c.now,
      });
    }
  }
});

// ---- who the browser is ---------------------------------------------------

/** One segment of a compound name (`iam.segment`). */
const SEGMENT = "[a-z0-9]+(?:-[a-z0-9]+)*";
/** A person's login: segments joined by dots (`iam.ValidLogin`). */
const PERSON_LOGIN = new RegExp(`^${SEGMENT}(?:\\.${SEGMENT})+$`);
/** A machine's login: segments joined by colons (`iam.ValidMachineHandle`). */
const MACHINE_LOGIN = new RegExp(`^${SEGMENT}(?::${SEGMENT})+$`);

/**
 * Refuse, naming the helper, a binding the engine would not honour: a seat
 * the chart does not hold, or an agent's, since a credential is bound only to
 * a HUMAN seat (`session.ResolveSeat`).
 */
function seatFor(c: DemoCompany, helper: string, handle: string): void {
  const role = allRoles(documentOf(c)).find((r) => handleOf(r) === handle);
  if (!role) throw new Error(`${helper}: there is no seat "${handle}".`);
  if (role.kind !== "human") {
    throw new Error(`${helper}: "${handle}" is an agent's seat, and only a human seat is bound.`);
  }
}

/**
 * The browser signed in with Tier A token `tokenId`, exchanged for a one-hour
 * session (`POST /auth/token`) eight minutes ago, so fifty-two minutes are
 * left. FairSplit's own default: `signedInAsToken()` is Miles's `review`
 * token, bound to his seat and granted admin.
 *
 * As the engine resolves it: login `token:<id>`; no name of its own, since a
 * token has no directory row; bound to `seat`, the guard re-kinds it a PERSON
 * acting as the seat, so the session answer's `kind` is `person` and the
 * viewer names the seat; bound to none (`seat` ""), it stays a `machine`
 * acting under its login. `grants` are the token entry's.
 *
 *     signedInAsToken("review", "miles")
 */
export function signedInAsToken(
  tokenId = "review",
  seat = "miles",
  grants: readonly string[] = ["admin"],
): Step {
  return edit((c) => {
    if (seat) seatFor(c, "signedInAsToken", seat);
    signInWithToken(c, {
      tokenId,
      seat,
      grants,
      at: new Date(nowOf(c) - 8 * 60_000).toISOString(),
    });
  });
}

/** Who `signedInAsPerson` signs in. */
export interface SignedInPerson {
  /** Their own name, as their directory row holds it: what the frame calls them. */
  name: string;
  /** Their dotted login, such as `ahmad.mollapour`. */
  login: string;
  /** The human seat they hold (every person holds one); Miles's by default. */
  seat?: string;
  /** The grants on their row; `admin` carries every app with it. Admin by default. */
  grants?: readonly string[];
}

/**
 * The browser signed in as a PERSON, with their password, two hours ago: a
 * session of the person's own (`kind` `person`), their own name and login,
 * bound to the human seat they hold, with the grants on their row and a
 * week's absolute deadline.
 *
 *     signedInAsPerson({ name: "Ahmad Mollapour", login: "ahmad.mollapour", grants: ["tracker"] })
 */
export function signedInAsPerson(person: SignedInPerson): Step {
  return edit((c) => {
    const seat = person.seat ?? "miles";
    seatFor(c, "signedInAsPerson", seat);
    if (!PERSON_LOGIN.test(person.login)) {
      throw new Error(
        `signedInAsPerson: "${person.login}" is not a person's login, which is dotted words such as ahmad.mollapour.`,
      );
    }
    signInAsPerson(c, {
      name: person.name,
      login: person.login,
      seat,
      grants: person.grants ?? ["admin"],
      at: new Date(nowOf(c) - 2 * 3_600_000).toISOString(),
    });
  });
}

/** Which service account `signedInAsServiceAccount` presents. */
export interface SignedInServiceAccount {
  /** Its machine login, `class:id`, such as `ci:deploy`. */
  login: string;
  /** The grants its token was minted with. */
  grants: readonly string[];
  /** A seat it is bound to, which it then acts as; none by default. */
  seat?: string;
  /** A name on its directory row, which a machine's rarely holds; none by default. */
  name?: string;
}

/**
 * The browser presenting a service account's machine token (`pat:<id>`) on
 * every request: no session, so the session answer carries no lineage and no
 * deadline. Unbound, it is a `machine` acting under its own login, with its
 * own record and no seat; bound to `seat`, it acts as that seat's holder.
 *
 * THE SHIPPED DASHBOARD HOLDS NO BEARER (it signs in with a session cookie),
 * so this is a state a proposal about machine credentials draws rather than
 * one a browser reaches today.
 *
 *     signedInAsServiceAccount({ login: "ci:deploy", grants: ["tracker"] })
 */
export function signedInAsServiceAccount(account: SignedInServiceAccount): Step {
  return edit((c) => {
    if (!MACHINE_LOGIN.test(account.login)) {
      throw new Error(
        `signedInAsServiceAccount: "${account.login}" is not a machine login, which is class:id such as ci:deploy.`,
      );
    }
    if (account.seat) seatFor(c, "signedInAsServiceAccount", account.seat);
    presentMachineToken(c, { ...account, at: c.now });
  });
}

/** What `addAgent` needs to know about the new seat. */
export interface NewAgent {
  name: string;
  handle: string;
  /** The unit it joins, by key or name; the root when absent. */
  unit?: string;
  goal?: string;
  character?: AvatarCharacter;
  color?: AvatarColor;
  /** The seats it manages, by handle, or whole units, by key. */
  manages?: string[];
}

/** One more agent seat, idle, in a unit or at the root. */
export function addAgent(agent: NewAgent): Step {
  return edit((c) => {
    const doc = structuredClone(documentOf(c));
    const role: ConfigRole = { name: agent.name, handle: agent.handle, llm: "default" };
    if (agent.goal) role.goal = agent.goal;
    if (agent.manages) role.manages = agent.manages;
    if (agent.character || agent.color) {
      role.avatar = {
        ...(agent.character ? { character: agent.character } : {}),
        ...(agent.color ? { color: agent.color } : {}),
      };
    }
    if (agent.unit) {
      const unit = findUnit(doc.units, agent.unit);
      if (!unit) throw new Error(`addAgent: there is no unit "${agent.unit}".`);
      (unit.roles ??= []).push(role);
    } else {
      (doc.roles ??= []).push(role);
    }
    replace(c, doc, `Added ${agent.name}`);
  });
}

/** What `addTeam` needs to know about the new unit. */
export interface NewTeam {
  name: string;
  /** The unit it sits under, by key or name; the root when absent. */
  parent?: string;
  type?: string;
  purpose?: string;
  /** Its agents, each made idle. The first leads it unless `lead` says otherwise. */
  agents?: Omit<NewAgent, "unit">[];
  /** Its lead, by handle. */
  lead?: string;
}

/** One more unit, with its agents. */
export function addTeam(team: NewTeam): Step {
  return edit((c) => {
    const doc = structuredClone(documentOf(c));
    const unit: ConfigUnit = { name: team.name, type: team.type ?? "team", roles: [] };
    if (team.purpose) unit.purpose = team.purpose;
    const lead = team.lead ?? team.agents?.[0]?.handle;
    if (lead) unit.lead = lead;
    for (const agent of team.agents ?? []) {
      const role: ConfigRole = { name: agent.name, handle: agent.handle, llm: "default" };
      if (agent.goal) role.goal = agent.goal;
      if (agent.character || agent.color) {
        role.avatar = {
          ...(agent.character ? { character: agent.character } : {}),
          ...(agent.color ? { color: agent.color } : {}),
        };
      }
      unit.roles!.push(role);
    }
    if (team.parent) {
      const parent = findUnit(doc.units, team.parent);
      if (!parent) throw new Error(`addTeam: there is no unit "${team.parent}".`);
      (parent.children ??= []).push(unit);
    } else {
      (doc.units ??= []).push(unit);
    }
    replace(c, doc, `Added the ${team.name} unit`);
  });
}

/** A seat's avatar drawn another way. */
export function restyleAgent(
  handle: string,
  avatar: { character?: AvatarCharacter; color?: AvatarColor },
): Step {
  return edit((c) => {
    const doc = structuredClone(documentOf(c));
    const role = allRoles(doc).find((r) => handleOf(r) === handle);
    if (!role) throw new Error(`restyleAgent: there is no seat "${handle}".`);
    role.avatar = { ...(role.avatar ?? {}), ...avatar };
    replace(c, doc, `Restyled ${role.name}`);
  });
}

/** What `addPerson` needs to know about the new person. */
export interface NewPerson {
  name: string;
  handle: string;
  /** The unit they join, by key or name; the root when absent. */
  unit?: string;
  email?: string;
  /** When and how they answer, which the agents they work with are told. */
  availability?: string;
  /** The seats they manage, by handle, or whole units, by key. */
  manages?: string[];
  /**
   * The chat identities they are reached on (`slack_user_id`, `github_login`
   * and the rest); none by default, which the engine allows with an advisory
   * for a person who only uses the dashboard.
   */
  contact?: Record<string, string>;
}

/**
 * One more person, a human seat (`kind: "human"`), in a unit or at the root,
 * as a new revision. A person has no live state: the chart, the people lists
 * and every reporting line read them from the document.
 *
 *     addPerson({ name: "Sara", handle: "sara", unit: "product-and-engineering", manages: ["engineer"] })
 */
export function addPerson(person: NewPerson): Step {
  return edit((c) => {
    const doc = structuredClone(documentOf(c));
    const role: ConfigRole = { name: person.name, kind: "human", handle: person.handle };
    if (person.email) role.email = person.email;
    if (person.availability) role.availability = person.availability;
    if (person.manages) role.manages = person.manages;
    if (person.contact) role.contact = person.contact;
    if (person.unit) {
      const unit = findUnit(doc.units, person.unit);
      if (!unit) throw new Error(`addPerson: there is no unit "${person.unit}".`);
      (unit.roles ??= []).push(role);
    } else {
      (doc.roles ??= []).push(role);
    }
    replace(c, doc, `Added ${person.name}`);
  });
}

/** Where a schedule is declared: on a seat, by its handle, or on a unit, by its key or name. */
export type ScheduleOwner = { seat: string } | { unit: string };

/** The owner as a sentence names it. */
const ownerName = (owner: ScheduleOwner) =>
  "seat" in owner ? `seat "${owner.seat}"` : `unit "${owner.unit}"`;

/**
 * One more schedule, as a new revision, written into its owner's `schedules`
 * as the document holds them: `name`, `cron` and `task`, and `timezone`,
 * `target` (a unit's: `each`, the default, or `lead`), `enabled`,
 * `timeout_seconds` and `catchup` where it sets them. Refused, naming the
 * rule, where the engine refuses the write: a malformed cron or zone, a
 * second schedule of one name, a unit schedule nobody can run, or any
 * schedule on a person.
 *
 *     addSchedule({ unit: "Product and Engineering" }, { name: "weekly-review", cron: "0 16 * * 5", task: "Review the week's delivered work.", target: "lead" })
 */
export function addSchedule(owner: ScheduleOwner, spec: ScheduleSpec): Step {
  return edit((c) => {
    const doc = structuredClone(documentOf(c));
    const holder =
      "seat" in owner
        ? allRoles(doc).find((r) => handleOf(r) === owner.seat)
        : findUnit(doc.units, owner.unit);
    if (!holder) throw new Error(`addSchedule: there is no ${ownerName(owner)}.`);
    const written = Object.fromEntries(
      Object.entries(spec).filter(([, value]) => value !== undefined),
    ) as ScheduleSpec;
    (holder.schedules ??= []).push(written);
    replace(c, doc, `Added the ${spec.name} schedule`);
  });
}

/**
 * One fire of a schedule, recorded as the scheduler records it in the
 * dispatch ledger the schedules screens read (`c.scheduleRuns`, newest
 * first). The fire is the schedule's latest tick at or before `hoursAgo`
 * hours ago, labelled on the schedule's own clock, and the outcome is what
 * the ledger says of it:
 *
 * - `fired`: dispatched to every seat the schedule reaches (the seat for a
 *   seat's schedule; for a unit's, its lead or each of its agents), each
 *   under a trace of its own, with the `scheduled_task_fired` event it
 *   publishes.
 * - `skipped_paused`: claimed for each of those seats and not dispatched,
 *   because a person had the seat paused.
 * - `skipped_catchup`: the one fire a restart did not run late, for no seat,
 *   written when the engine came back just after the fire's catchup window.
 *
 * Refused where the engine could not have written it by now: a schedule
 * nobody declared, a fire still to come, one already recorded, or a catchup
 * skip of a schedule that does not catch up.
 *
 *     scheduleRan({ seat: "pm" }, "daily-standup", "fired", 26)
 */
export function scheduleRan(
  owner: ScheduleOwner,
  name: string,
  outcome: FireOutcome,
  hoursAgo: number,
): Step {
  return edit((c) => {
    if (!(hoursAgo >= 0)) {
      throw new Error("scheduleRan: hoursAgo is how long ago the fire was due, zero or more.");
    }
    const now = Date.parse(c.now);
    const doc = documentOf(c);
    // A SCHEDULE IS NAMED by its seat's handle or its unit's key (`scope_name`).
    const [scope, id] =
      "seat" in owner
        ? ["role", owner.seat]
        : ["unit", unitKeyOf(findUnit(doc.units, owner.unit) ?? { name: owner.unit })];
    const row = scheduleRows(doc, new Date(now)).find(
      (r) => r.scope_type === scope && r.scope_name === id && r.name === name,
    );
    if (!row) throw new Error(`scheduleRan: ${ownerName(owner)} declares no schedule "${name}".`);
    const { runs, events } = fireRecords(row, outcome, new Date(now - hoursAgo * 3_600_000));
    for (const run of runs) {
      if (Date.parse(run.fired_at) > now) {
        throw new Error(
          `scheduleRan: the ${run.fire_label} fire of "${name}" is written at ${run.fired_at}, which is still to come.`,
        );
      }
      const claimed = c.scheduleRuns.some(
        (r) =>
          r.scope_type === run.scope_type &&
          r.scope_id === run.scope_id &&
          r.schedule_name === run.schedule_name &&
          r.fire_label === run.fire_label &&
          r.target_handle === run.target_handle,
      );
      if (claimed) {
        throw new Error(
          `scheduleRan: the ${run.fire_label} fire of "${name}"${run.target_handle ? ` for ${run.target_handle}` : ""} is already recorded, and the ledger claims each fire once.`,
        );
      }
    }
    c.scheduleRuns = [...c.scheduleRuns, ...runs].sort(ledgerOrder);
    c.events = [...c.events, ...events].sort((a, b) => b.timestamp.localeCompare(a.timestamp));
  });
}

/** Every agent seat idle, never paused, with no turn on record. */
export const freshCompany: Step = edit((c) => {
  for (const seat of Object.values(c.seats)) seat.overlay = idleOverlay();
});

/**
 * The node's address (`api.external_url`, Tier A) set to `url`, or to none
 * with "", and the node restarted on it: every connect form's delivery
 * address and each agent's Slack manifest follow from it, and the reconcile
 * loop's first passes re-register what the engine registers itself.
 * FairSplit's node serves on `https://fairsplit.example.com` already; a node
 * with no address at all is one the engine's own suites build and no
 * configuration a node boots reaches, kept for a proposal about that state.
 */
export function withPublicAddress(url = "https://fairsplit.example.com"): Step {
  return edit((c) => {
    c.engine.external_url = url;
    afterRevision(c, c.now);
  });
}

/**
 * A tool connected, through the same path a person's connect form takes:
 * each submission is one form section (the company's, or one agent's own
 * with `seat`), its values as a person would type them.
 *
 *     connectIntegration("mattermost", [{ values: { url: "https://chat.example.com", team: "fairsplit", admin_token: "x" } }])
 */
export function connectIntegration(key: string, submissions: Submission[]): Step {
  return edit((c) => {
    for (const submission of submissions) {
      const reply = submit(c, key, submission, c.now);
      if (reply.status >= 400) {
        throw new Error(`connectIntegration(${key}): ${JSON.stringify(reply.body)}`);
      }
    }
  });
}

/**
 * What the tool answers when the reconcile loop runs a pass on one declared
 * SURFACE (`mattermost`, `jira`, ...): a phase (`degraded`, `awaiting_admin`,
 * `provisioning`, `activating`, `disconnecting`), who has to act, the detail,
 * the findings and any fault (`last_error`), recorded as the pass that heard
 * it. Every later pass hears the same until a person writes the surface's
 * inputs or disconnects it; the next pass then asks afresh and hears what the
 * demo derives (GitHub's report from the agents' own apps, see `seatApp`, and
 * ready on every other surface). The tool's card reads its worst surface.
 *
 *     integrationStatus("mattermost", { phase: "awaiting_admin", actor: "admin", detail: "Approve the bots." })
 */
export function integrationStatus(surface: string, status: Partial<DemoSurfaceState>): Step {
  return edit((c) => {
    if (!blockOf(documentOf(c), surface)) {
      throw new Error(
        `integrationStatus: the company declares no "${surface}" block; connect it first.`,
      );
    }
    reportFor(c, surface, status, c.now);
  });
}

/**
 * One agent's own GitHub app at a step of its two acts, as the engine records
 * it (`setupapi.recordSeatApp`): `create_app` (opted in, no app yet),
 * `install_app` (the app exists, sealed key and all, and nothing installed
 * it) or `installed`. The app's private key and webhook secret are sealed
 * under the names the engine gives them, so a disconnect names them.
 *
 * Unless a scenario scripts GitHub's answer (`integrationStatus`), its pass
 * reports from these steps as the engine's does: one finding naming every
 * agent with no app, one per agent whose app nothing installed, and ready
 * once every agent's app is installed. An agent at `create_app` carries an
 * app block, which the engine also reports on its own (see the README).
 *
 *     seatApp("github", "pm", "installed")
 */
export function seatApp(
  key: "github",
  handle: string,
  step: "create_app" | "install_app" | "installed",
): Step {
  return edit((c) => {
    if (key !== "github")
      throw new Error(`seatApp: only GitHub gives each agent an app of its own.`);
    const doc = structuredClone(documentOf(c));
    const seat = agentRoles(doc).find((r) => r.handle === handle);
    if (!seat) throw new Error(`seatApp: there is no agent seat "${handle}".`);
    const role = seat.role as ConfigRole & { integrations?: Record<string, unknown> };
    const integrations = (role.integrations ??= {});
    const prior = (integrations.github as Record<string, unknown> | undefined) ?? {};
    // The tier and the repositories are the operator's; the rest is the app's.
    const operator: Record<string, unknown> = {};
    if (prior.tier) operator.tier = prior.tier;
    if (prior.repos) operator.repos = prior.repos;
    if (step === "create_app") {
      integrations.github = operator;
    } else {
      const keyVar = `GITHUB_APP_KEY_${varSuffix(handle)}`;
      const hookVar = `GITHUB_APP_WEBHOOK_SECRET_${varSuffix(handle)}`;
      const number = (salt: string) =>
        100_000 +
        (parseInt(fixtureId(`${salt}-${handle}`).replace(/-/g, "").slice(0, 8), 16) % 900_000);
      integrations.github = {
        ...operator,
        app_id: number("github-app"),
        app_slug: `${slugify(doc.name ?? "crewlet")}-${handle}`,
        private_key: `\${${keyVar}}`,
        webhook_secret: `\${${hookVar}}`,
        installation_id: step === "installed" ? number("github-installation") : 0,
      };
      for (const name of [keyVar, hookVar]) seal(c, name, c.now, actorOf(c), "setup");
    }
    replace(c, doc, `GitHub app for ${role.name}: ${step.replace(/_/g, " ")}`);
  });
}

/**
 * One agent's own account on a tool whose agent accounts the engine creates
 * (`datadog`, `atlassian`, `gitlab` or `mattermost`), as the tool's
 * provisioning pass leaves it: the agent opted in by a `${VAR}` named at its
 * credential address (the one it already names, or one named as the
 * engine's documentation names them), and what the pass minted sealed under
 * that name by the reconcile loop. The tool's form then lists the agent as
 * ready, under the account the engine names it by: its Datadog address, its
 * GitLab user, its Mattermost bot, or the Atlassian account address the pass
 * recorded (`email`, by default one in Atlassian's service-account shape).
 * Refused where the pass would not provision the agent: the tool is not
 * connected, the handle is no agent's, or its credential is a value.
 *
 *     seatProvisioned("datadog", "pm")
 */
export function seatProvisioned(
  kind: ProvisionedKind,
  handle: string,
  options: { email?: string } = {},
): Step {
  return edit((c) => {
    let planned: ReturnType<typeof provisioning>;
    try {
      planned = provisioning(c, kind, handle, options);
    } catch (err) {
      throw new Error(`seatProvisioned: ${(err as Error).message}`);
    }
    if (planned.doc)
      replace(c, planned.doc, `Named a ${APP_NAMES[kind]} credential variable for ${handle}`);
    for (const { name, shown } of planned.sealed) {
      seal(c, name, c.now, RECONCILE_LOOP, PROVISION_SOURCE);
      if (shown !== undefined) c.recorded[name] = shown;
    }
  });
}

/** One delivery a tool made to this engine, for `integrationTraffic`. */
export interface Delivery {
  /** The tool's own event name, such as `push` or `app_mention`: the row is filed as `webhook:<type>`. */
  type: string;
  summary: string;
  /** The agent's handle a per-agent delivery was addressed to. */
  recipient?: string;
  minutesAgo: number;
  /** The tool's own id for the delivery, which its console shows. */
  delivery_key?: string;
}

/**
 * Deliveries one surface made, as the event log files them (category
 * `webhook`, type `webhook:<event>`, tagged with the route and the
 * recipient), and the notification outcomes over the window. The room's
 * inbound count is COUNTED from those rows, as the engine counts it, so an
 * `inbound` beyond the deliveries listed repeats them further back in the
 * thirty-day window.
 *
 *     integrationTraffic("github", { inbound: 40, skipped: 2, deliveries: [{ type: "push", summary: "Pushed 3 commits to main", minutesAgo: 12 }] })
 */
export function integrationTraffic(
  surface: string,
  traffic: { inbound?: number; skipped?: number; coalesced?: number; deliveries?: Delivery[] },
): Step {
  return edit((c) => {
    const given = traffic.deliveries ?? [];
    const inbound = traffic.inbound ?? given.length;
    if (inbound < given.length) {
      throw new Error(
        "integrationTraffic: inbound counts the deliveries, so it cannot be fewer than those listed.",
      );
    }
    if (inbound > given.length && given.length === 0) {
      throw new Error(
        "integrationTraffic: name at least one delivery for the rest of the count to repeat.",
      );
    }
    const now = Date.parse(c.now);
    const window = 30 * 24 * 60;
    const oldest = Math.max(0, ...given.map((d) => d.minutesAgo));
    const more = inbound - given.length;
    const spacing = more > 0 ? Math.max(1, Math.floor((window - 60 - oldest) / more)) : 0;
    const all = [
      ...given,
      ...Array.from({ length: more }, (_, i) => {
        const d = given[i % given.length]!;
        return { ...d, minutesAgo: oldest + (i + 1) * spacing, delivery_key: undefined };
      }),
    ];
    const rows: EventRecord[] = all.map((d, i) => {
      const label = /^(webhook|forge|socket):/.test(d.type) ? d.type : `webhook:${d.type}`;
      const key = d.delivery_key ?? fixtureId(`delivery-key-${surface}-${i}`);
      const tags: Record<string, string> = { route: surface, delivery_key: key };
      if (d.recipient) tags.recipient = d.recipient;
      return {
        id: fixtureId(`delivery-${surface}-${i}-${d.minutesAgo}`),
        type: label,
        timestamp: new Date(now - d.minutesAgo * 60_000).toISOString(),
        source: surface,
        actor: surface,
        summary: d.summary,
        category: "webhook",
        trace_id: fixtureId(`delivery-trace-${surface}-${i}`),
        span_id: "",
        parent_span_id: "",
        topic: "inbound_delivery",
        tags,
        payload: {
          label,
          route: surface,
          summary: d.summary,
          ...(d.recipient ? { recipient: d.recipient } : {}),
          delivery_key: key,
          body: {},
        },
      };
    });
    c.events = [...c.events, ...rows].sort((a, b) => b.timestamp.localeCompare(a.timestamp));
    c.integrations.outcomes[surface] = {
      skipped: traffic.skipped ?? 0,
      coalesced: traffic.coalesced ?? 0,
    };
  });
}

/**
 * One seat stopped for a reason of the engine's own: `unplaced` (no node holds
 * it), `provider` (its models are refusing), `budget` (a window is spent; see
 * also `seatOutOfBudget`) or `paused`.
 */
export function seatStopped(handle: string, reason: StoppedReason): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`seatStopped: there is no agent seat "${handle}".`);
    seat.overlay = {
      ...seat.overlay,
      activity: "stopped",
      stopped_reason: reason,
      turn: null,
      live_call: null,
      ...(reason === "paused" ? {} : { paused: null }),
    };
  });
}

/** One seat whose last turn ended in an error the engine reports on its row. */
export function seatFailed(handle: string, message: string, kind = "provider"): Step {
  return edit((c) => {
    const seat = c.seats[handle];
    if (!seat) throw new Error(`seatFailed: there is no agent seat "${handle}".`);
    const at = new Date(Date.parse(c.now) - 3 * 60_000).toISOString();
    const turnId = fixtureId(`${handle}-failed-turn`);
    seat.overlay = {
      ...seat.overlay,
      activity: "stopped",
      stopped_reason: "provider",
      paused: null,
      turn: null,
      last_turn: { turn_id: turnId, ended_at: at, outcome: "failed" },
      last_error: {
        kind,
        message,
        phase: "execute",
        turn_id: turnId,
        at,
        event_id: fixtureId(`${handle}-failed-event`),
      },
    };
  });
}

// ---- helpers ------------------------------------------------------------------

/** A unit by its key or, for a helper's convenience, its name. */
function findUnit(units: ConfigUnit[] | undefined, name: string): ConfigUnit | null {
  for (const unit of units ?? []) {
    if (unitKeyOf(unit) === name || unit.name === name) return unit;
    const found = findUnit(unit.children, name);
    if (found) return found;
  }
  return null;
}

function allRoles(doc: ReturnType<typeof documentOf>): ConfigRole[] {
  const out = [...(doc.roles ?? [])];
  const walk = (units: ConfigUnit[] | undefined) => {
    for (const unit of units ?? []) {
      out.push(...(unit.roles ?? []));
      walk(unit.children);
    }
  };
  walk(doc.units);
  return out;
}

/**
 * `doc` as a new active revision, as if a person had saved it from the
 * builder: the same path every write takes, so the seats follow the roster.
 * Refused, naming every rule it breaks, where the engine would refuse the
 * write. The id is stable, so a scenario's revision has the same address on
 * every load.
 */
function replace(c: DemoCompany, doc: ReturnType<typeof documentOf>, summary: string): void {
  const { problems } = validate(doc);
  if (problems.length > 0) {
    throw new Error(
      `${summary}: the engine refuses this document: ${problems.map((p) => p.message).join("; ")}`,
    );
  }
  const n = c.config.revisions.length + 1;
  appendRevision(c, doc, summary, {
    at: c.now,
    source: "api",
    id: fixtureId(`scenario-revision-${n}-${summary}`),
  });
}
