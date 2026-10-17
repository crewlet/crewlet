/**
 * Whether this browser may make one change, and if not, the one sentence that
 * says why.
 *
 * A WRITE CONTROL IS NEVER HIDDEN. A button that appears only for some readers
 * teaches everybody else that the product cannot do the thing — the founder
 * reading on a phone who has not signed in, the colleague the engine does
 * not offer that change to — and says nothing about what would change that.
 * So every control asks here, is drawn for everybody, and is DISABLED with
 * the reason when it cannot act: the sentence is the instruction.
 *
 * SEVEN REASONS, in the order a person has to clear them:
 *
 *  - REFUSED — the engine knows who this browser is and will not serve it the
 *    company: a person whose seat was taken out of the chart
 *    (`store.accessRefused`). Before OFFLINE, because a socket that is not
 *    open for that reason is not one a reconnect brings back, and "reconnect
 *    to make changes" sent a person — online, and refused — looking for a
 *    network fault;
 *  - OFFLINE — the socket is down. A change is never queued for later: a
 *    queued write is one the person has walked away from believing it
 *    happened, and the company may have moved under it by the time it goes;
 *  - LOADING — nobody has said who this browser is yet;
 *  - ANONYMOUS — nobody is signed in, so there is nobody to record the
 *    change under;
 *  - NO ACCESS — the change is an app's and the reader does not hold that
 *    app: "This change needs Worklet access. An admin can give it to you."
 *    Decided only for a change `viewer.acts` leaves out, and only where the
 *    reader holds none of the apps the change needs ([actionApps]) — so a
 *    change the engine DOES offer without the app (answering an ask you were
 *    asked) is never held by it;
 *  - NOT SERVED — the engine does not make this change for this person at
 *    all: a company whose tracker is Jira has no Worklet to write to, and a caller
 *    the authority table admits to it under no object at all is not offered
 *    it. `viewer.acts` is the engine's own list, never a guess from the
 *    tool's name or from the access;
 *  - HELD — the screen is showing somebody ELSE's record, and says so
 *    ([HoldWrites]). My work read on a report's day is the case it exists
 *    for: the questions there are asked of the report, the queue is theirs,
 *    and a control that looked pressable would invite a change the reader
 *    came to look at rather than to make. Last, because every reason above
 *    it is one the person can clear on this screen and this one is a fact
 *    about what the screen is showing — and the SENTENCE is the screen's,
 *    since only the screen knows whose record it is.
 *
 * AN UNBOUND CALLER IS NOT A REASON. Somebody the identity directory binds to
 * no seat — an operator outside the chart, a pipeline's token — acts under
 * their own login, and the engine records the change as theirs (ADR-0024);
 * what a binding would add is the seat they act AS, which `as` names. A gate
 * that held them would lock a person out of a change the engine makes for
 * them, with a remedy that is not theirs to take.
 */

import { createContext, createElement, useContext, type ReactNode } from "react";
import { useLeadScope, type LeadScope } from "./leadScope.ts";
import { isOwnSeat } from "./seats.ts";
import { useConnection } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import { useServicesRun, type Service, type ServicesRun } from "./services.ts";
import { ADMIN, holds, needsWords, type App } from "./access.ts";
import type { ActionTool } from "~/protocol/act.ts";
import { ACTIONS } from "~/contract/actions.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";

/** Why a control cannot act, as a value a test can name. */
export type WriteBlock =
  "refused" | "offline" | "loading" | "anonymous" | "no_access" | "not_served" | "held";

/**
 * What the socket says about this browser: open, down — which a reconnect
 * repairs — or refused by an engine that knows who it is, which none does.
 */
export type Link = "open" | "offline" | "refused";

/** The [Link] a connection posture (`useConnection`) is. */
export function linkOf(connection: { connected: boolean; accessRefused: string | null }): Link {
  if (connection.accessRefused !== null) return "refused";
  return connection.connected ? "open" : "offline";
}

export type WriteAccess =
  | {
      can: true;
      /**
       * The name the change will be recorded under — the seat a bound person
       * acts as, and the login of anybody bound to none (`viewer.owner`, which
       * is the engine's `iam.RecordOwner`).
       */
      as: string;
      /** Every change the engine makes for this person. */
      acts: readonly string[];
    }
  | { can: false; block: WriteBlock; reason: string };

