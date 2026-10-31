/**
 * The views beside the rooms: Activity (a list and a reading pane), Threads,
 * Drafts and sent, Saved, and New message. Search and Browse rooms have files
 * of their own.
 *
 * # Every list is the engine's answer about the reader
 *
 * Activity, Threads, Saved and Sent are the reader's own views
 * (`chat_activity`, `chat_threads`, `chat_saved`, `chat_sent`), each read only
 * over rooms the reader may read and each paged as every history is
 * (`usePaged`). What a row draws is the engine's: who did what, where, and
 * when. Nothing here decides what reaches the reader; the reader's marks
 * (follow, save, the notification level) are written through `mark_chat_read`
 * and come back as their read state, which the lists are read again on.
 *
 * # Quoted messages are the room's rows, read-only
 *
 * A card in Threads or Saved quotes a message as the room draws it, through
 * the room's own row in its read-only form (`where="context"`): a card is a
 * pointer to a conversation, and its actions are a press away in the thread
 * or the room, where the room's rules for each change are read once. A list
 * of twenty cards from a dozen rooms would otherwise read a dozen rooms'
 * details to offer a reaction nobody came to the list to make. Activity's
 * reading pane is the exception, since it is one message read in place with a
 * reply box under it, so it reads its one room and holds the full row.
 */

import { useId, useMemo, useRef, useState, type ReactNode } from "react";

import {
  Button,
  ChipField,
  EmptyState,
  IconButton,
  InboxList,
  InboxRow,
  Input,
  ListDetail,
  PersonRow,
  Skeleton,
  Switch,
  TabPanel,
  Tabs,
  Tag,
} from "@crewlethq/ui";
import { AtSignGlyph, MessagesSquareGlyph, SendGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useNow } from "~/lib/clock.ts";
import { useRoomNames } from "~/lib/roomNames.ts";
import { relTime } from "~/lib/format.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor, type QueryResult } from "~/lib/useQuery.ts";
import type { ChatActivityItem, ChatActivityKind, ChatKind, QueryMap } from "~/protocol/index.ts";

import { bodyExcerpt } from "./Body.tsx";
import { Composer } from "./Composer.tsx";
import { markRead } from "./cursor.ts";
import { continues } from "./days.ts";
import { SuggestedPerson } from "./Dialogs.tsx";
import { useChatDrafts } from "./drafts.ts";
import { ChatFace } from "./Face.tsx";
import { useChatActivity, useChatRead, useCoalesced } from "~/lib/chatLive.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { MessageRow } from "./MessageRow.tsx";
import { PendingRow } from "./Pending.tsx";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { matchesPerson, useChatPeople } from "./people.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { roomMark, roomTitle } from "./rooms.ts";
import { RoomScopeProvider, useRoomScope } from "./scope.tsx";
import { reportFor } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

/** How many rows a view's page holds: the reader's views are short lists. */
const VIEW_PAGE = 20;

/* Shared parts --------------------------------------------------------- */

/**
 * A view's head: its title, which takes focus when the card that had it is
 * removed, and the view's own tools.
 */
export function ViewHead({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <header className="chat-view-head">
      <h1 className="chat-view-head__title" tabIndex={-1}>
        {title}
      </h1>
      {children && <div className="chat-view-head__tools">{children}</div>}
    </header>
  );
}

/** A room as a reader view names it: its id, kind and, for a named room, its name. */
export interface RoomRef {
  id: string;
  kind: ChatKind;
  name?: string | undefined;
}

/**
 * What a room is called in a view: the rooms module's one titling rule, with a
 * direct conversation titled by its people as the reader's rail holds them.
 */
export function useRoomWords(): {
  title: (room: RoomRef) => string;
  where: (room: RoomRef) => string;
} {
  const frame = useChatFrame();
  return useMemo(() => {
    const title = (room: RoomRef) => {
      const summary = frame.channels.find((row) => row.channel.id === room.id);
      return roomTitle(room, {
        participants: summary?.participants,
        viewer: frame.viewer,
        nameOf: frame.nameOf,
      });
    };
    return {
      title,
      where: (room: RoomRef) => (isDirect(room.kind) ? title(room) : `#${title(room)}`),
    };
  }, [frame.channels, frame.viewer, frame.nameOf]);
}

