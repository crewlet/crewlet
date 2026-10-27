/**
 * Making one change from a screen: the press, what it came to, and what the
 * person is told.
 *
 * WRITES ARE CONFIRMED, NOT OPTIMISTIC. Nothing on screen moves until the
 * engine has answered: an `applied` or `pending` answer raises this tab's read
 * floor for the domain (`protocol/session.ts`), every question that reads that
 * domain asks again at the floor, and the screen is redrawn from an answer
 * that includes the change. A board that moved the card first and put it back
 * on a refusal would have shown a person a company that never existed.
 *
 * WHAT EACH OUTCOME TELLS A PERSON:
 *
 *  - `applied`: a toast naming what changed, which goes by itself; or, where
 *    the engine answered caveats beside it, one carrying their sentences,
 *    which stays only when one of them asks the person to act
 *    ([caveatToast]);
 *  - `pending`: "Sent. This node has not applied it yet", because the
 *    reads that follow wait for it and a person watching a spinner deserves
 *    to know why;
 *  - `unknown`: a toast that STAYS: "Could not confirm. It may have
 *    landed", with a Retry that sends the SAME operation key, which the engine
 *    scopes to the person and takes as the write's identity, so a retry is
 *    the first attempt again rather than a second change. Never retried on
 *    its own: whether to try again is the person's call about their own
 *    change;
 *  - `refused` — the engine's reason, held on the hook (`refusal`) for the
 *    control to draw beside itself, where it stays until the next press:
 *    a refusal in a toast is gone before the person has read which field it
 *    named.
 *
 * AND OFFLINE IS DISABLED, NEVER QUEUED — `lib/useWriteAccess.ts` says so on
 * the control, and `run` refuses to send while it does.
 */

import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { useToast } from "@crewlethq/ui";
import { isAbort } from "~/protocol/rest.ts";
import {
  act,
  newActOpID,
  type ActionArgs,
  type ActionTool,
  type ActResult,
  type Caveat,
} from "~/protocol/act.ts";
import { useWriteAccess, type WriteAccess } from "./useWriteAccess.ts";

/** A refusal as a control draws it. */
export interface Refusal {
  sentence: string;
  /** The engine's own remedy, where it sent one. */
  hint: string;
  /**
   * The grants any one of which would have admitted this person, where the
   * refusal was on authority — empty otherwise. What would change the answer,
   * which a sentence alone cannot say for a gate the person cannot see.
   */
  grants: readonly string[];
  /** Sending it again may succeed; the Retry reuses the operation key. */
  retryable: boolean;
}

/** What a press is called, for the sentence that reports it. */
export interface PressLabel {
  /** What happened, past tense, naming the object: "Assigned ENG-42 to Ana". */
  done: string;
  /**
   * The control draws what came back in place of a toast: an answer the
   * person asked for is shown where they asked, and a toast saying "Answered"
   * over the answer itself is the same fact twice. Applied, pending and
   * unknown then raise no toast and the caller renders the result `run`
   * resolves with; a refusal is held on the hook exactly as for any press.
   */
  quiet?: boolean;
  /**
   * The control draws the press's caveats where it was made (a board draws
   * a card's missed place beside the board), so the toast reports the press
   * alone and the same sentence is never shown twice. The caveats are still
   * on the result `run` resolves with, in the person's words.
   */
  drawsCaveats?: boolean;
}

/** How one press is sent. */
export interface RunOptions {
  /**
   * Abandons the press: `run` resolves with null and nothing is reported.
   * For a press that a later one supersedes (the palette's answer, when the
   * question changes) — never for a change the person made, whose outcome
   * they are owed.
   */
  signal?: AbortSignal;
}

export interface Act<T extends ActionTool> {
  /** Whether this browser may make the change, and why not. */
  access: WriteAccess;
  /** A press is in flight. */
  busy: boolean;
  /** The last press's refusal, until the next press. */
  refusal: Refusal | null;
  /**
   * Make the change. Resolves with what it came to, or null when nothing was
   * sent or the press was abandoned through its signal.
   */
  run: (args: ActionArgs<T>, label: PressLabel, options?: RunOptions) => Promise<ActResult | null>;
  /**
   * Send the press `refusal` answered again — its arguments and its operation
   * key — or nothing when there is no refusal to retry.
   */
  retry: () => Promise<ActResult | null>;
  /** Forget a refusal the person dismissed. */
  dismiss: () => void;
}