/**
 * The sentence each block is shown as, second person, with its remedy — every
 * block but [HoldWrites]', whose sentence the holding screen writes, because it
 * names whose record is on screen, and NO ACCESS, whose sentence names the app
 * ([noAccessReason]).
 */
export const WRITE_REASONS: Readonly<Record<Exclude<WriteBlock, "held" | "no_access">, string>> = {
  refused: "Can’t make changes right now.",
  offline: "Offline. Reconnect to make changes.",
  loading: "Checking who you are before anything can be changed.",
  anonymous: "Sign in to make changes.",
  not_served: "This can’t be changed from here.",
};

// EVERY ROW'S APPS ARE APPS, checked by the compiler: an app spelled wrong in
// the table would be a lock that names access nobody can be given.
const NEEDS: { readonly [T in ActionTool]: { readonly apps: readonly App[] } } = ACTIONS;

/**
 * The apps ANY ONE of which a change needs — its app for an app's change,
 * every app for one that draws on whichever the caller holds
 * (`answer_knowledge`), none for a change that is no app's (pausing a seat,
 * answering a coding run).
 *
 * THE ROW'S OWN `apps` (`contract/actions.ts`), which an engine gate holds to
 * the authority table's row for the tool. It used to be read off the row's
 * `domain`, the log the write lands in — a different fact, which agreed only
 * while every app's change landed in that app's log: `answer_knowledge` lands
 * in none and needs an app, so a reader holding neither was told the engine
 * does not make the change rather than what to ask an admin for.
 */
export function actionApps(tool: ActionTool): readonly App[] {
  return NEEDS[tool].apps;
}

/**
 * The service built into Crewlet a change lands in, read off the same
 * `domain`: Worklet for the `tracker` log, Doclet for the `pages` log. A
 * company running another system in its place, or none, has nowhere for the
 * change to land.
 */
export function actionService(tool: ActionTool): Service | null {
  const domain: string | null = ACTIONS[tool].domain;
  if (domain === "tracker") return "tracker";
  if (domain === "pages") return "knowledge";
  return null;
}

/**
 * The sentence a change is held with where the company does not run the
 * service it lands in. The control that would START such a change (New task,
 * Message, Assign, Ask an agent) is not drawn there at all; this is for what
 * stays drawn, such as an action folded into a phone's More menu.
 */
export const SERVICE_OFF_REASONS: Readonly<Record<Service, string>> = {
  tracker: "Worklet is not in use, so no task can be filed or changed here.",
  knowledge: "Doclet is not in use, so no page can be written here.",
};

/** The sentence a change is held with when the reader holds none of its apps. */
export function noAccessReason(apps: readonly App[]): string {
  return needsWords("This change", apps);
}

/** The decision itself, over values — what the hook reads, and what a test pins. */
export function writeAccess(
  tool: ActionTool,
  viewer: ViewerState,
  link: Link,
  /** Which of Crewlet's own services the company runs (`lib/services.ts`). */
  runs: ServicesRun,
  /** The hold the nearest [HoldWrites] placed, or null where none is. */
  held: string | null = null,
): WriteAccess {
  const blocked = (block: Exclude<WriteBlock, "held" | "no_access">): WriteAccess => ({
    can: false,
    block,
    reason: WRITE_REASONS[block],
  });
  if (link !== "open") return blocked(link);
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  // A SERVICE THE COMPANY DOES NOT RUN FIRST: no access the reader could be
  // given would give the change anywhere to land.
  const service = actionService(tool);
  if (service !== null && !runs[service]) {
    return { can: false, block: "held", reason: SERVICE_OFF_REASONS[service] };
  }
  if (!viewer.acts.includes(tool)) {
    // THE APP FIRST, where the reader lacks it: "needs Worklet access" is a
    // remedy they can ask an admin for, and "this engine does not make this
    // change for you" about a change it would make once they held it sends
    // them nowhere.
    const apps = actionApps(tool);
    if (apps.length > 0 && !apps.some((app) => holds(viewer.grants, app))) {
      return { can: false, block: "no_access", reason: noAccessReason(apps) };
    }
    return blocked("not_served");
  }
  if (held) return { can: false, block: "held", reason: held };
  return { can: true, as: viewer.owner || viewer.login, acts: viewer.acts };
}