/**
 * A room by its id alone, as a draft or an unsent message names one: the
 * reader's own rooms first, then the names of every room they may read
 * (`lib/roomNames.ts`). A room neither holds is still somewhere the reader
 * wrote, so it is drawn as "a room" rather than as its id.
 */
function useRoomRef(): (id: string) => RoomRef {
  const frame = useChatFrame();
  const names = useRoomNames();
  return (id) => {
    const summary = frame.channels.find((row) => row.channel.id === id);
    if (summary) return { id, kind: summary.channel.kind, name: summary.channel.name };
    // THE NAMES ARE KEYED BY NAME, and carry no kind: the room is drawn under
    // its name with the mark of a room whose kind is not in hand.
    for (const [name, named] of names) if (named === id) return { id, kind: "", name };
    return { id, kind: "", name: "a room" };
  };
}

/** A room's mark and title, as a card's head names it. */
function RoomName({ room }: { room: RoomRef }) {
  const words = useRoomWords();
  return (
    <span className="chat-context__where">
      <NamedGlyph name={roomMark(room.kind)} size="sm" />
      <span className="chat-context__name">{words.title(room)}</span>
    </span>
  );
}

/**
 * Removing a card (Unfollow, Remove) takes the pressed button with it, and
 * focus with it to the page body. Called from the press, while the button
 * still holds focus: focus moves to the same button on the card that takes its
 * place, or the one before it, or to the view's title when the list is empty.
 */
function handOff(button: Element | null = document.activeElement) {
  if (!(button instanceof HTMLElement)) return;
  const card = button.closest<HTMLElement>("[data-card]");
  const sibling = (
    from: Element | null | undefined,
    step: "nextElementSibling" | "previousElementSibling",
  ) => {
    let next = from?.[step] ?? null;
    while (next && !next.matches("[data-card]")) next = next[step];
    return next;
  };
  const next = sibling(card, "nextElementSibling") ?? sibling(card, "previousElementSibling");
  const view = button.closest(".chat-view");
  const label = button.textContent;
  window.requestAnimationFrame(() => {
    const same = next?.isConnected
      ? [...next.querySelectorAll<HTMLElement>("button")].find((b) => b.textContent === label)
      : undefined;
    (same ?? view?.querySelector<HTMLElement>(".chat-view-head__title"))?.focus();
  });
}

/**
 * One of the reader's paged views: its first page as an ordinary question,
 * each after it on request, all read again when the socket sees chat move or
 * the reader's own read state does (a follow, a save, a cursor).
 *
 * THE CALLER ASKS, BY NAME: each view spells its own question in its own
 * `useQuery` and `socket.query` (`app/source.test.ts` refuses a question
 * handed through a variable, and the engine's reader gate reads the literal),
 * and hands both here to be paged and kept live.
 */
function useReaderPages<A extends { next_cursor?: string }, T>(
  first: QueryResult<A>,
  params: Record<string, unknown>,
  page: (cursor: string) => Promise<A>,
  pick: (answer: A) => readonly T[],
  idOf: (entry: T) => string,
) {
  const paged = usePaged(first, params, page, pick, idOf, (answer) => answer.next_cursor ?? "");
  useCoalesced(useChatActivity(), paged.refresh);
  useCoalesced(useChatRead()?.revision ?? 0, paged.refresh);
  return { ...paged, first };
}

/** The cursor a later page of a reader's view is asked from, beside its first page's words. */
const after = (params: Record<string, unknown>, cursor: string) =>
  JSON.stringify({ ...params, cursor });

/** The pager's foot: why a page did not come, and More. */
function PageFoot({
  paged,
  more,
}: {
  paged: {
    pageError: Parameters<typeof QueryState>[0]["error"];
    more: boolean;
    next: () => void;
    paging: boolean;
  };
  more: string;
}) {
  return (
    <>
      {paged.pageError && <QueryState error={paged.pageError} refusal={null} loading={false} />}
      {paged.more && (
        <Button variant="ghost" size="small" onClick={paged.next} loading={paged.paging}>
          {more}
        </Button>
      )}
    </>
  );
}

/* Activity ------------------------------------------------------------- */

type ActivityFilter = "all" | ChatActivityKind;

