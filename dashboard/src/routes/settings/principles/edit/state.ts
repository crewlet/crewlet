/**
 * The editing state of the Principles page as one value and one reducer.
 *
 * EACH PART IS EDITED ON ITS OWN. A part in edit is a [Session]: what the
 * person started from (the base, which decides whether a save is a conflict),
 * what they have typed (the draft) and where the save stands. Several parts
 * may be open at once, and closing one never touches another.
 *
 * A SAVED PART IS SHOWN AS SAVED until the page has it. A save answers with
 * the revision it stored and the epoch it activates at, and the org
 * projection this page reads arrives only once this node has applied it, a
 * moment later. Between the two the card would show the words the person
 * just replaced, which reads as a save that did not take. So a save leaves a
 * [Landed] value the card shows until the projection agrees, moves on to
 * something else, or this node is done with the revision ([landedView]) — and
 * says so in place of the saved words when this node refused it.
 *
 * NOTHING HERE IS KEPT IN STORAGE. A draft is the company's policies in the
 * person's words, and storage outlives the session and is offered to whoever
 * uses the tab next (the builder keeps only its log for the same reason,
 * `routes/org/builder/model/persistence.ts`). A reload or a closed tab asks
 * first instead.
 */

import type { EngineHealth } from "~/contract/health.ts";
import { nodeStanding, type ActivatedRevision } from "~/lib/nodeStanding.ts";
import { samePart, type CharterPart, type PartProblems, type PartValue } from "./charter.ts";

/** How a part came to be edited, which decides where the focus lands. */
export type Origin =
  /** Edit on a written part: the focus goes to the first field. */
  | "edit"
  /** Add on an unwritten part: the focus goes where the first words are written. */
  | "add"
  /** An address that opened the page in edit (`?edit=`), from the org chart's company node. */
  | "address";

/** Where one part's save stands. */
export type Phase =
  | { readonly kind: "editing" }
  | { readonly kind: "saving" }
  /** Somebody changed the part since the person started; `theirs` is what it says now. */
  | { readonly kind: "conflict"; readonly theirs: PartValue }
  /** The save did not land, and why. */
  | { readonly kind: "failed"; readonly message: string };

/** One part in edit. */
export interface Session {
  readonly base: PartValue;
  readonly draft: PartValue;
  /**
   * The policy typed in the list's new-item box and not yet added with Enter.
   * It is part of the draft ([effective]): leaving it out, a policy somebody
   * wrote and then pressed Save on would be lost without a word. Always "" for
   * a statement.
   */
  readonly pending: string;
  /** The part was opened from the charter the page had, and has not yet been read from the engine. */
  readonly seeded: boolean;
  readonly origin: Origin;
  /**
   * The part takes the focus as it opens. Every Edit and Add does; of the
   * parts an address opens together, only the first, since one focus is all
   * a page has.
   */
  readonly focusOnOpen: boolean;
  /** What the engine refused the last save over, placed where it applies; cleared by the next keystroke. */
  readonly refused: PartProblems | null;
  readonly phase: Phase;
}

/** A part just saved, shown as saved until the page reads it back. */
export interface Landed {
  readonly value: PartValue;
  /**
   * The revision the save stored and the epoch it activates at, from its
   * answer; null when the save was settled by a read, or is a colleague's
   * words taken over, which name neither.
   */
  readonly activated: ActivatedRevision | null;
  /** What the page showed for the part when the save landed. */
  readonly shownWas: PartValue;
}

export interface EditingState {
  readonly sessions: Readonly<Partial<Record<CharterPart, Session>>>;
  readonly landed: Readonly<Partial<Record<CharterPart, Landed>>>;
}

export const INITIAL: EditingState = { sessions: {}, landed: {} };

export type Action =
  | {
      readonly type: "open";
      readonly part: CharterPart;
      readonly base: PartValue;
      readonly origin: Origin;
      readonly focus: boolean;
    }
  | { readonly type: "change"; readonly part: CharterPart; readonly draft: PartValue }
  /** The policy typed in the new-item box changed. */
  | { readonly type: "pending"; readonly part: CharterPart; readonly text: string }
  /**
   * The engine's charter arrived for a part just opened from the page's own
   * copy, which can lag the engine's: an untouched draft starts from it.
   */
  | { readonly type: "seed"; readonly part: CharterPart; readonly value: PartValue }
  | { readonly type: "saving"; readonly part: CharterPart }
  | {
      readonly type: "saved";
      readonly part: CharterPart;
      readonly value: PartValue;
      readonly activated: ActivatedRevision | null;
      readonly shownWas: PartValue;
    }
  | { readonly type: "conflict"; readonly part: CharterPart; readonly theirs: PartValue }
  /** Keep the draft over what somebody else wrote: their words become the base, so the next Save writes over them. */
  | { readonly type: "keep_mine"; readonly part: CharterPart }
  /** Drop the draft for what somebody else wrote, shown at once although the page may not have it yet. */
  | { readonly type: "use_theirs"; readonly part: CharterPart; readonly shownWas: PartValue }
  | {
      readonly type: "failed";
      readonly part: CharterPart;
      readonly message: string;
      readonly refused: PartProblems | null;
    }
  | { readonly type: "close"; readonly part: CharterPart }
  /** The page reads the landed value now: show the projection again. */
  | { readonly type: "settle"; readonly part: CharterPart };