const Held = createContext<string | null>(null);

/**
 * Hold every change made under it, with the sentence that says why.
 *
 * A SCREEN SHOWING SOMEBODY ELSE'S RECORD wraps what it draws in one, and
 * every control inside — a button, a picker, a drag — is DISABLED WITH THIS
 * SENTENCE through the same [useWriteAccess] it already asks, rather than each
 * control being told separately and one of them forgetting. The controls stay
 * drawn: a write control is never hidden, and a reader who sees the Answer
 * buttons disabled with "these are asked of Rui" has learned something a
 * missing button would not have told them.
 *
 * THE NEAREST ONE WINS, so `reason={null}` inside a hold RELEASES it for what
 * it wraps — the one change a screen offers on somebody else's record (a
 * lead's reorder of a report's queue) is released exactly where it is drawn,
 * and nothing else is.
 */
export function HoldWrites({ reason, children }: { reason: string | null; children: ReactNode }) {
  return createElement(Held.Provider, { value: reason }, children);
}

/** Whether this browser may make `tool`'s change now, and why not. */
export function useWriteAccess(tool: ActionTool): WriteAccess {
  const viewer = useViewer();
  const link = linkOf(useConnection());
  const held = useContext(Held);
  return writeAccess(tool, viewer, link, useServicesRun(), held);
}

/**
 * A write folded into a menu, held as its inline control is: disabled, with
 * the sentence that says why. One helper because a page's inline control and
 * its phone "More" entry for the same write must refuse in the same words.
 */
export function menuHold(access: WriteAccess): { disabled?: boolean; description?: string } {
  return access.can ? {} : { disabled: true, description: access.reason };
}

// ---------------------------------------------------------------------------
// Messaging a seat
// ---------------------------------------------------------------------------

/** The sentence Message is held with on the reader's own seat. */
export const OWN_SEAT_MESSAGE = "This is your own seat.";

/**
 * Whether this browser may MESSAGE the seat `handle` — file an ask of it — and
 * why not: the act's own access, HELD on the reader's own seat.
 *
 * A message files an ask the seat owes an answer on, and on your own seat that
 * is a question only you could answer. THE RULE IS THE CONTROL'S, never each
 * screen's: the seat page wrapped its Message in a hold while the seat peek
 * drew the same button bare, so a person opening their own seat from the
 * chart was offered an enabled Message to themselves one press from the page
 * that refused it. So Message holds itself (`MessageSeatButton`), and a
 * phone's folded entry for it asks here too and is held in the same words.
 *
 * LAST, as every hold is ([writeAccess]): a reader who cannot act at all is
 * told what they can clear first, and only one who could file is told this.
 */
export function messageSeatAccess(
  access: WriteAccess,
  viewer: Pick<ViewerState, "handle">,
  handle: string,
): WriteAccess {
  return access.can && isOwnSeat(viewer, handle)
    ? { can: false, block: "held", reason: OWN_SEAT_MESSAGE }
    : access;
}

/** [messageSeatAccess] for this browser now. */
export function useMessageSeatAccess(handle: string): WriteAccess {
  return messageSeatAccess(useWriteAccess("create_work_item"), useViewer(), handle);
}

// ---------------------------------------------------------------------------
// A change to the company's configuration
// ---------------------------------------------------------------------------

/**
 * Why this browser cannot change the company's CONFIGURATION — a ceiling, a
 * seat, a server — as opposed to acting in it.
 *
 * A DIFFERENT GATE FROM AN ACT, and the difference is the engine's: a
 * `/config` write is decided by ADMIN, not by the act catalogue and not by a
 * seat binding — so a credential bound to no seat that holds it may change a
 * ceiling, and a bound person without it may not. `viewer.grants` is the
 * engine's own answer to exactly that question,
 * read here rather than guessed per screen. The step-up such a write may
 * ask for is the transport's to replay (`protocol/rest.ts`), not a block: a
 * person asked to confirm who they are can, on the spot.
 *
 * AND A DOCUMENT CAN BE MANAGED (ADR-0030): then only the credentials the
 * deployment names as its writers may change it, whatever grant or subtree
 * anybody else holds — which `viewer.configWriter` answers ([managedBlock]).
 * [mayChangeConfig] is the two together, for a screen that has to know
 * before anything is pressed.
 */