const EMPTY_ACTIVITY: Record<ActivityFilter, string> = {
  all: "Nothing here yet",
  mention: "No mentions yet",
  reply: "No replies yet",
  reaction: "No reactions yet",
};

/**
 * Mentions, replies in threads the reader follows and reactions to their
 * messages, newest first, beside a reading pane that shows the one chosen as
 * the room draws it, with a reply box under it.
 *
 * A NARROW COLUMN SHOWS NO READING PANE, so a row there opens where it was
 * said rather than being chosen out of sight.
 */
export function ActivityView() {
  const nav = useChatNav();
  const now = useNow();
  const people = useChatPeople();
  const words = useRoomWords();
  const { socket, store } = useClient();
  const mark = useAct("mark_chat_read");
  const panel = useId();
  const reading = useRef<HTMLDivElement>(null);
  const [filter, setFilter] = useState<ActivityFilter>("all");
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [selected, setSelected] = useState("");
  const params = {
    ...(filter === "all" ? {} : { kind: filter }),
    ...(unreadOnly ? { unread: true } : {}),
    limit: VIEW_PAGE,
  };
  const feed = useReaderPages(
    useQuery("chat_activity", params),
    params,
    (cursor) =>
      socket.query("chat_activity", withFloor("chat_activity", after(params, cursor))) as Promise<
        QueryMap["chat_activity"]
      >,
    (answer) => answer.items,
    (item) => item.key,
  );
  const counts = feed.first.data?.unread;
  const items = feed.items;
  // THE READING PANE SHOWS WHAT THE LIST SHOWS: an item the filter hides is
  // not left open beside a list that no longer has it.
  const current = items.find((item) => item.key === selected) ?? items[0];

  const open = (item: ChatActivityItem) =>
    item.thread_root
      ? nav.openThread(item.channel_id, item.thread_root)
      : nav.openRoom(item.channel_id, { message: item.message_id });

  const choose = (item: ChatActivityItem) => {
    setSelected(item.key);
    // MARKED READ WHERE IT IS READ: the row's own position, sent as its room's
    // cursor, which only moves forward.
    if (item.unread && item.read_through !== undefined) {
      void markRead(
        mark,
        store,
        { cursors: { [item.channel_id]: item.read_through } },
        {
          done: "Marked as read",
          quiet: true,
        },
      );
    }
    if (reading.current?.offsetParent === null) open(item);
  };

  const line = (item: ChatActivityItem) => {
    const where = words.where({
      id: item.channel_id,
      kind: item.channel_kind,
      name: item.channel_name,
    });
    if (item.kind === "mention")
      return `${item.collective ? "notified everyone" : "mentioned you"} in ${where}`;
    if (item.kind === "reply") return `replied to a thread in ${where}`;
    return `reacted ${item.emoji ?? ""} to your message in ${where}`;
  };

  const empty = unreadOnly
    ? {
        title: "You’re all caught up",
        description: "Nothing here is unread. Turn off Unread only to see everything again.",
      }
    : {
        title: EMPTY_ACTIVITY[filter],
        description:
          "Mentions, replies in threads you follow and reactions to your messages show here.",
      };

  return (
    <section className="chat-view">
      <ViewHead title="Activity">
        <Switch label="Unread only" checked={unreadOnly} onCheckedChange={setUnreadOnly} />
      </ViewHead>
      <Tabs
        className="chat-view-tabs"
        variant="underline"
        size="sm"
        ariaLabel="Show"
        panelId={panel}
        value={filter}
        onValueChange={(value) => setFilter(value as ActivityFilter)}
        items={[
          { value: "all", label: "All" },
          { value: "mention", label: "Mentions", count: counts?.mention || null },
          { value: "reply", label: "Threads", count: counts?.reply || null },
          { value: "reaction", label: "Reactions" },
        ]}
      />
      <TabPanel id={panel} value={filter}>
        <QueryState
          error={feed.error}
          refusal={feed.refusal}
          detail={feed.first.detail}
          loading={feed.loading}
        >
          {items.length === 0 ? (
            <EmptyState size="compact" title={empty.title} description={empty.description} />
          ) : (
            <ListDetail
              className="chat-activity"
              list={
                <>
                  <InboxList label="Activity">
                    {items.map((item) => {
                      const actor = people.person(item.actor, item.actor_kind);
                      return (
                        <InboxRow
                          key={item.key}
                          title={actor.name}
                          qualifier={line(item)}
                          meta={relTime(item.at, now)}
                          leading={
                            <ChatFace
                              person={actor}
                              size={32}
                              status={false}
                              mark={activityMark(item)}
                              markLabel={
                                item.kind === "reaction"
                                  ? "Reaction"
                                  : item.kind === "mention"
                                    ? "Mention"
                                    : "Thread reply"
                              }
                            />
                          }
                          selected={current?.key === item.key}
                          unread={item.unread}
                          unreadLabel="Unread"
                          onClick={() => choose(item)}
                        >
                          {bodyExcerpt(item.excerpt)}
                        </InboxRow>
                      );
                    })}
                  </InboxList>
                  <PageFoot paged={feed} more="Older activity" />
                </>
              }
            >
              <div className="chat-activity-context" ref={reading}>
                {current && (
                  <ActivityContext key={current.key} item={current} onOpen={() => open(current)} />
                )}
              </div>
            </ListDetail>
          )}
        </QueryState>
      </TabPanel>
    </section>
  );
}

