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
 * FIVE REASONS, in the order a person has to clear them:
 *
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
import { useConnection } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import type { ActionTool } from "~/protocol/act.ts";

/** Why a control cannot act, as a value a test can name. */
export type WriteBlock = "offline" | "loading" | "anonymous" | "not_served" | "held";

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
  offline: "Offline — reconnect to make changes. Nothing is queued while you are away.",
  loading: "Checking who you are before anything can be changed.",
  anonymous: "Sign in to make changes — every change is recorded under your name.",
  not_served: "This engine does not make this change for you.",
};

/** The decision itself, over values — what the hook reads, and what a test pins. */
export function writeAccess(
  tool: ActionTool,
  viewer: ViewerState,
  connected: boolean,
  /** The hold the nearest [HoldWrites] placed, or null where none is. */
  held: string | null = null,
): WriteAccess {
  const blocked = (block: Exclude<WriteBlock, "held">): WriteAccess => ({
    can: false,
    block,
    reason: WRITE_REASONS[block],
  });
  if (!connected) return blocked("offline");
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
  const { connected } = useConnection();
  const held = useContext(Held);
  return writeAccess(tool, viewer, connected, held);
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
export type ConfigWriteBlock = "offline" | "loading" | "anonymous" | "no_grant" | "held";

export type ConfigWriteAccess =
  { can: true } | { can: false; block: ConfigWriteBlock; reason: string };

/** The grant a configuration write is decided by (`iam.GrantConfigWrite`). */
export const CONFIG_WRITE_GRANT = "config:write";

/** The sentence each block is shown as — every block but a [HoldWrites]'. */
export const CONFIG_WRITE_REASONS: Readonly<Record<Exclude<ConfigWriteBlock, "held">, string>> = {
  offline: WRITE_REASONS.offline,
  loading: WRITE_REASONS.loading,
  anonymous: "Sign in to change the company's configuration.",
  no_grant:
    "Changing the company's configuration takes the config:write grant, which you do not hold — an administrator who holds people:manage can give it to you.",
};

/** The decision over values — what the hook reads, and what a test pins. */
export function configWriteAccess(
  viewer: ViewerState,
  connected: boolean,
  held: string | null = null,
): ConfigWriteAccess {
  const blocked = (block: Exclude<ConfigWriteBlock, "held">): ConfigWriteAccess => ({
    can: false,
    block,
    reason: CONFIG_WRITE_REASONS[block],
  });
  if (!connected) return blocked("offline");
  if (viewer.loading) return blocked("loading");
  if (viewer.anonymous) return blocked("anonymous");
  if (!viewer.grants.includes(CONFIG_WRITE_GRANT)) return blocked("no_grant");
  if (held) return { can: false, block: "held", reason: held };
  return { can: true };
}

/** Whether this browser may change the company's configuration now, and why not. */
export function useConfigWriteAccess(): ConfigWriteAccess {
  const viewer = useViewer();
  const { connected } = useConnection();
  const held = useContext(Held);
  return configWriteAccess(viewer, connected, held);
}