export type ConfigWriteBlock =
  | "refused"
  | "offline"
  | "loading"
  | "anonymous"
  | "no_grant"
  | "outside_scope"
  | "managed"
  | "held";

export type ConfigWriteAccess =
  { can: true } | { can: false; block: ConfigWriteBlock; reason: string };

/** The access a configuration write is decided by (`iam.GrantAdmin`). */
export const CONFIG_WRITE_GRANT = ADMIN;

/**
 * The sentence each block is shown as — every block but a [HoldWrites]', a
 * lead's `outside_scope` ([outsideScopeReason]) and `managed`, whose sentence
 * names who manages the document ([managedSentence]).
 */
export const CONFIG_WRITE_REASONS: Readonly<
  Record<Exclude<ConfigWriteBlock, "held" | "outside_scope" | "managed">, string>
> = {
  refused: WRITE_REASONS.refused,
  offline: WRITE_REASONS.offline,
  loading: WRITE_REASONS.loading,
  anonymous: "Sign in to change the configuration.",
  no_grant: needsWords("Changing the configuration", [ADMIN]),
};

/**
 * What a `/config` write the engine refused on AUTHORITY says, in one
 * sentence — the one reading every writer of the company document shows.
 *
 * BY THE REFUSAL'S CODE. Most are the access this module gates on, and say so
 * in the gate's own words. But a step-up the person declined and a write sent
 * from another site are refused 403 too, and "you do not hold Admin" about
 * either sends somebody to an admin for nothing — so every
 * other code the request guard writes is said in the act surface's words
 * for it (`ACT_ERRORS`), which are the same refusals. A 401 has already sent
 * the reader to sign in (`protocol/rest.ts`); its sentence says the same.
 */
export function configGuardedReason(code: string): string {
  const own = Object.hasOwn(ACT_ERRORS, code) ? ACT_ERRORS[code as keyof typeof ACT_ERRORS] : null;
  return code === "unauthorized" || own === null ? CONFIG_WRITE_REASONS.no_grant : own;
}

/**
 * Why THIS caller may not change a managed company document, or null — when
 * the document is managed by nobody, or by this caller's own credential.
 *
 * The engine's own answer decides (`viewer.config_writer`), and only a
 * document somebody manages is managed: a reader the engine named no writers
 * to (an anonymous one) is not told the document is managed here, because
 * the gate in front of this one already says what they lack.
 */
export function managedBlock(viewer: ViewerState): string | null {
  if (viewer.configManagedBy.length === 0 || viewer.configWriter) return null;
  return managedSentence(viewer.configManagedBy);
}

/**
 * [managedBlock] for the screen this browser is on: the sentence a managed
 * document's editing controls are disabled with, or null.
 */
export function useManagedConfig(): string | null {
  return managedBlock(useViewer());
}

/**
 * Whether this reader may change the company's configuration at all — Admin,
 * and on a managed document a credential it names as a writer.
 *
 * THE STANDING ANSWER, apart from this moment's connection or a screen's
 * hold: what a screen asks when it decides whether a change is the reader's
 * to make — a stopped seat counted as THEIR decision, "Set one" offered as a
 * link — rather than whether a control may be pressed now
 * ([configWriteAccess]). Both read the same two answers, so they cannot
 * disagree about who may write.
 */
export function mayChangeConfig(viewer: ViewerState): boolean {
  return holds(viewer.grants, CONFIG_WRITE_GRANT) && managedBlock(viewer) === null;
}