/** The mark on an activity row's face: what kind of activity it is. */
function activityMark(item: ChatActivityItem): ReactNode {
  if (item.kind === "mention") return <AtSignGlyph size="xs" />;
  if (item.kind === "reply") return <MessagesSquareGlyph size="xs" />;
  return item.emoji ?? "";
}

/**
 * The chosen activity, read in place: the message as the room draws it (its
 * thread's root and the replies just before it, for a reply), and a reply box
 * where the room takes one from the reader.
 */
function ActivityContext({ item, onOpen }: { item: ChatActivityItem; onOpen: () => void }) {
  const detail = useRoomDetail(item.channel_id);
  const read = useQuery("chat_message", {
    channel_id: item.channel_id,
    message_id: item.message_id,
  });
  return (
    <RoomScopeProvider channelID={item.channel_id} detail={detail}>
      <div className="chat-context">
        <header className="chat-context__head">
          <span className="chat-context__where">
            <RoomName
              room={{ id: item.channel_id, kind: item.channel_kind, name: item.channel_name }}
            />
            {item.thread_root && <Tag size="xs">Thread</Tag>}
          </span>
          <Button size="small" variant="secondary" onClick={onOpen}>
            Open in {item.thread_root ? "thread" : "room"}
          </Button>
        </header>
        <QueryState
          error={read.error}
          refusal={read.refusal}
          detail={read.detail}
          loading={read.loading}
          empty={
            read.data
              ? undefined
              : { title: "That message isn’t here", hint: "It was taken back or erased since." }
          }
        >
          {read.data && (
            <div className="chat-context__body" data-transcript>
              {read.data.root && <MessageRow view={read.data.root} where="root" />}
              {[...(read.data.before ?? []), read.data.view].map((view, index, rows) => (
                <MessageRow
                  key={view.message.id}
                  view={view}
                  where={read.data?.root ? "thread" : "room"}
                  continuation={continues(rows[index - 1], view)}
                />
              ))}
            </div>
          )}
        </QueryState>
        <ActivityReply item={item} />
      </div>
    </RoomScopeProvider>
  );
}

/** The reply box under an activity, where the room takes one. */
function ActivityReply({ item }: { item: ChatActivityItem }) {
  const scope = useRoomScope();
  const words = useRoomWords();
  if (scope.rules.post !== "") return null;
  const where = words.where({
    id: item.channel_id,
    kind: item.channel_kind,
    name: item.channel_name,
  });
  return (
    <div className="chat-context__reply">
      <Composer
        channelID={item.channel_id}
        {...(item.thread_root ? { threadRoot: item.thread_root } : {})}
        compact
        label={item.thread_root ? "Reply in the thread" : `Message ${where}`}
        placeholder={item.thread_root ? "Reply in the thread…" : `Reply in ${where}`}
      />
    </div>
  );
}

/* Threads -------------------------------------------------------------- */