/** The sentence a pending answer is shown as. */
export const PENDING_SENTENCE = "Sent. This node has not applied it yet.";

/** The title an unknown answer is shown under. */
export const UNKNOWN_TITLE = "Could not confirm. It may have landed";

/**
 * One press: what was sent, under which operation key, and what it is called.
 * Every retry of a press sends THIS object again, never "whatever was pressed
 * last" — see [useAct].
 */
interface Press<T extends ActionTool> {
  args: ActionArgs<T>;
  opId: string;
  label: PressLabel;
}

/** The toast a landed write with caveats is shown as. */
export interface CaveatToast {
  /** `warning` (stays until dismissed) or `info` (goes by itself). */
  variant: "warning" | "info";
  /** Every caveat's sentence, in the order the engine answered them. */
  message: string;
  /**
   * How long an `info` toast stays, in milliseconds, from how long its
   * message takes to read ([readingDuration]); absent on a `warning`, which
   * the kit keeps until it is dismissed.
   */
  duration?: number;
}

/** A reader's pace, in milliseconds per word: two hundred words a minute. */
const READ_MS_PER_WORD = 300;
/** The time a toast takes to be noticed at all, before a word of it is read. */
const READ_LEAD_MS = 2_000;
/** No toast that goes by itself goes sooner than the kit's own info default. */
const READ_FLOOR_MS = 4_000;
/**
 * No toast that goes by itself stays longer than this: past it, a sentence is
 * long enough to be one somebody must act on, which is `required`'s sticky
 * warning and not an info toast.
 */
const READ_CAP_MS = 15_000;

/**
 * How long a toast that goes by itself stays, from how long its message takes
 * to read: the time to notice it, then a reader's pace per word, between the
 * kit's own default and a cap.
 *
 * THE KIT'S FOUR SECONDS FIT A TITLE, not a caveat: "The launch date holds a
 * date without a time, so it was saved as 2026-03-04, the day …" is thirty
 * words, and a pending press's sentence beside one is forty-five, gone before
 * the second line was read.
 */
export function readingDuration(message: string): number {
  const words = message.trim().split(/\s+/).filter(Boolean).length;
  return Math.min(READ_CAP_MS, Math.max(READ_FLOOR_MS, READ_LEAD_MS + words * READ_MS_PER_WORD));
}

/**
 * The toast a landed write's caveats are shown as, or null when it has none.
 *
 * THE ENGINE'S WORDS FOR A PERSON, and never the receipt's `warnings`: those
 * are written for a model and name tools and arguments ("Use ask: ana"), and
 * the engine answers every caveat's person phrasing in `caveats` instead
 * (ADR-0060). A toast that read the receipt showed a person advice only a
 * model could follow, and kept it on screen until they closed it.
 *
 * STICKY ONLY WHEN SOMEBODY MUST ACT. The engine states per caveat whether it
 * asks the person to act (`required`: a setting left broken until somebody
 * changes it) or only to know (`none`: a value stored in its canonical form).
 * The kit's `warning` toast stays until it is dismissed and its `info` toast
 * goes by itself, each by its own default, so one caveat that must be acted
 * on makes the press's toast a warning, and anything else is information that
 * does not demand a click to clear, for as long as it takes to read
 * ([readingDuration]). lead is a sentence the toast says before the caveats
 * (a pending press's), counted in that time.
 */
export function caveatToast(caveats: readonly Caveat[], lead = ""): CaveatToast | null {
  if (caveats.length === 0) return null;
  const message = [lead, ...caveats.map((c) => c.message)].filter((s) => s !== "").join(" ");
  if (caveats.some((c) => c.action === "required")) return { variant: "warning", message };
  return { variant: "info", message, duration: readingDuration(message) };
}