/** The decision over values — what the hook reads, and what a test pins. */
export function configWriteAccess(
  viewer: ViewerState,
  link: Link,
  held: string | null = null,
): ConfigWriteAccess {
  const blocked = (
    block: Exclude<ConfigWriteBlock, "held" | "outside_scope" | "managed">,
  ): ConfigWriteAccess => ({
    can: false,
    block,
    reason: CONFIG_WRITE_REASONS[block],
  });
  if (link !== "open") return blocked(link);
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  if (!holds(viewer.grants, CONFIG_WRITE_GRANT)) return blocked("no_grant");
  // AFTER ADMIN, because a reader who could not write anyway is told the
  // thing they can clear; and BEFORE A HOLD, because no screen can release
  // it. The engine's own answer decides — `config_writer` — and only a
  // document somebody manages is managed.
  const managed = managedBlock(viewer);
  if (managed !== null) return { can: false, block: "managed", reason: managed };
  if (held) return { can: false, block: "held", reason: held };
  return { can: true };
}

/** Whether this browser may change the company's configuration now, and why not. */
export function useConfigWriteAccess(): ConfigWriteAccess {
  const viewer = useViewer();
  const link = linkOf(useConnection());
  const held = useContext(Held);
  return configWriteAccess(viewer, link, held);
}

// ---------------------------------------------------------------------------
// A change to one part of the org chart
// ---------------------------------------------------------------------------

/**
 * What a change to the org chart is about: one seat (by handle), one unit (by
 * key), ANYWHERE a lead could make one — the builder itself, or adding a seat
 * to the unit they lead — or the COMPANY itself (its own ceilings, its
 * settings), which is never a lead's.
 */
export type OrgTarget =
  { readonly seat: string } | { readonly unit: string } | "anywhere" | "company";

/**
 * The sentence a lead reads where a change reaches outside what they lead:
 * which units those are, and the access that reaches the rest.
 */
export function outsideScopeReason(scope: LeadScope): string {
  const names = scope.tops.map((t) => t.name);
  const led =
    names.length <= 1 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
  return `You lead ${led}, and this is outside ${names.length === 1 ? "it" : "them"}: changing it takes Admin access.`;
}

/**
 * Whether this browser may change one part of the org chart, and why not.
 *
 * ADMIN, OR A LEAD INSIDE THEIR OWN SUBTREE — the engine's rule
 * (`lib/leadScope.ts`): without Admin, a person may still change the
 * seats and units inside a unit they lead, through the per-seat and per-unit
 * writes (`/config/roles/{handle}`, `/config/units/{key}`), and the engine
 * refuses whatever such a write reaches outside it. A control about a seat
 * or a unit OUTSIDE what they lead says so in its own words rather than as a
 * missing access, since the person may well know they lead something. And on a
 * managed document (ADR-0030) a lead's write is refused like everybody's but
 * its writers', so the lead path ends at [managedBlock] too.
 */
export function orgWriteAccess(
  viewer: ViewerState,
  link: Link,
  scope: LeadScope,
  target: OrgTarget,
  held: string | null = null,
): ConfigWriteAccess {
  const company = configWriteAccess(viewer, link, held);
  if (company.can || company.block !== "no_grant" || scope.tops.length === 0) return company;
  const inside =
    target === "anywhere" ||
    (target !== "company" &&
      ("seat" in target ? scope.seats.has(target.seat) : scope.units.has(target.unit)));
  if (!inside) return { can: false, block: "outside_scope", reason: outsideScopeReason(scope) };
  // A LEAD IS NO WRITER OF A MANAGED DOCUMENT either: the engine refuses
  // every credential its writers list does not name, a lead's subtree write
  // included (ADR-0030). After the scope, for Admin's reason above.
  const managed = managedBlock(viewer);
  if (managed !== null) return { can: false, block: "managed", reason: managed };
  if (held) return { can: false, block: "held", reason: held };
  return { can: true };
}

/** [orgWriteAccess] for the person reading, now. */
export function useOrgWriteAccess(target: OrgTarget): ConfigWriteAccess {
  return useOrgWriteAccessFor()(target);
}

/**
 * [orgWriteAccess] for the person reading, now, for a target named later: a
 * gate that learns what it is about from a payload (an assistant's prepared
 * ceiling) rather than from the component it is drawn in.
 */
export function useOrgWriteAccessFor(): (target: OrgTarget) => ConfigWriteAccess {
  const viewer = useViewer();
  const link = linkOf(useConnection());
  const held = useContext(Held);
  const scope = useLeadScope();
  return (target) => orgWriteAccess(viewer, link, scope, target, held);
}
