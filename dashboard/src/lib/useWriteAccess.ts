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
 * SIX REASONS, in the order a person has to clear them:
 *
 *  - REFUSED — the engine knows who this browser is and will not serve it the
 *    company: a session without `state:read`, or a person whose seat was taken
 *    out of the chart (`store.accessRefused`). Before OFFLINE, because a
 *    socket that is not open for that reason is not one a reconnect brings
 *    back, and "reconnect to make changes" sent a person invited with no
 *    grants — online, and refused — looking for a network fault;
 *  - OFFLINE — the socket is down. A change is never queued for later: a
 *    queued write is one the person has walked away from believing it
 *    happened, and the company may have moved under it by the time it goes;
 *  - LOADING — nobody has said who this browser is yet;
 *  - ANONYMOUS — nobody is signed in, so there is nobody to record the
 *    change under;
 *  - NOT SERVED — the engine does not make this change for this person at
 *    all: a company whose tracker is Jira has no native writer, and a caller
 *    the authority table admits to it under no object at all is not offered
 *    it. `viewer.acts` is the engine's own list, never a guess from the
 *    tool's name or from the grants;
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
import { useConnection } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import type { ActionTool } from "~/protocol/act.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";

/** Why a control cannot act, as a value a test can name. */
export type WriteBlock = "refused" | "offline" | "loading" | "anonymous" | "not_served" | "held";

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
 * names whose record is on screen.
 */
export const WRITE_REASONS: Readonly<Record<Exclude<WriteBlock, "held">, string>> = {
  refused:
    "The engine is not serving you the company, so nothing in it can be changed here — your Account says what you hold.",
  offline: "Offline — reconnect to make changes. Nothing is queued while you are away.",
  loading: "Checking who you are before anything can be changed.",
  anonymous: "Sign in to make changes — every change is recorded under your name.",
  not_served: "This engine does not make this change for you.",
};

/** The decision itself, over values — what the hook reads, and what a test pins. */
export function writeAccess(
  tool: ActionTool,
  viewer: ViewerState,
  link: Link,
  /** The hold the nearest [HoldWrites] placed, or null where none is. */
  held: string | null = null,
): WriteAccess {
  const blocked = (block: Exclude<WriteBlock, "held">): WriteAccess => ({
    can: false,
    block,
    reason: WRITE_REASONS[block],
  });
  if (link !== "open") return blocked(link);
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  if (!viewer.acts.includes(tool)) return blocked("not_served");
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
  return writeAccess(tool, viewer, link, held);
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
// A change to the company's configuration
// ---------------------------------------------------------------------------

/**
 * Why this browser cannot change the company's CONFIGURATION — a ceiling, a
 * seat, a server — as opposed to acting in it.
 *
 * A DIFFERENT GATE FROM AN ACT, and the difference is the engine's: a
 * `/config` write is decided by the `config:write` GRANT, not by the act
 * catalogue and not by a seat binding — so a person bound to no
 * seat who holds it may change a ceiling, and a bound person without it may
 * not. `viewer.grants` is the engine's own answer to exactly that question,
 * read here rather than guessed per screen. The step-up such a write may
 * ask for is the transport's to replay (`protocol/rest.ts`), not a block: a
 * person asked to confirm who they are can, on the spot.
 */
export type ConfigWriteBlock =
  "refused" | "offline" | "loading" | "anonymous" | "no_grant" | "outside_scope" | "held";

export type ConfigWriteAccess =
  { can: true } | { can: false; block: ConfigWriteBlock; reason: string };

/** The grant a configuration write is decided by (`iam.GrantConfigWrite`). */
export const CONFIG_WRITE_GRANT = "config:write";

/** The sentence each block is shown as — every block but a [HoldWrites]'. */
export const CONFIG_WRITE_REASONS: Readonly<
  Record<Exclude<ConfigWriteBlock, "held" | "outside_scope">, string>
> = {
  refused: WRITE_REASONS.refused,
  offline: WRITE_REASONS.offline,
  loading: WRITE_REASONS.loading,
  anonymous: "Sign in to change the company's configuration.",
  no_grant:
    "Changing the company's configuration takes the config:write grant, which you do not hold — an administrator who holds people:manage can give it to you.",
};

/**
 * What a `/config` write the engine refused on AUTHORITY says, in one
 * sentence — the one reading every writer of the company document shows.
 *
 * BY THE REFUSAL'S CODE. Most are the grant this module gates on, and say so
 * in the gate's own words. But a step-up the person declined and a write sent
 * from another site are refused 403 too, and "you do not hold config:write"
 * about either sends somebody to an administrator for nothing — so every
 * other code the request guard writes is said in the act surface's words
 * for it (`ACT_ERRORS`), which are the same refusals. A 401 has already sent
 * the reader to sign in (`protocol/rest.ts`); its sentence says the same.
 */
export function configGuardedReason(code: string): string {
  const own = Object.hasOwn(ACT_ERRORS, code) ? ACT_ERRORS[code as keyof typeof ACT_ERRORS] : null;
  return code === "unauthorized" || own === null ? CONFIG_WRITE_REASONS.no_grant : own;
}

/** The decision over values — what the hook reads, and what a test pins. */
export function configWriteAccess(
  viewer: ViewerState,
  link: Link,
  held: string | null = null,
): ConfigWriteAccess {
  const blocked = (
    block: Exclude<ConfigWriteBlock, "held" | "outside_scope">,
  ): ConfigWriteAccess => ({
    can: false,
    block,
    reason: CONFIG_WRITE_REASONS[block],
  });
  if (link !== "open") return blocked(link);
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  if (!viewer.grants.includes(CONFIG_WRITE_GRANT)) return blocked("no_grant");
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
 * which units those are, and the grant that reaches the rest.
 */
export function outsideScopeReason(scope: LeadScope): string {
  const names = scope.tops.map((t) => t.name);
  const led =
    names.length <= 1 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
  return `You lead ${led}, and this is outside ${names.length === 1 ? "it" : "them"}: changing it takes the config:write grant.`;
}

/**
 * Whether this browser may change one part of the org chart, and why not.
 *
 * THE COMPANY'S GRANT, OR A LEAD INSIDE THEIR OWN SUBTREE — the engine's rule
 * (`lib/leadScope.ts`): without `config:write`, a person may still change the
 * seats and units inside a unit they lead, through the per-seat and per-unit
 * writes (`/config/roles/{handle}`, `/config/units/{key}`), and the engine
 * refuses whatever such a write reaches outside it. A control about a seat
 * or a unit OUTSIDE what they lead says so in its own words rather than as a
 * missing grant, since the person may well know they lead something.
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
  if (held) return { can: false, block: "held", reason: held };
  return { can: true };
}

/** [orgWriteAccess] for the person reading, now. */
export function useOrgWriteAccess(target: OrgTarget): ConfigWriteAccess {
  const viewer = useViewer();
  const link = linkOf(useConnection());
  const held = useContext(Held);
  const scope = useLeadScope();
  return orgWriteAccess(viewer, link, scope, target, held);
}