/** Every thread the reader follows, newest reply first. */
export function ThreadsView() {
  const nav = useChatNav();
  const now = useNow();
  const { socket, store } = useClient();
  const mark = useAct("mark_chat_read");
  const params = { limit: VIEW_PAGE };
  const threads = useReaderPages(
    useQuery("chat_threads", params),
    params,
    (cursor) =>
      socket.query("chat_threads", withFloor("chat_threads", after(params, cursor))) as Promise<
        QueryMap["chat_threads"]
      >,
    (answer) => answer.threads,
    (thread) => thread.root.message.id,
  );

  return (
    <section className="chat-view">
      <ViewHead title="Threads" />
      <div className="chat-view-scroll">
        <QueryState
          error={threads.error}
          refusal={threads.refusal}
          detail={threads.first.detail}
          loading={threads.loading}
          empty={
            threads.items.length === 0
              ? {
                  title: "No threads yet",
                  hint: "Threads you start, reply in or follow are listed here, newest reply first.",
                }
              : undefined
          }
        >
          {threads.items.map((thread) => {
            const root = thread.root;
            const summary = root.thread;
            const replies = summary?.replies ?? thread.latest.length;
            const hidden = replies - thread.latest.length;
            const room: RoomRef = {
              id: thread.channel_id,
              kind: thread.channel_kind,
              name: thread.channel_name,
            };
            return (
              <article key={root.message.id} className="chat-thread-card" data-card>
                <header className="chat-thread-card__head">
                  <span className="chat-context__where">
                    <RoomName room={room} />
                    <span className="chat-muted-text">
                      · {replies} {replies === 1 ? "reply" : "replies"} · last{" "}
                      {relTime(summary?.last_reply_at ?? root.message.created_at, now)}
                    </span>
                  </span>
                  <WriteButton
                    size="small"
                    variant="ghost"
                    write={mark}
                    showRefusal={false}
                    onPress={() => {
                      handOff();
                      void markRead(
                        mark,
                        store,
                        { unfollow: [{ channel_id: thread.channel_id, root_id: root.message.id }] },
                        { done: "Stopped following the thread", quiet: true },
                      );
                    }}
                  >
                    Unfollow
                  </WriteButton>
                </header>
                <RoomScopeProvider channelID={thread.channel_id} detail={null}>
                  <div data-transcript>
                    <MessageRow view={root} where="context" />
                    {hidden > 0 && (
                      <button
                        type="button"
                        className="chat-more-replies"
                        onClick={() => nav.openThread(thread.channel_id, root.message.id)}
                      >
                        {hidden} more {hidden === 1 ? "reply" : "replies"}
                      </button>
                    )}
                    {thread.latest.map((view, index) => (
                      <MessageRow
                        key={view.message.id}
                        view={view}
                        where="context"
                        continuation={continues(thread.latest[index - 1], view)}
                      />
                    ))}
                  </div>
                </RoomScopeProvider>
                <div className="chat-thread-card__foot">
                  <Button
                    size="small"
                    variant="secondary"
                    leadingIcon={<MessagesSquareGlyph size="sm" />}
                    onClick={() => nav.openThread(thread.channel_id, root.message.id)}
                  >
                    Reply
                  </Button>
                </div>
              </article>
            );
          })}
          <PageFoot paged={threads} more="Older threads" />
        </QueryState>
      </div>
    </section>
  );
}

/* Drafts and sent -------------------------------------------------------- */

/**
 * What the reader has not sent yet (a message the engine did not confirm, and
 * the words left in a box), and what they did send, in every room.
 *
 * A DRAFT IS THIS TAB'S, never the engine's: it lives in the screen's drafts
 * store (`drafts.ts`) for as long as the tab does, and is never written to the
 * address or to browser storage.
 */
