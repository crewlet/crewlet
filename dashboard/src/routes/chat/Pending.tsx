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
 * all: the broker did not answer, so the message may be in the room and may
 * never arrive. Saying so — and offering the retry, which reuses the same
 * operation id and therefore cannot say it twice — is the only honest
 * rendering. It leaves by being superseded: the moment the transcript carries
 * the message, this row is gone and the real one is there.
 */
export function PendingMessage({
  row,
  nameOf,
  viewer,
  onRetry,
  onDiscard,
}: {
  row: Outgoing;
  nameOf: (handle: string) => string;
  viewer: string;
  onRetry: (operationID: string) => void;
  onDiscard: (operationID: string) => void;
}) {
  const label =
    row.state === "sending"
      ? "Sending…"
      : row.state === "accepted"
        ? "Said — waiting for this node to catch up"
        : row.state === "unknown"
          ? "The broker did not answer. This may already be in the room."
          : row.detail || "Refused.";
  return (
    // THE STATE IS AN ATTRIBUTE RATHER THAN A CLASS, because it is a VALUE
    // the stylesheet reads: four states from one union, and a class per state
    // would be four names a sheet and a component have to keep agreeing about.
    <div className="chat-message pending" data-state={row.state} data-mine="true">
      <div className="row gap-2 chat-author">
        <strong className="truncate t-cell">{nameOf(viewer)}</strong>
        <span className="t-caption muted">{label}</span>
      </div>
      <p className="chat-body">{row.body}</p>
      {(row.state === "unknown" || row.state === "refused") && (
        <div className="row gap-2">
          <Button variant="secondary" onClick={() => onRetry(row.operationID)}>
            {row.state === "unknown" ? "Ask again under the same id" : "Try again"}
          </Button>
          <Button variant="tertiary" onClick={() => onDiscard(row.operationID)}>
            Take it off the screen
          </Button>
        </div>
      )}
    </div>
  );
}