export function reducer(state: EditingState, action: Action): EditingState {
  const session = state.sessions[action.part];
  switch (action.type) {
    case "open":
      // OPENING AN OPEN PART KEEPS ITS DRAFT: a second press of Add, or the
      // address arriving again, is not a reason to lose what was typed.
      if (session) return state;
      return withSession(state, action.part, {
        base: action.base,
        draft: action.base,
        pending: "",
        seeded: false,
        origin: action.origin,
        focusOnOpen: action.focus,
        refused: null,
        phase: { kind: "editing" },
      });
    case "change":
      if (!session || session.phase.kind === "saving") return state;
      return withSession(state, action.part, {
        ...session,
        draft: action.draft,
        refused: null,
        // A FAILURE IS ANSWERED BY THE NEXT KEYSTROKE, which is the person
        // acting on it. A CONFLICT IS NOT: it is a question about whose words
        // stand, and typing on does not answer it; only Keep mine or Use
        // theirs does.
        phase: session.phase.kind === "failed" ? { kind: "editing" } : session.phase,
      });
    case "pending":
      if (!session || session.phase.kind === "saving") return state;
      return withSession(state, action.part, {
        ...session,
        pending: action.text,
        phase: session.phase.kind === "failed" ? { kind: "editing" } : session.phase,
      });
    case "seed":
      // ONLY AN UNTOUCHED DRAFT: once the person has typed, the words they
      // started from stay the base, and a save that finds the engine holding
      // something else asks them (`save.ts`) rather than moving their draft.
      if (!session || session.seeded) return state;
      if (session.pending !== "" || !samePart(session.draft, session.base)) {
        return withSession(state, action.part, { ...session, seeded: true });
      }
      return withSession(state, action.part, {
        ...session,
        base: action.value,
        draft: action.value,
        seeded: true,
      });
    case "saving":
      if (!session) return state;
      return withSession(state, action.part, { ...session, phase: { kind: "saving" } });
    case "saved": {
      const { [action.part]: _closed, ...sessions } = state.sessions;
      return {
        sessions,
        landed: {
          ...state.landed,
          [action.part]: {
            value: action.value,
            activated: action.activated,
            shownWas: action.shownWas,
          },
        },
      };
    }
    case "conflict":
      if (!session) return state;
      return withSession(state, action.part, {
        ...session,
        phase: { kind: "conflict", theirs: action.theirs },
      });
    case "keep_mine":
      if (!session || session.phase.kind !== "conflict") return state;
      return withSession(state, action.part, {
        ...session,
        base: session.phase.theirs,
        phase: { kind: "editing" },
      });
    case "use_theirs": {
      if (!session || session.phase.kind !== "conflict") return state;
      const { [action.part]: _closed, ...sessions } = state.sessions;
      return {
        sessions,
        landed: {
          ...state.landed,
          [action.part]: {
            value: session.phase.theirs,
            activated: null,
            shownWas: action.shownWas,
          },
        },
      };
    }
    case "failed":
      if (!session) return state;
      return withSession(state, action.part, {
        ...session,
        refused: action.refused,
        phase: { kind: "failed", message: action.message },
      });
    case "close": {
      if (!session) return state;
      const { [action.part]: _closed, ...sessions } = state.sessions;
      return { ...state, sessions };
    }
    case "settle": {
      if (!state.landed[action.part]) return state;
      const { [action.part]: _settled, ...landed } = state.landed;
      return { ...state, landed };
    }
  }
}

function withSession(state: EditingState, part: CharterPart, session: Session): EditingState {
  return { ...state, sessions: { ...state.sessions, [part]: session } };
}

/**
 * What a session's draft is as Save would send it: the policies with the one
 * typed in the new-item box after them (a blank one adds nothing), or the
 * statement.
 */
export function effective(session: Session): PartValue {
  return typeof session.draft === "string" ? session.draft : [...session.draft, session.pending];
}

/** Whether a session holds typed work that closing it would lose. */
export function isDirty(session: Session | undefined): boolean {
  return session !== undefined && !samePart(effective(session), session.base);
}

/** Where a landed save stands for the card that shows it ([landedView]). */
export type LandedView = "applying" | "refused" | "settled";

/**
 * Where a landed save stands for the card that shows it:
 *
 * - `settled`: the page has caught up with it — it reads the saved words, it
 *   reads something newer than what it showed when the save landed (a
 *   colleague's later change), or this node has applied the save's revision
 *   or passed it for a later one, after which the projection is the answer
 *   whatever it says;
 * - `refused`: this node's attempt at the save's revision failed, so the
 *   projection it serves is the part as it was, and stays so — the card
 *   draws it, and says the save was refused here;
 * - `applying`: nothing says yet, and the card goes on drawing the saved
 *   words.
 *
 * BY WHERE THIS NODE STANDS ON THE REVISION (`nodeStanding`), never by its
 * applied epoch alone: a refusal leaves that where it was, so a refused save
 * drew the saved words, said to be shown "as soon as this node applies" it,
 * for as long as the page stayed open.
 *
 * `health` is this node's LAST push (`useLastHealth`), null only before the
 * tab has had one, when the save is still where it landed: applying. Never
 * the live slice, which a dropped socket clears — read as a standing, that
 * put the saved words back over a refusal until the socket returned.
 */
export function landedView(
  landed: Landed,
  shown: PartValue,
  health: Pick<EngineHealth, "applied_epoch" | "config_attempt"> | null,
): LandedView {
  if (samePart(shown, landed.value)) return "settled";
  if (!samePart(shown, landed.shownWas)) return "settled";
  if (landed.activated === null || health === null) return "applying";
  switch (nodeStanding(health, landed.activated).kind) {
    case "applied":
    case "passed":
      return "settled";
    case "refused":
      return "refused";
    case "applying":
      return "applying";
  }
}
