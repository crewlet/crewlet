/**
 * A thread, beside the room it is in.
 *
 * # One level deep, by construction
 *
 * A reply to a reply carries the same root, which is what makes "this thread"
 * a range rather than a recursive walk — and it is why nothing in this pane
 * opens another thread. A reply here has no thread affordance of its own, not
 * because it is hidden but because there is nothing it could address: the
 * engine resolves a reply's root from the message being answered, so answering
 * a reply lands in this same thread.
 *
 * # It reads forwards
 *
 * The room pages backwards from its newest message, because that is where a
 * reader arrives. A thread is a conversation somebody is following from its
 * first line, so it pages forwards from the root — which is also why it is its
 * own question rather than a filter on the transcript: one question answering
 * both would make every caller branch on what came back.
 */

import { useEffect, useState } from "react";

import { Button, Skeleton } from "@crewlethq/ui";
import { CloseGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, fmtTime } from "~/lib/format.ts";
import type { ChatMessageView } from "~/protocol/index.ts";
import type { Outgoing } from "./outbox.ts";

import { Composer } from "./Composer.tsx";
import { PendingMessage } from "./Pending.tsx";
import { DEGRADED_POLL_MS, useCoalesced } from "./live.ts";
import { EMPTY_THREAD, mergeReplies, type ThreadTail } from "./messages.ts";
import { PAGE } from "./page.ts";

export interface ThreadPaneProps {
  /** Whether the socket is up. While it is not, this pane polls for the
   *  reason the room does. */
  connected: boolean;
  channelID: string;
  rootID: string;
  /** How many frames this room has seen, so the thread re-reads with it. */
  activity: number;
  onClose: () => void;
  onReplies: (replies: ChatMessageView[]) => void;
  send: (message: { body: string; mentions: string[]; collective: boolean }) => void;
  handles: readonly string[];
  nameOf: (handle: string) => string;
  writable: boolean;
  disabledReason: string;
  /** Replies this tab is still trying to say, drawn under the thread. */
  pending: Outgoing[];
  onRetry: (operationID: string) => void;
  onDiscard: (operationID: string) => void;
  viewer: string;
}

export function ThreadPane({
  connected,
  channelID,
  rootID,
  activity,
  onClose,
  onReplies,
  send,
  handles,
  nameOf,
  writable,
  disabledReason,
  pending,
  onRetry,
  onDiscard,
  viewer,
}: ThreadPaneProps) {
  const [tail, setTail] = useState<ThreadTail>(EMPTY_THREAD);
  // THE CURSOR IS A PARAMETER OF THE QUESTION, and every answer is merged into
  // the same tail — so paging forward and re-reading after a live frame are
  // one mechanism. The root comes back with every page whatever the cursor is,
  // which is what keeps an edited or deleted root current.
  const [cursor, setCursor] = useState("");
  const answer = useQuery(
    "chat_thread",
    {
      channel_id: channelID,
      root_id: rootID,
      limit: PAGE,
      ...(cursor ? { cursor } : {}),
    },
    connected ? undefined : { pollMs: DEGRADED_POLL_MS },
  );

  useEffect(() => {
    if (!answer.data) return;
    setTail((held) => mergeReplies(held, answer.data!));
  }, [answer.data]);

  // The room's own live signal drives this too: a reply is a message in the
  // room, so the frame that makes the transcript re-read is the same frame
  // that means this pane is behind.
  useCoalesced(activity, answer.refetch);

  useEffect(() => onReplies(tail.replies), [tail.replies, onReplies]);

  const root = answer.data?.root;

  return (
    <aside className="chat-thread" aria-label="Thread">
      <div className="row gap-2 chat-thread-head">
        <strong className="t-cell">Thread</strong>
        <span className="spacer" />
        <Button
          variant="tertiary"
          onClick={onClose}
          leadingIcon={<CloseGlyph size="sm" />}
          aria-label="Close the thread"
        >
          Close
        </Button>
      </div>

      {answer.loading && !answer.data && <Skeleton variant="text" rows={4} label="Reading" />}
      <QueryState
        error={answer.error}
        loading={answer.loading}
        empty={
          root
            ? undefined
            : {
                title: "That message is not here",
                hint: "A thread hangs off one message. If it was erased or pruned below the room's horizon there is nothing left to hang off — a deleted message would still be here, as a tombstone.",
              }
        }
      >
        {root && (
          <div className="chat-thread-body">
            <ThreadMessage view={root} nameOf={nameOf} root />
            {tail.replies.map((reply) => (
              <ThreadMessage key={reply.message.id} view={reply} nameOf={nameOf} />
            ))}
            {pending.map((row) => (
              <PendingMessage
                key={row.operationID}
                row={row}
                viewer={viewer}
                nameOf={nameOf}
                onRetry={onRetry}
                onDiscard={onDiscard}
              />
            ))}
            {tail.next && (
              <Button variant="tertiary" onClick={() => setCursor(tail.next)}>
                More replies
              </Button>
            )}
            {answer.data?.participants && answer.data.participants.length > 0 && (
              <p className="t-caption muted">
                {answer.data.participants.map((handle) => nameOf(handle)).join(", ")} have spoken
                here. Taking part in a thread means a reply reaches you — it obliges nothing.
              </p>
            )}
          </div>
        )}
      </QueryState>

      <Composer
        what="this thread"
        inThread
        handles={handles}
        send={send}
        disabled={!writable}
        disabledReason={disabledReason}
      />
    </aside>
  );
}

function ThreadMessage({
  view,
  nameOf,
  root = false,
}: {
  view: ChatMessageView;
  nameOf: (handle: string) => string;
  root?: boolean;
}) {
  const { message } = view;
  const deleted = !!message.deleted_at;
  return (
    <div className={`chat-message${root ? " root" : ""}${deleted ? " tombstone" : ""}`}>
      <div className="row gap-2 chat-author">
        <strong className="truncate t-cell">{nameOf(message.author)}</strong>
        <span className="t-caption muted" title={fmtDateTime(message.created_at)}>
          {fmtTime(message.created_at)}
        </span>
      </div>
      {deleted ? (
        <p className="chat-body muted">
          Deleted{message.deleted_by ? ` by ${nameOf(message.deleted_by)}` : ""}. The row stays so
          the thread keeps the message it hangs off.
        </p>
      ) : (
        <p className="chat-body">{message.body}</p>
      )}
    </div>
  );
}
