/**
 * An operator's gesture on a placement map — the object store's or the estate
 * map's — as a dialog sends it and reads it back.
 *
 * # One compare-and-set on the map, answered whole
 *
 * Each gesture is a read of the stored map, a pure change and a
 * compare-and-set in the engine (`internal/engine/mapcontrol.go`, one loop for
 * both maps), so the answer is not "done" but whether the map NOW SAYS what
 * was asked — `landed` — and the map as it stands. A gesture that lost every
 * race to the map's maintainer is not a refusal and not a fault: nothing it
 * asked is in the map, the answer's `hint` says so, and sending it again is the
 * remedy. A request nobody answered is safe to send again too, for a reason
 * that is not the same for every gesture — which each dialog says in `resend`.
 *
 * ONE MODULE FOR BOTH MAPS, because what it reads back is one rule
 * (ADR-0008): written per map, the two dialogs' ideas of a lost race, a
 * refusal and an unanswered request would be two readings of one answer.
 */

import { Callout } from "@crewlethq/ui";
import { ClockGlyph } from "@crewlethq/icons/glyphs";
import { RestError, type RestResponse } from "~/protocol/index.ts";

/** The part of every gesture answer this module reads. */
export interface GestureAnswer {
  /** False only when every attempt lost its race: nothing asked is in the map. */
  landed: boolean;
  epoch: number;
  /** What to do next, where there is something — in no surface's vocabulary. */
  hint?: string;
}

/** What the last request came back with, rendered under a dialog's text. */
export type Heard<A extends GestureAnswer> =
  | { kind: "answer"; answer: A }
  | { kind: "refused"; detail: string; hint: string }
  | { kind: "unanswered"; why: string };

/**
 * Reads back what one gesture's request did.
 *
 * THE REQUEST IS THE CALLER'S, written at its call site with its path as a
 * literal, rather than a path handed in here: `src/protocol/proxy.test.ts`
 * holds the dev server's proxy to every engine path the source reaches, and a
 * helper that takes its path as a parameter is a call whose path no gate can
 * read — which is how `/objects` and `/estate` went unproxied, so `npm run dev`
 * answered every gesture with Vite's own 404.
 */
export async function hearGesture<A extends GestureAnswer>(
  sent: Promise<RestResponse>,
): Promise<Heard<A>> {
  try {
    const answer = (await sent).body as A | null;
    if (!answer) return { kind: "unanswered", why: "the node answered with no body" };
    return { kind: "answer", answer };
  } catch (err) {
    if (err instanceof RestError && err.unanswered) {
      // NOT A REFUSAL: nothing here knows what the node did. Sending the
      // same gesture again is how to find out, and the dialog says what
      // that does.
      return {
        kind: "unanswered",
        why:
          err.status === 0 || err.code === "unreadable_body"
            ? err.message
            : `a ${err.status} came back with no engine error code in it — something in front of the node answered`,
      };
    }
    if (err instanceof RestError) return { kind: "refused", detail: err.message, hint: err.hint };
    return { kind: "refused", detail: String(err), hint: "" };
  }
}

/** Whether what came back is a gesture the map now says. */
export function landed<A extends GestureAnswer>(heard: Heard<A> | null): boolean {
  return heard?.kind === "answer" && heard.answer.landed;
}

/** What sending a gesture again does, for one the map already says changes nothing. */
export const RESEND_CHANGES_NOTHING =
  "Sending the same gesture again is safe: one the map already says changes nothing and writes nothing.";

/** What sending a hold again does: it is the one gesture a repeat is not a no-op for. */
export const RESEND_REPLACES_THE_HOLD =
  "Sending the hold again is safe, but it replaces the one in force: its length is counted from the resend.";

/**
 * The outcome line under a gesture's text. `map` names the map in the epoch
 * line; `resend` says what sending the same gesture again does, for a request
 * nobody answered.
 */
export function GestureOutcome<A extends GestureAnswer>({
  heard,
  done,
  resend,
  map,
}: {
  heard: Heard<A>;
  done: React.ReactNode;
  resend: string;
  map: string;
}) {
  switch (heard.kind) {
    case "answer":
      return heard.answer.landed ? (
        <Callout variant="success" role="status">
          <span className="col" style={{ gap: 6 }}>
            <span>{done}</span>
            {heard.answer.hint && <span className="t-caption">{heard.answer.hint}</span>}
            <span className="t-caption">
              {map} epoch {heard.answer.epoch}
            </span>
          </span>
        </Callout>
      ) : (
        <Callout variant="warning" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>
              <strong>Not in the map.</strong> {heard.answer.hint}
            </span>
          </span>
        </Callout>
      );
    case "unanswered":
      return (
        <Callout variant="warning" role="alert" icon={<ClockGlyph size="md" />}>
          <span>
            <strong>No answer.</strong> {heard.why}, so whether the map changed is unknown. {resend}
          </span>
        </Callout>
      );
    case "refused":
      return (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>{heard.detail}</span>
            {heard.hint && <span className="t-caption">{heard.hint}</span>}
          </span>
        </Callout>
      );
  }
}