/** One change a screen can make, as the signed-in person. */
export function useAct<T extends ActionTool>(tool: T): Act<T> {
  const access = useWriteAccess(tool);
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  // THE REFUSAL TRAVELS WITH THE PRESS IT ANSWERED, and every Retry is bound
  // to its own press rather than read from a shared "last press". A toast's
  // Retry is a closure made when its answer arrived and may be clicked many
  // presses later; reading the latest press there re-sent a DIFFERENT change
  // under a different operation key — press A came back unknown, press B
  // went out, and A's still-visible Retry sent B while A was never retried.
  const [refused, setRefused] = useState<{ refusal: Refusal; press: Press<T> } | null>(null);
  // WHETHER THIS BROWSER MAY ACT NOW, read when a Retry is pressed rather than
  // when its toast was made: a toast outlives the render that drew it, and a
  // Retry pressed after the socket dropped must refuse to send exactly as the
  // control does — offline is disabled, never queued.
  const can = useRef(access.can);
  useLayoutEffect(() => {
    can.current = access.can;
  }, [access.can]);

  // PRESSES IN FLIGHT, COUNTED: a press abandoned while a newer one is out
  // settles first, and a plain flag it cleared would call the newer one done.
  const inFlight = useRef(0);

  const send = useCallback(
    async (press: Press<T>, signal?: AbortSignal): Promise<ActResult | null> => {
      if (!can.current) return null;
      inFlight.current += 1;
      setBusy(true);
      setRefused(null);
      let result: ActResult;
      try {
        result = await act(tool, press.args, { opId: press.opId, signal });
      } catch (err) {
        // ABANDONED, which is the caller's own doing and not an answer:
        // nothing to report, and nothing to retry.
        if (isAbort(err)) return null;
        throw err;
      } finally {
        inFlight.current -= 1;
        setBusy(inFlight.current > 0);
      }
      if (press.label.quiet && result.kind !== "refused") return result;
      switch (result.kind) {
        case "applied": {
          // A CHANGE THAT LANDED WITH A CAVEAT SAYS IT. The engine answers
          // caveats beside an applied write when it did something other
          // than the obvious (a create filed to triage because the
          // project's default assignee left the chart), and a plain "done"
          // over it tells the person the opposite of what happened. How
          // long it stays is the caveat's to say ([caveatToast]); the
          // variant's own duration applies.
          const caveated = press.label.drawsCaveats ? null : caveatToast(result.caveats);
          if (caveated) {
            toast.show({
              variant: caveated.variant,
              title: press.label.done,
              message: caveated.message,
              duration: caveated.duration,
            });
          } else {
            toast.ok(press.label.done);
          }
          break;
        }
        case "pending": {
          // A PENDING WRITE WAS DECIDED, so its caveats are as true as an
          // applied one's, and said beside the wait.
          const caveated = press.label.drawsCaveats
            ? null
            : caveatToast(result.caveats, PENDING_SENTENCE);
          toast.show({
            variant: caveated?.variant ?? "info",
            title: press.label.done,
            message: caveated?.message ?? PENDING_SENTENCE,
            duration: caveated?.duration,
          });
          break;
        }
        case "unknown":
          toast.show({
            // ONE TOAST PER PRESS, replaced rather than stacked when its own
            // Retry comes back unknown again.
            id: `act-unknown-${press.opId}`,
            variant: "warning",
            title: UNKNOWN_TITLE,
            message: `${press.label.done}? ${result.reason}`,
            duration: 0,
            action: { label: "Retry", onClick: () => void send(press) },
          });
          break;
        case "refused":
          setRefused({
            refusal: {
              sentence: result.sentence,
              hint: result.hint,
              grants: result.grants,
              retryable: result.retryable,
            },
            press,
          });
          break;
      }
      return result;
    },
    [tool, toast],
  );

  // A NEW PRESS IS A NEW OPERATION KEY, minted here in the handler and never
  // per attempt: only a retry of that press reuses it.
  const run = useCallback(
    (args: ActionArgs<T>, label: PressLabel, options?: RunOptions) =>
      send({ args, opId: newActOpID(), label }, options?.signal),
    [send],
  );

  const retry = useCallback(
    () => (refused ? send(refused.press) : Promise.resolve(null)),
    [refused, send],
  );

  return {
    access,
    busy,
    refusal: refused?.refusal ?? null,
    run,
    retry,
    dismiss: useCallback(() => setRefused(null), []),
  };
}
