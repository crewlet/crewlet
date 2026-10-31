/**
 * A thread, beside the room it is in or beside the view it was opened from.
 *
 * # One level deep, by construction
 *
 * A reply to a reply carries the same root, which is what makes "this thread"
 * a range rather than a recursive walk, and it is why nothing in this pane
 * opens another thread. A reply here has no thread affordance of its own, not
 * because it is hidden but because there is nothing it could address: the
 * engine resolves a reply's root from the message being answered, so answering
 * a reply lands in this same thread.
 *
 * # It reads forwards
 *
 * The room pages backwards from its newest message, because that is where a
 * reader arrives. A thread is a conversation somebody is following from its
 * first line, so it pages forwards from the root, which is also why it is its
 * own question rather than a filter on the transcript: one question answering
 * both would make every caller branch on what came back.
 *
 * # Paged as every history is, and each page read again when a frame names it
 *
 * The pages are `usePaged`'s: the first an ordinary question from the root,
 * each after it asked once when the reader asks for more and held under the
 * cursor it was asked with. A page is RE-ASKED, under its own cursor, when a
 * live frame in the room names something it holds: the root, a reply on it,
 * or (for the last page) a new reply arriving at the end
 * ([threadPagesTouched]). Not every page on every frame: the engine bounds who
 * takes part in a thread and not how much they say, so a long thread asked a
 * query per page for every message anywhere in the room, competing with the
 * transcript and the rail for the socket's four in-flight queries.
 *
 * # The rows are the room's
 *
 * The root and every reply are the room's own message row, inside the room's
 * own scope (`scope.tsx`): the same actions, the same rules for each change,
 * the same outbox. The pane is opened from a view as often as from its room
 * (Threads, Activity, Saved), and the room's scope reads its detail itself
 * when it is not the open one, so a reply here is held by exactly the rule
 * that holds one in the room.
 *
 * # Following is the reader's
 *
 * A thread the reader started, replied in or was named in is followed without
 * a mark; a mark by hand decides either way (`ChatReadState.threads`). The
 * button says which, and replying in a thread the reader unfollowed follows it
 * again, as the engine asks of a screen that replies there.
 */

import { useCallback, useEffect, useMemo, useRef } from "react";

import {
  AvatarStack,
  Button,
  ChatDivider,
  ChatThread,
  Skeleton,
  ThinkingIndicator,
  type ChatThreadHandle,
} from "@crewlethq/ui";

import { QueryState } from "~/components/common.tsx";
import { WriteButton } from "~/components/WriteButton.tsx";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { andList } from "~/lib/words.ts";
import type { ChatMessageView, ChatReadState, ChatThreadAnswer } from "~/protocol/index.ts";

import { Composer } from "./Composer.tsx";
import { markRead, packPosition } from "./cursor.ts";
import { continues } from "./days.ts";
import { ChatFace, stackMember } from "./Face.tsx";
import { useChatRead } from "~/lib/chatLive.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { useChatFrames, useChatPresence } from "./live.ts";
import { MessageRow } from "./MessageRow.tsx";
import { PendingRow } from "./Pending.tsx";
import { threadPagesTouched } from "./messages.ts";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { PAGE } from "./page.ts";
import { ChatSidePanel } from "./PanePanel.tsx";
import { useChatPeople } from "./people.ts";
import { roomTitle } from "./rooms.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { RoomScopeProvider, useRoomScope } from "./scope.tsx";

const repliesOn = (answer: ChatThreadAnswer) => answer.replies ?? [];
const replyID = (view: ChatMessageView) => view.message.id;
const nextOf = (answer: ChatThreadAnswer) => answer.next_cursor ?? "";

export function ThreadPane({
  room,
  root,
  onClose,
}: {
  room: string;
  root: string;
  onClose: () => void;
}) {
  const detail = useRoomDetail(room);
  return (
    <RoomScopeProvider channelID={room} detail={detail}>
      <Thread room={room} root={root} onClose={onClose} />
    </RoomScopeProvider>
  );
}

