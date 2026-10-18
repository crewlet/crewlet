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
 *
 * # Paged as every history is, and each page read again when a frame names it
 *
 * The pages are `usePaged`'s: the first an ordinary question from the root,
 * each after it asked once when the reader asks for more and held under the
 * cursor it was asked with, every page's foot checked against the next, and
 * one change to what is held at a time. This pane kept a pager of its own
 * once, and it was the second copy of one rule drifting from the first: a
 * page it asked for while a re-read moved the last page's end was dropped, so
 * "More replies" did nothing and came back.
 *
 * A page is RE-ASKED, under its own cursor, when a live frame in the room
 * names something it holds: the root, a reply on it, or — for the last page —
 * a new reply arriving at the end ([threadPagesTouched]). The cursor used to
 * be a parameter of the one question, so after "More replies" every live
 * re-read asked only from there onwards: an edit or a tombstone on an earlier
 * page was never drawn, and an erased reply stayed on screen.
 *
 * NOT EVERY PAGE ON EVERY FRAME. The engine bounds who takes part in a thread
 * (`MaxThreadParticipants`) and not how much they say, so a long thread paged
 * forward asked a query per page for every message anywhere in the room —
 * competing with the transcript, the rail and the room's own detail for the
 * socket's four in-flight queries. What bounds the cost now is what the
 * frames name: one page per touched message, the last page per new reply,
 * and every page only for an erase, a prune or frames this tab let go of.
 */

import { useCallback, useEffect } from "react";

import { Button, Skeleton } from "@crewlethq/ui";
import { XGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { ChatMessageView, ChatThreadAnswer } from "~/protocol/index.ts";
import type { Outgoing } from "./outbox.ts";

import { Composer } from "./Composer.tsx";
import { PendingMessage } from "./Pending.tsx";
import { useChatFrames } from "./live.ts";
import { MessageLinks } from "./MessageLinks.tsx";
import { threadPagesTouched } from "./messages.ts";
import { AuthorLine } from "./Transcript.tsx";
import { PAGE } from "./page.ts";

export interface ThreadPaneProps {
  channelID: string;
  rootID: string;
  onClose: () => void;
  onReplies: (replies: ChatMessageView[]) => void;
  send: (body: string) => void;
  nameOf: (handle: string) => string;
  /** Why the ROOM takes no replies from this reader, or "" — see `Composer`. */
  closed: string;
  /** Why this BROWSER cannot reply now, or "" — see `Composer`. */
  held: string;
  /** Replies this tab is still trying to say, drawn under the thread. */
  pending: Outgoing[];
  onRetry: (opId: string) => void;
  onDiscard: (opId: string) => void;
  viewer: string;
}

const repliesOn = (answer: ChatThreadAnswer) => answer.replies ?? [];
const replyID = (view: ChatMessageView) => view.message.id;
const nextOf = (answer: ChatThreadAnswer) => answer.next_cursor ?? "";

export function ThreadPane({
  channelID,
  rootID,
  onClose,
  onReplies,
  send,
  nameOf,
  closed,
  held,
  pending,
  onRetry,
  onDiscard,
  viewer,
}: ThreadPaneProps) {
  const { socket } = useClient();
  const asked = { channel_id: channelID, root_id: rootID, limit: PAGE };
  // THE FIRST PAGE, FROM THE ROOT, as an ordinary question — and the root
  // comes back with it, which is what keeps an edited or deleted root current.
  const first = useQuery("chat_thread", asked);
  /** One page after the first, at this tab's read floor as every ask is. */
  const page = useCallback(
    (cursor: string) =>
      socket.query(
        "chat_thread",
        withFloor(
          "chat_thread",
          JSON.stringify({ channel_id: channelID, root_id: rootID, limit: PAGE, cursor }),
        ),
      ) as Promise<ChatThreadAnswer>,
    [socket, channelID, rootID],
  );
  const thread = usePaged(first, asked, page, repliesOn, replyID, nextOf);

  // THE ROOM'S FRAMES drive this too — a reply is a message in the room —
  // but only the pages they name are asked again.
  useChatFrames(channelID, (frames) => thread.refresh(threadPagesTouched(rootID, frames)));

  useEffect(() => onReplies(thread.items), [thread.items, onReplies]);

  // THE FIRST PAGE AS DRAWN carries the root; the LAST carries who has
  // spoken here — every page carries the whole set, and the last is the one
  // a new reply makes this pane read again.
  const drawn = thread.answers;
  const root = drawn[0]?.root;
  const participants = drawn[drawn.length - 1]?.participants ?? [];

  return (
    <aside className="chat-thread" aria-label="Thread">
      <div className="row gap-2 chat-thread-head">
        <strong className="t-cell">Thread</strong>
        <span className="spacer" />
        <Button
          variant="ghost"
          onClick={onClose}
          leadingIcon={<XGlyph size="sm" />}
          aria-label="Close the thread"
        >
          Close
        </Button>
      </div>

      {thread.loading && !first.data && <Skeleton variant="text" rows={4} label="Reading" />}
      <QueryState
        error={thread.error}
        refusal={thread.refusal}
        detail={first.detail}
        loading={thread.loading}
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
            {thread.items.map((reply) => (
              <ThreadMessage key={reply.message.id} view={reply} nameOf={nameOf} />
            ))}
            {pending.map((row) => (
              <PendingMessage
                key={row.opId}
                row={row}
                viewer={viewer}
                nameOf={nameOf}
                onRetry={onRetry}
                onDiscard={onDiscard}
                blocked={closed || held}
              />
            ))}
            {/* WHY THE NEXT PAGE DID NOT COME, in the words every pager
                uses; what is on screen is still what this node holds. */}
            {thread.pageError && (
              <QueryState error={thread.pageError} refusal={null} loading={false} />
            )}
            {thread.more && (
              <Button variant="ghost" onClick={thread.next} loading={thread.paging}>
                More replies
              </Button>
            )}
            {participants.length > 0 && (
              <p className="t-caption muted">
                {participants.map((handle) => nameOf(handle)).join(", ")} have spoken here. Taking
                part in a thread means a reply reaches you — it obliges nothing.
              </p>
            )}
          </div>
        )}
      </QueryState>

      <Composer what="this thread" inThread send={send} closed={closed} held={held} />
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
      <AuthorLine message={message} nameOf={nameOf} />
      {deleted ? (
        <p className="chat-body muted">
          Deleted{message.deleted_by ? ` by ${nameOf(message.deleted_by)}` : ""}. The row stays so
          the thread keeps the message it hangs off.
        </p>
      ) : (
        <p className="chat-body">{message.body}</p>
      )}
      <MessageLinks links={message.links} />
    </div>
  );
}
