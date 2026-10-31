/**
 * A message between being typed and being a message, drawn in place.
 *
 * ONE ROW for the room and for a thread, because the four states are the same
 * four wherever somebody said it, and the one that must never be drawn as
 * sent is the same one in both. It is the reader's own message row, drawn
 * faint while it is on its way, so it sits where the real one will land.
 *
 * NONE OF THESE RENDER AS SENT, and the unknown one is why the state exists
 * at all: nobody can say whether it landed, so the message may be in the room
 * and may never arrive. Saying so, and offering the retry, which reuses the
 * same operation key and therefore cannot say it twice, is the only honest
 * rendering. It leaves by being superseded: the moment the transcript carries
 * the message, this row is gone and the real one is there.
 *
 * A RETRY IS OFFERED ONLY WHERE ONE CAN LAND: an unknown row, and a refusal
 * the NODE made. A refusal of the message itself (too long, a room that takes
 * no messages) is refused the same way however often it is sent, and a
 * button that only ever repeats a refusal is a button somebody presses twice.
 */

import { Button, ChatMessage, ChatMessageFailure } from "@crewlethq/ui";
import { fmtDateTime } from "~/lib/format.ts";

import { MessageBody } from "./Body.tsx";
import { clockOf, timeOf } from "./days.ts";
import { ChatFace } from "./Face.tsx";
import type { Outgoing } from "./outbox.ts";
import { useChatPeople } from "./people.ts";
import { useRoomScope } from "./scope.tsx";

/** What a row on its way says about itself. */
export function pendingLine(row: Outgoing): string {
  switch (row.state) {
    case "sending":
      return "Sending…";
    case "accepted":
      return "Sent. Waiting for this node to catch up.";
    case "unknown":
      return "Couldn’t confirm this was sent. It may already be in the room.";
    case "refused":
      return row.detail ? `Not sent. ${row.detail}` : "Not sent.";
  }
}

export function PendingRow({
  row,
  continuation = false,
}: {
  row: Outgoing;
  continuation?: boolean;
}) {
  const scope = useRoomScope();
  const people = useChatPeople();
  const me = people.person(scope.viewer, "human");
  const access = row.threadRoot ? scope.outbox.reply : scope.outbox.post;
  // WHY THIS BROWSER CANNOT SEND ANYTHING NOW, after the room's own rule:
  // the retry is held with the sentence rather than sending nothing silently.
  const blocked = scope.rules.post || (access.can ? "" : access.reason);
  const failed = row.state === "unknown" || row.state === "refused";
  const retryable = row.state === "unknown" || (row.state === "refused" && row.retryable);
  const at = new Date(row.startedAt).toISOString();
  return (
    <ChatMessage
      id={row.opId}
      label={`${me.name}, ${pendingLine(row)}`}
      avatar={<ChatFace person={me} size={36} />}
      author={<span>{me.name}</span>}
      time={timeOf(at)}
      gutterTime={clockOf(at)}
      timeTitle={fmtDateTime(at)}
      continuation={continuation && !failed}
      pending={failed ? "failed" : "sending"}
      footer={
        failed && retryable && !blocked ? (
          // A RETRY THAT CAN LAND NOW: the kit's failed line, its retry under
          // the row's own operation key.
          <ChatMessageFailure
            reason={pendingLine(row)}
            onRetry={() => void scope.outbox.retry(row.opId)}
            onDiscard={() => scope.outbox.discard(row.opId)}
            labels={{ retry: row.state === "unknown" ? "Send again" : "Try again" }}
          />
        ) : (
          // EVERY OTHER STATE SAYS WHAT IT IS: on its way, or failed with a
          // retry held by why (offline, a room that stopped taking messages),
          // or a refusal no retry can change, which can only be discarded.
          <div className="chat-pending-line" role="status" data-state={row.state}>
            <span>{pendingLine(row)}</span>
            {failed && retryable && (
              <Button size="small" variant="ghost" disabledReason={blocked} title={blocked}>
                {row.state === "unknown" ? "Send again" : "Try again"}
              </Button>
            )}
            {failed && (
              <Button size="small" variant="ghost" onClick={() => scope.outbox.discard(row.opId)}>
                Discard
              </Button>
            )}
          </div>
        )
      }
    >
      <MessageBody message={{ body: row.body }} viewer={scope.viewer} nameOf={people.nameOf} />
    </ChatMessage>
  );
}