/**
 * Whether the reader follows a thread: their mark where they made one, and
 * otherwise whether they started it, replied in it or were named in it.
 */
export function follows(
  read: ChatReadState | null | undefined,
  rootID: string,
  viewer: string,
  root: ChatMessageView | undefined,
  participants: readonly string[],
  replies: readonly ChatMessageView[],
): boolean {
  const mark = read?.threads?.find((entry) => entry.root_id === rootID);
  if (mark) return mark.following;
  if (!root || viewer === "") return false;
  const named = (view: ChatMessageView) => view.message.mentions?.includes(viewer) ?? false;
  return (
    root.message.author === viewer ||
    participants.includes(viewer) ||
    named(root) ||
    replies.some(named)
  );
}

function Thread({
  room,
  root: rootID,
  onClose,
}: {
  room: string;
  root: string;
  onClose: () => void;
}) {
  const nav = useChatNav();
  const frame = useChatFrame();
  const scope = useRoomScope();
  const people = useChatPeople();
  const { socket, store } = useClient();
  const log = useRef<ChatThreadHandle | null>(null);
  const asked = { channel_id: room, root_id: rootID, limit: PAGE };
  // THE FIRST PAGE, FROM THE ROOT, as an ordinary question; the root comes
  // back with it, which is what keeps an edited or deleted root current.
  const first = useQuery("chat_thread", asked);
  const page = useCallback(
    (cursor: string) =>
      socket.query(
        "chat_thread",
        withFloor("chat_thread", JSON.stringify({ ...asked, cursor })),
      ) as Promise<ChatThreadAnswer>,
    // `asked` is rebuilt each render from these two.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [socket, room, rootID],
  );
  const thread = usePaged(first, asked, page, repliesOn, replyID, nextOf);

  // THE ROOM'S FRAMES drive this too (a reply is a message in the room), but
  // only the pages they name are asked again.
  useChatFrames(room, (frames) => thread.refresh(threadPagesTouched(rootID, frames)));

  // THE FIRST PAGE AS DRAWN carries the root; the LAST carries who has spoken
  // here: every page carries the whole set, and the last is the one a new
  // reply makes this pane read again.
  const drawn = thread.answers;
  const rootView = drawn[0]?.root;
  const participants = drawn[drawn.length - 1]?.participants ?? [];
  const replies = thread.items;
  const count = rootView?.thread?.replies ?? replies.length;

  // THE THREAD'S READ PLACE moves to the newest reply on screen, through the
  // seat's one cursor writer, as the room's own place moves with the room.
  const newest = replies[replies.length - 1];
  useEffect(() => {
    if (newest) frame.cursors.seeThread(rootID, packPosition(newest.position));
  }, [frame.cursors, rootID, newest]);

  const read = useChatRead();
  const following = follows(read, rootID, frame.viewer, rootView, participants, replies);
  const unfollowed = read?.threads?.some((entry) => entry.root_id === rootID && !entry.following);
  const mark = useAct("mark_chat_read");
  const ref = { channel_id: room, root_id: rootID };
  const setFollow = (follow: boolean) =>
    void markRead(mark, store, follow ? { follow: [ref] } : { unfollow: [ref] }, {
      done: follow ? "Following the thread" : "Stopped following the thread",
      quiet: true,
    });

  const presence = useChatPresence(room);
  const working = (presence.working ?? []).filter((entry) => entry.thread === rootID);
  const pending = frame.outbox.rows.filter(
    (row) => row.channelID === room && row.threadRoot === rootID,
  );
  const reach = useMemo(
    () => participants.filter((handle) => handle !== frame.viewer),
    [participants, frame.viewer],
  );

  const summary = frame.channels.find((row) => row.channel.id === room);
  const channel = scope.channel ?? summary?.channel;
  const title = channel
    ? roomTitle(channel, {
        participants: summary?.participants,
        viewer: frame.viewer,
        nameOf: people.nameOf,
      })
    : "";
  const where = channel && !isDirect(channel.kind) ? `#${title}` : title;

  return (
    <ChatSidePanel
      title="Thread"
      subtitle={
        where && (
          <a className="chat-pane-where" href={nav.hrefOf({ kind: "room", id: room }, nav.pane)}>
            {where}
          </a>
        )
      }
      actions={
        rootView && (
          <WriteButton
            size="small"
            variant="ghost"
            pressed={following}
            write={mark}
            showRefusal={false}
            onPress={() => setFollow(!following)}
          >
            {following ? "Following" : "Follow"}
          </WriteButton>
        )
      }
      flush
      onClose={onClose}
      footer={
        <div className="chat-pane-foot">
          {scope.rules.post === "" ? (
            <Composer
              key={rootID}
              channelID={room}
              threadRoot={rootID}
              compact
              label="Reply in the thread"
              placeholder="Reply…"
              onSent={() => {
                // REPLYING AGAIN DOES NOT UNDO AN UNFOLLOW BY ITSELF: the
                // engine asks a screen that replies there to follow it.
                if (unfollowed) setFollow(true);
                log.current?.scrollToLatest();
              }}
            />
          ) : (
            <p className="chat-pane-note">{scope.rules.post}</p>
          )}
          {reach.length > 0 && (
            <p className="chat-pane-note chat-pane-note--lead">
              <AvatarStack
                members={reach.map((handle) => stackMember(people.person(handle)))}
                size="xs"
                max={4}
                decorative
              />
              <span>Replies reach {andList(reach.map(people.nameOf))}.</span>
            </p>
          )}
        </div>
      }
    >
      {thread.loading && !first.data && <Skeleton variant="text" rows={4} label="Reading" />}
      <QueryState
        error={thread.error}
        refusal={thread.refusal}
        detail={first.detail}
        loading={thread.loading}
        empty={
          rootView
            ? undefined
            : {
                title: "That message isn’t here",
                hint: "It was erased, or it’s older than the room keeps messages for.",
              }
        }
      >
        {rootView && (
          <ChatThread ref={log} label="Thread" className="chat-pane-transcript">
            <MessageRow view={rootView} where="root" />
            <ChatDivider
              variant="date"
              label={
                count === 0 ? "No replies yet" : `${count} ${count === 1 ? "reply" : "replies"}`
              }
            />
            {replies.map((view, index) => (
              <MessageRow
                key={view.message.id}
                view={view}
                where="thread"
                continuation={continues(replies[index - 1], view)}
              />
            ))}
            {pending.map((row) => (
              <PendingRow key={row.opId} row={row} />
            ))}
            {/* AN AGENT WRITING A REPLY, drawn as the reply on its way: its
                face in the avatar column, its name on the author line and
                what it is doing under it, so the name is never what an
                ellipsis cuts. */}
            {working.map((entry) => {
              const person = people.person(entry.handle);
              return (
                <div key={entry.handle} className="chat-pane-working">
                  <ChatFace person={person} size={36} />
                  <div className="chat-pane-working__text">
                    <span className="chat-pane-working__name">{person.name}</span>
                    <ThinkingIndicator
                      status={entry.status || "Writing a reply"}
                      label={`${person.name} is writing a reply`}
                    />
                  </div>
                </div>
              );
            })}
            {/* WHY THE NEXT PAGE DID NOT COME, in the words every pager
                uses; what is on screen is still what this node holds. */}
            {thread.pageError && (
              <QueryState error={thread.pageError} refusal={null} loading={false} />
            )}
            {thread.more && (
              <Button variant="ghost" size="small" onClick={thread.next} loading={thread.paging}>
                More replies
              </Button>
            )}
          </ChatThread>
        )}
      </QueryState>
    </ChatSidePanel>
  );
}