export function DraftsView() {
  const nav = useChatNav();
  const now = useNow();
  const frame = useChatFrame();
  const drafts = useChatDrafts();
  const roomOf = useRoomRef();
  const panel = useId();
  const [tab, setTab] = useState<"drafts" | "sent">("drafts");
  const unsent = frame.outbox.rows.filter((row) => row.state !== "sending");
  const kept = drafts.list();
  const { socket } = useClient();
  const params = { limit: VIEW_PAGE };
  const sent = useReaderPages(
    useQuery("chat_sent", params),
    params,
    (cursor) =>
      socket.query("chat_sent", withFloor("chat_sent", after(params, cursor))) as Promise<
        QueryMap["chat_sent"]
      >,
    (answer) => answer.messages,
    (entry) => entry.view.message.id,
  );

  return (
    <section className="chat-view">
      <ViewHead title="Drafts & sent" />
      <Tabs
        className="chat-view-tabs"
        variant="underline"
        size="sm"
        ariaLabel="Show"
        panelId={panel}
        value={tab}
        onValueChange={(value) => setTab(value as "drafts" | "sent")}
        items={[
          {
            value: "drafts",
            label: "Drafts and unsent",
            count: kept.length + unsent.length || null,
          },
          { value: "sent", label: "Sent" },
        ]}
      />
      <TabPanel id={panel} value={tab}>
        <div className="chat-view-scroll">
          {tab === "drafts" && (
            <>
              {unsent.length > 0 && <h2 className="chat-group-label">Not sent</h2>}
              {unsent.map((row) => (
                <div key={row.opId} className="chat-list-card" data-transcript>
                  <header className="chat-thread-card__head">
                    <RoomName room={roomOf(row.channelID)} />
                  </header>
                  <RoomScopeProvider channelID={row.channelID} detail={null}>
                    <PendingRow row={row} />
                  </RoomScopeProvider>
                </div>
              ))}
              {kept.length > 0 && <h2 className="chat-group-label">Drafts</h2>}
              {kept.map((draft) => (
                <button
                  key={`${draft.room}/${draft.root ?? ""}`}
                  type="button"
                  className="chat-draft"
                  onClick={() =>
                    draft.root ? nav.openThread(draft.room, draft.root) : nav.openRoom(draft.room)
                  }
                >
                  <span className="chat-context__where">
                    <RoomName room={roomOf(draft.room)} />
                    {draft.root && <Tag size="xs">Thread</Tag>}
                  </span>
                  <span className="chat-draft__text">{bodyExcerpt(draft.text)}</span>
                </button>
              ))}
              {unsent.length + kept.length === 0 && (
                <EmptyState
                  title="Nothing waiting"
                  description="A message you start and leave is kept here per room, and a message the engine didn’t confirm waits here with Retry."
                />
              )}
            </>
          )}
          {tab === "sent" && (
            <QueryState
              error={sent.error}
              refusal={sent.refusal}
              detail={sent.first.detail}
              loading={sent.loading}
              empty={
                sent.items.length === 0
                  ? {
                      title: "Nothing sent yet",
                      hint: "Messages you send in any room are listed here, newest first.",
                    }
                  : undefined
              }
            >
              {sent.items.map((entry) => {
                const room: RoomRef = {
                  id: entry.view.message.channel_id,
                  kind: entry.channel_kind,
                  name: entry.channel_name,
                };
                const message = entry.view.message;
                return (
                  <button
                    key={message.id}
                    type="button"
                    className="chat-draft"
                    onClick={() =>
                      message.thread_root
                        ? nav.openThread(room.id, message.thread_root)
                        : nav.openRoom(room.id, { message: message.id })
                    }
                  >
                    <span className="chat-context__where">
                      <RoomName room={room} />
                      <span className="chat-muted-text">· {relTime(message.created_at, now)}</span>
                    </span>
                    <span className="chat-draft__text">{bodyExcerpt(message.body)}</span>
                  </button>
                );
              })}
              <PageFoot paged={sent} more="Older messages" />
            </QueryState>
          )}
        </div>
      </TabPanel>
    </section>
  );
}

/* Saved ---------------------------------------------------------------- */

