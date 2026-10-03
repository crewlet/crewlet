/**
 * Making one change from a screen: the press, what it came to, and what the
 * person is told.
 *
 * WRITES ARE CONFIRMED, NOT OPTIMISTIC. Nothing on screen moves until the
 * engine has answered: an `applied` or `pending` answer raises this tab's read
 * floor for the domain (`protocol/floors.ts`), every question that reads that
 * domain asks again at the floor, and the screen is redrawn from an answer
 * that includes the change. A board that moved the card first and put it back
 * on a refusal would have shown a person a company that never existed.
 *
 * WHAT EACH OUTCOME TELLS A PERSON:
 *
 *  - `applied` — a toast naming what changed, which goes by itself;
 *  - `pending` — "Sent — this node has not applied it yet", because the
 *    reads that follow wait for it and a person watching a spinner deserves
 *    to know why;
 *  - `unknown` — a toast that STAYS: "Could not confirm — it may have
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
export const PENDING_SENTENCE = "Sent — this node has not applied it yet.";

/** The title an unknown answer is shown under. */
export const UNKNOWN_TITLE = "Could not confirm — it may have landed";

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

/**
 * The caveats an applied write's own answer carries (`warnings`), as sentences
 * — and none for an answer that carries none, or carries them in a shape this
 * build does not read.
 */
export function receiptWarnings(receipt: unknown): string[] {
  const warnings = (receipt as { warnings?: unknown } | null)?.warnings;
  return Array.isArray(warnings)
    ? warnings.filter((w): w is string => typeof w === "string" && w.trim() !== "")
    : [];
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
          // `warnings` beside an applied write when it did something other
          // than the obvious — a create filed to triage because the
          // project's default assignee left the chart — and a plain "done"
          // over it tells the person the opposite of what happened.
          const warned = receiptWarnings(result.receipt);
          if (warned.length > 0) {
            toast.show({
              variant: "warning",
              title: press.label.done,
              message: warned.join(" "),
              duration: 0,
            });
          } else {
            toast.ok(press.label.done);
          }
          break;
        }
        case "pending":
          toast.show({ variant: "info", title: press.label.done, message: PENDING_SENTENCE });
          break;
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
