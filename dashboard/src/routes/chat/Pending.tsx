/**
 * A message between being typed and being a message.
 *
 * ONE COMPONENT for the room and for a thread, because the four states are the
 * same four wherever somebody said it — and the one that must never be drawn
 * as sent is the same one in both.
 */

import { Button } from "@crewlethq/ui";

import type { Outgoing } from "./outbox.ts";

/**
 * A message this tab is still trying to say.
 *
 * NONE OF THESE RENDER AS SENT, and the unknown one is why the state exists at
 * all: nobody can say whether it landed, so the message may be in the room and
 * may never arrive. Saying so — and offering the retry, which reuses the same
 * operation key and therefore cannot say it twice — is the only honest
 * rendering.
 * It leaves by being superseded: the moment the transcript carries the
 * message, this row is gone and the real one is there.
 *
 * A RETRY IS OFFERED ONLY WHERE ONE CAN LAND: an unknown row, and a refusal
 * the NODE made. A refusal of the message itself — too long, a room that takes
 * no messages — is refused the same way however often it is sent, and a
 * button that only ever repeats a refusal is a button somebody presses twice.
 */
export function PendingMessage({
  row,
  nameOf,
  viewer,
  onRetry,
  onDiscard,
  blocked = "",
}: {
  row: Outgoing;
  nameOf: (handle: string) => string;
  viewer: string;
  onRetry: (opId: string) => void;
  onDiscard: (opId: string) => void;
  /** Why this browser cannot send anything now — offline, say — which holds
   *  the retry with the sentence rather than sending nothing silently. */
  blocked?: string;
}) {
  const label =
    row.state === "sending"
      ? "Sending…"
      : row.state === "accepted"
        ? "Said — waiting for this node to catch up"
        : row.state === "unknown"
          ? "Nobody can say whether this landed. It may already be in the room."
          : row.detail || "Refused.";
  const retryable = row.state === "unknown" || (row.state === "refused" && row.retryable);
  return (
    // THE STATE IS AN ATTRIBUTE RATHER THAN A CLASS, because it is a VALUE
    // the stylesheet reads: four states from one union, and a class per state
    // would be four names a sheet and a component have to keep agreeing about.
    <div className="chat-message pending" data-state={row.state}>
      <div className="row gap-2 chat-author">
        <strong className="truncate t-cell">{nameOf(viewer)}</strong>
        <span className="t-caption muted">{label}</span>
      </div>
      <p className="chat-body">{row.body}</p>
      {(row.state === "unknown" || row.state === "refused") && (
        <div className="row gap-2">
          {retryable && (
            <Button
              variant="secondary"
              size="small"
              disabledReason={blocked || undefined}
              title={blocked || undefined}
              onClick={() => onRetry(row.opId)}
            >
              {row.state === "unknown" ? "Ask again under the same id" : "Try again"}
            </Button>
          )}
          <Button variant="ghost" size="small" onClick={() => onDiscard(row.opId)}>
            Take it off the screen
          </Button>
        </div>
      )}
    </div>
  );
}