/** The messages the reader saved for later, the newest save first. */
export function SavedView() {
  const nav = useChatNav();
  const { socket, store } = useClient();
  const mark = useAct("mark_chat_read");
  const params = { limit: VIEW_PAGE };
  const saved = useReaderPages(
    useQuery("chat_saved", params),
    params,
    (cursor) =>
      socket.query("chat_saved", withFloor("chat_saved", after(params, cursor))) as Promise<
        QueryMap["chat_saved"]
      >,
    (answer) => answer.saved,
    (entry) => `${entry.mark.channel_id}:${entry.mark.message_id}`,
  );

  const unsave = (channel: string, message: string) =>
    void markRead(
      mark,
      store,
      { unsave: [{ channel_id: channel, message_id: message }] },
      {
        done: "Removed from Saved",
        quiet: true,
      },
    );

  return (
    <section className="chat-view">
      <ViewHead title="Saved" />
      <div className="chat-view-scroll">
        <QueryState
          error={saved.error}
          refusal={saved.refusal}
          detail={saved.first.detail}
          loading={saved.loading}
          empty={
            saved.items.length === 0
              ? {
                  title: "Nothing saved",
                  hint: "Save a message from its toolbar (or press S on it) to keep it here.",
                }
              : undefined
          }
        >
          {saved.items.map((entry) => {
            const { channel_id: channel, message_id: message } = entry.mark;
            const room: RoomRef = {
              id: channel,
              kind: entry.channel_kind ?? "",
              name: entry.channel_name,
            };
            const thread = entry.view?.message.thread_root;
            return (
              <div key={`${channel}:${message}`} className="chat-list-card" data-card>
                <header className="chat-thread-card__head">
                  <RoomName room={room} />
                  <span className="chat-list-card__actions">
                    {!entry.gone && (
                      <Button
                        size="small"
                        variant="ghost"
                        onClick={() =>
                          thread
                            ? nav.openThread(channel, thread)
                            : nav.openRoom(channel, { message })
                        }
                      >
                        Open
                      </Button>
                    )}
                    <WriteButton
                      size="small"
                      variant="ghost"
                      write={mark}
                      showRefusal={false}
                      onPress={() => {
                        handOff();
                        unsave(channel, message);
                      }}
                    >
                      Remove
                    </WriteButton>
                  </span>
                </header>
                {entry.view && !entry.gone ? (
                  <RoomScopeProvider channelID={channel} detail={null}>
                    <MessageRow view={entry.view} where="context" />
                  </RoomScopeProvider>
                ) : (
                  <p className="chat-pane-note chat-list-card__gone">
                    This message isn&rsquo;t there any more: it was taken back, erased, or is in a
                    room you can no longer read.
                  </p>
                )}
              </div>
            );
          })}
          <PageFoot paged={saved} more="Older saves" />
        </QueryState>
      </div>
    </section>
  );
}

/* New message ---------------------------------------------------------- */

/**
 * A new message: who it is to, the conversation it lands in, and the words.
 *
 * A CONVERSATION IS ITS PEOPLE. A direct conversation's id is derived from who
 * is in it, so the people chosen either name one the reader already has (its
 * last few messages are shown, and sending adds to it) or name a new one,
 * which sending opens (`open_dm`) and then posts to. Nothing is opened until
 * there is something to say: an empty conversation is a room nobody asked for.
 * `#` in the field finds one of the reader's rooms instead, which opens.
 */
export function NewMessageView() {
  const nav = useChatNav();
  const frame = useChatFrame();
  const people = useChatPeople();
  const open = useAct("open_dm");
  const initial = nav.place.kind === "new" ? (nav.place.to ?? []) : [];
  const [to, setTo] = useState<string[]>(() => [...initial]);
  const [q, setQ] = useState("");
  const [body, setBody] = useState("");
  const [report, setReport] = useState<ReturnType<typeof reportFor> | null>(null);
  const field = useRef<HTMLInputElement>(null);

  const others = people.everyone.filter(
    (person) => person.handle !== frame.viewer && !to.includes(person.handle),
  );
  const roomQuery = q.startsWith("#") ? q.slice(1).toLowerCase() : null;
  const matches = roomQuery === null ? others.filter((person) => matchesPerson(person, q)) : [];
  const rooms =
    roomQuery === null
      ? []
      : frame.channels
          .map((row) => row.channel)
          .filter((channel) => !isDirect(channel.kind) && (channel.name ?? "").includes(roomQuery));
  // THE CONVERSATION THESE PEOPLE ALREADY HAVE, from the reader's own rooms:
  // the same set of people, the reader among them.
  const wanted = [...new Set([frame.viewer, ...to])].sort().join(",");
  const existing =
    to.length > 0
      ? frame.channels.find(
          (row) =>
            isDirect(row.channel.kind) &&
            [...new Set(row.participants ?? [])].sort().join(",") === wanted,
        )
      : undefined;
  const names = to.map(people.nameOf);
  const blocked =
    to.length === 0
      ? "Choose who it is to first"
      : body.trim() === ""
        ? "Write a message first"
        : undefined;

  async function send() {
    if (blocked) return;
    const words = body.trim();
    let room = existing?.channel.id ?? "";
    if (!room) {
      if (!pressable(open)) return;
      setReport(null);
      const result = await open.run({ to: to }, { done: "Opened the conversation", quiet: true });
      if (result === null) return;
      const answer = reportFor("direct", result);
      if (!answer.go) {
        setReport(answer);
        return;
      }
      room = answer.go;
    }
    // THE OUTBOX SAYS IT, as every message is said: a press the engine does
    // not confirm waits in Drafts & sent with Retry.
    void frame.outbox.send({ channelID: room, body: words });
    setBody("");
    nav.openRoom(room);
  }

  return (
    <section className="chat-view">
      <ViewHead title="New message" />
      <ChipField
        variant="bare"
        prefix="To:"
        label="Who the message is to"
        inputRef={field}
        autoFocus
        placeholder={to.length ? "Add someone" : "A person, an agent, or #room"}
        query={q}
        onQueryChange={setQ}
        items={to.map((handle) => {
          const person = people.person(handle);
          return {
            id: handle,
            label: person.name,
            leading: <ChatFace person={person} size="xs" status={false} />,
          };
        })}
        onRemove={(handle) => setTo((list) => list.filter((h) => h !== handle))}
        onCommit={() => {
          const first = matches[0];
          if (first) {
            setTo((list) => [...list, first.handle]);
            setQ("");
          }
        }}
      />
      <div className="chat-view-scroll">
        {(q !== "" || to.length === 0) && (
          <ul className="chat-pick-list">
            {rooms.map((channel) => (
              <li key={channel.id}>
                <PersonRow
                  avatar={<NamedGlyph name={roomMark(channel.kind)} size="md" />}
                  name={channel.name ?? channel.id}
                  detail={channel.topic || undefined}
                  onSelect={() => nav.openRoom(channel.id)}
                />
              </li>
            ))}
            {matches.map((person) => (
              <li key={person.handle}>
                <SuggestedPerson
                  person={person}
                  onSelect={() => {
                    // THE ROW GOES once it is chosen, so focus goes back to the field.
                    setTo((list) => [...list, person.handle]);
                    setQ("");
                    field.current?.focus();
                  }}
                />
              </li>
            ))}
          </ul>
        )}
        {to.length > 0 && q === "" && (
          <div className="chat-new-preview">
            {existing ? (
              <>
                <p className="chat-pane-note">
                  You already have a conversation with {names.join(", ")}. Sending adds to it.
                </p>
                <LastMessages channelID={existing.channel.id} />
              </>
            ) : (
              <p className="chat-pane-note">
                This starts a new conversation with {names.join(", ")}.
              </p>
            )}
          </div>
        )}
        <StartNote report={report} />
      </div>
      <footer className="chat-view-foot">
        <div className="chat-new-compose">
          <Input
            aria-label="Message"
            width="full"
            placeholder={to.length ? `Message ${names.join(", ")}` : "Choose who it is to first"}
            value={body}
            disabled={to.length === 0}
            onChange={(event) => setBody(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                void send();
              }
            }}
          />
          <IconButton
            label="Send"
            variant="primary"
            icon={<SendGlyph size="sm" />}
            disabledReason={
              blocked ?? (frame.outbox.post.can ? undefined : frame.outbox.post.reason)
            }
            onClick={() => void send()}
          />
        </div>
      </footer>
    </section>
  );
}

/** The newest few messages of a conversation, read-only, as a preview. */
function LastMessages({ channelID }: { channelID: string }) {
  const page = useQuery("chat_messages", { channel_id: channelID, limit: 3 });
  const rows = [...(page.data?.messages ?? [])].reverse();
  if (page.loading) return <Skeleton variant="text" rows={2} label="Reading the conversation" />;
  return (
    <RoomScopeProvider channelID={channelID} detail={null}>
      <div data-transcript>
        {rows.map((view, index) => (
          <MessageRow
            key={view.message.id}
            view={view}
            where="context"
            continuation={continues(rows[index - 1], view)}
          />
        ))}
      </div>
    </RoomScopeProvider>
  );
}
