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
  InboxGroup,
  InboxList,
  InboxRow,
  Input,
  ListDetail,
  MessageAction,
  MessageActions,
  PersonRow,
  Skeleton,
  Switch,
  TabPanel,
  Tabs,
  Tag,
  Tooltip,
} from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  AtSignGlyph,
  CheckGlyph,
  FaceSlightlySmilingGlyph,
  MessagesSquareGlyph,
  SendGlyph,
} from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { usePageCoverage } from "~/app/Shell.tsx";
import { MAX_DIRECT_PARTICIPANTS } from "~/contract/chat.ts";
import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useClock, useNow } from "~/lib/clock.ts";
import { useRoomNames } from "~/lib/roomNames.ts";
import { readerDay, relTime } from "~/lib/format.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { focusPageHeading } from "~/lib/wizardFocus.ts";
import { useQuery, withFloor, type QueryResult } from "~/lib/useQuery.ts";
import type {
  ChatActivityItem,
  ChatActivityKind,
  ChatKind,
  ChatServed,
  QueryMap,
} from "~/protocol/index.ts";

import { bodyExcerpt, MessageLine } from "./Body.tsx";
import { Composer } from "./Composer.tsx";
import { markRead } from "./cursor.ts";
import { continues, dayLabel, dayOf, timeOf } from "./days.ts";
import { SuggestedPerson } from "./Dialogs.tsx";
import { useChatDrafts } from "./drafts.ts";
import { glyphOf, reactionShown } from "./emoji.ts";
import { ChatFace } from "./Face.tsx";
import { useChatActivity, useChatRead, useCoalesced } from "~/lib/chatLive.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { MessageRow } from "./MessageRow.tsx";
import { ChatDetailPlacement, ChatSidePanel } from "./PanePanel.tsx";
import { PendingRow } from "./Pending.tsx";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { matchesPerson, useChatPeople } from "./people.ts";
import { coverageOf } from "./page.ts";
import { useRoomDetail } from "./roomDetail.ts";
import { isConversationWith, roomMark, roomTitle, roomWhere } from "./rooms.ts";
import { RoomScopeProvider, useRoomScope } from "./scope.tsx";
import { reportFor } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { ThreadPane } from "./Thread.tsx";
import { ViewHead, useViewBarTight } from "./ViewHead.tsx";

/** How many rows a view's page holds: the reader's views are short lists. */
const VIEW_PAGE = 20;

/* Shared parts --------------------------------------------------------- */

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
      where: (room: RoomRef) => roomWhere(room.kind, title(room)),
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
 * place, or the one before it, or to the page's heading (the trail's last
 * crumb) when the list is empty.
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
  const label = button.textContent;
  window.requestAnimationFrame(() => {
    const same = next?.isConnected
      ? [...next.querySelectorAll<HTMLElement>("button")].find((b) => b.textContent === label)
      : undefined;
    if (same) same.focus();
    else focusPageHeading();
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
 *
 * THE VIEW PUBLISHES ITS FIRST PAGE'S COVERAGE (`usePageCoverage`), as the
 * room publishes its transcript's: the state bar describes the answer the
 * reader is looking at, so a page a deferred record met is said to be
 * incomplete rather than read as all there is.
 */
function useReaderPages<A extends ChatServed & { next_cursor?: string }, T>(
  first: QueryResult<A>,
  params: Record<string, unknown>,
  page: (cursor: string) => Promise<A>,
  pick: (answer: A) => readonly T[],
  idOf: (entry: T) => string,
) {
  const paged = usePaged(first, params, page, pick, idOf, (answer) => answer.next_cursor ?? "");
  usePageCoverage(coverageOf(first.data));
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

/** What reached the reader, as the context line names it. */
const ACTIVITY_WHY: Record<ChatActivityKind, string> = {
  mention: "Mention",
  reply: "Reply",
  reaction: "Reaction",
};

/**
 * Mentions, replies in threads the reader follows and reactions to their
 * messages, newest first, by day.
 *
 * TWO STATES OF ONE ADDRESS. With nothing open, the list is the whole view and
 * each item reads as the message it is, in full, with why it is here under
 * it. Opening one (`?item=`, so a reload keeps it) splits the view: the list
 * keeps half, its words clamped to two lines, beside the item's conversation:
 * the thread pane a room uses, for an item in a thread, and the message in its
 * room otherwise. Close or Escape gives the list the width back, with focus
 * on the row. On a phone the open item is the whole screen, with the way back.
 */
export function ActivityView() {
  const nav = useChatNav();
  const { rail } = useChatFrame();
  const today = useClock(readerDay);
  const { socket, store } = useClient();
  const mark = useAct("mark_chat_read");
  const panel = useId();
  const listBox = useRef<HTMLDivElement>(null);
  const [filter, setFilter] = useState<ActivityFilter>("all");
  const [unreadOnly, setUnreadOnly] = useState(false);
  const tight = useViewBarTight();
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

  // THE OPEN ITEM OUTLIVES THE LIST'S FILTER: opened, it stays open while a
  // tab or Unread only hides it, or reading it takes it out of an unread
  // list. Only the list stops drawing it. A reload finds it in the list.
  const wanted = nav.place.kind === "activity" ? (nav.place.item ?? "") : "";
  const [held, setHeld] = useState<ChatActivityItem | null>(null);
  const current = wanted
    ? (items.find((item) => item.key === wanted) ?? (held?.key === wanted ? held : null))
    : null;

  const markItem = (item: ChatActivityItem) => {
    // MARKED READ WHERE IT IS READ: the row's own position, sent as its
    // room's cursor, which only moves forward.
    if (!item.unread || item.read_through === undefined) return;
    void markRead(
      mark,
      store,
      { cursors: { [item.channel_id]: item.read_through } },
      { done: "Marked as read", quiet: true },
    );
  };
  const choose = (item: ChatActivityItem) => {
    setHeld(item);
    markItem(item);
    // ONE STEP OF BACK FOR THE OPEN ITEM, however many the reader looks at.
    nav.go({ kind: "activity", item: item.key }, { replace: wanted !== "" });
  };
  const close = () => {
    const key = wanted;
    nav.go({ kind: "activity" }, { replace: true });
    // FOCUS STAYS ON THE ROW the reader closed, once the list is whole again.
    window.requestAnimationFrame(() => focusRow(listBox.current, key));
  };
  const openInRoom = (item: ChatActivityItem) =>
    nav.openRoom(item.channel_id, { message: item.thread_root ?? item.message_id });

  // CHATLET OPENS HERE, so a reader in no rooms at all is told how a room
  // comes to exist rather than that nothing has named them yet: every other
  // empty answer is the filter's.
  const roomless = rail.data?.channels.length === 0;
  // A FILTER THAT EMPTIED THE LIST OFFERS THE WAY BACK, as a control rather
  // than an instruction to find one.
  const empty: { title: string; description: string; action?: ReactNode } = roomless
    ? {
        title: "You’re in no rooms yet",
        description:
          "Give a team a channel in the org chart and Menu lists its room. Anyone can also message you directly.",
        action: (
          <Button size="small" variant="secondary" onClick={() => nav.go({ kind: "browse" })}>
            Browse rooms
          </Button>
        ),
      }
    : unreadOnly
      ? {
          title: "You’re all caught up",
          description: "Nothing here is unread.",
          action: (
            <Button size="small" variant="secondary" onClick={() => setUnreadOnly(false)}>
              Show everything
            </Button>
          ),
        }
      : {
          title: EMPTY_ACTIVITY[filter],
          description:
            "Mentions, replies in threads you follow and reactions to your messages show here.",
        };

  const days = byDay(items, today);
  const list = (
    <div className="chat-activity-list" ref={listBox}>
      {items.length === 0 ? (
        <EmptyState
          size="compact"
          title={empty.title}
          description={empty.description}
          action={empty.action}
        />
      ) : (
        <InboxList label="Activity" density="chat">
          {days.map((day) => (
            <InboxGroup key={day.key} label={day.label}>
              {day.items.map((item) => (
                <ActivityRow
                  key={item.key}
                  item={item}
                  open={current?.key === item.key}
                  split={current !== null}
                  onOpen={() => choose(item)}
                  onRead={() => markItem(item)}
                  onRoom={() => openInRoom(item)}
                />
              ))}
            </InboxGroup>
          ))}
        </InboxList>
      )}
      <div className="chat-activity-foot">
        <PageFoot paged={feed} more="Older activity" />
      </div>
    </div>
  );

  return (
    <section
      className="chat-view"
      onKeyDown={(event) => {
        // ESCAPE CLOSES THE OPEN ITEM from the list as from its pane, unless
        // something nearer (a menu, a field) answered it first.
        if (event.key !== "Escape" || event.defaultPrevented || !current) return;
        event.preventDefault();
        close();
      }}
    >
      <ViewHead>
        {/* A TIGHT BAR DRAWS THE TRACK ALONE, still named by its label and
            hinted by the tooltip; the one element across both keeps its focus. */}
        <Tooltip content="Unread only" disabled={!tight}>
          <Switch
            label="Unread only"
            size="sm"
            labelHidden={tight}
            checked={unreadOnly}
            onCheckedChange={setUnreadOnly}
          />
        </Tooltip>
      </ViewHead>
      <Tabs
        className="chat-view-tabs"
        variant="underline"
        size="sm"
        density="chat"
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
          <ListDetail list={list} showing={current ? "detail" : "list"} listWidth="half" flush>
            {current && (
              <ChatDetailPlacement back="Back to Activity">
                <ActivityDetail
                  key={current.key}
                  item={current}
                  onClose={close}
                  onRoom={() => openInRoom(current)}
                />
              </ChatDetailPlacement>
            )}
          </ListDetail>
        </QueryState>
      </TabPanel>
    </section>
  );
}

/** A day of activity: its civil key, its name, and its items, newest first. */
interface ActivityDay {
  key: string;
  label: string;
  items: ChatActivityItem[];
}

/** The items under their days, in the feed's own order (newest first). */
function byDay(items: readonly ChatActivityItem[], today: string): ActivityDay[] {
  const days: ActivityDay[] = [];
  for (const item of items) {
    const key = dayOf(item.at);
    let day = days[days.length - 1];
    if (!day || day.key !== key) {
      day = { key, label: dayLabel(key, today), items: [] };
      days.push(day);
    }
    day.items.push(item);
  }
  return days;
}

/** Focus a row's own control in a list, by the key the row carries (`data-item`). */
export function focusRow(list: HTMLElement | null, key: string): void {
  if (!list || !key) return;
  const row = [...list.querySelectorAll<HTMLElement>("[data-item]")].find(
    (each) => each.dataset.item === key,
  );
  row?.querySelector<HTMLElement>("button, a")?.focus();
}

/**
 * One activity, read as the message it is: the face, the name and the time,
 * the words in full (two lines beside an open item), and why it is here.
 */
function ActivityRow({
  item,
  open,
  split,
  onOpen,
  onRead,
  onRoom,
  leaving,
  onLeft,
}: {
  item: ChatActivityItem;
  open: boolean;
  split: boolean;
  onOpen: () => void;
  onRead: () => void;
  onRoom: () => void;
  /** Handed by the kit's group while the row closes its height on its way out. */
  leaving?: boolean | undefined;
  onLeft?: (() => void) | undefined;
}) {
  const { viewer, channels, nameOf } = useChatFrame();
  const people = useChatPeople();
  const words = useRoomWords();
  const actor = people.person(item.actor, item.actor_kind);
  const summary = channels.find((row) => row.channel.id === item.channel_id);
  const room = { id: item.channel_id, kind: item.channel_kind, name: item.channel_name };
  // A CONVERSATION OF THE READER AND THE ACTOR ALONE IS NOT NAMED: its title
  // is the actor's own name, which the row has just said ([isConversationWith]).
  const where = isConversationWith(item.channel_kind, item.actor, viewer, summary?.participants)
    ? "a direct message"
    : words.where(room);
  const why =
    item.kind === "mention" && item.collective ? "Mention of everyone" : ACTIVITY_WHY[item.kind];
  // A REACTION'S EMOJI IS ITS GLYPH where the table draws it, and its words
  // where it does not, so no reaction is left unnamed.
  const emoji =
    item.kind === "reaction" && !glyphOf(item.emoji ?? "")
      ? ` ${reactionShown(item.emoji ?? "")}`
      : "";
  const context = `${why}${emoji} in ${where}${item.thread_root ? " thread" : ""}`;
  // WHO THE WORDS NAMED, as far as the row knows it: a mention named the
  // reader (or the whole room); any other handle stays the inline token.
  const message = {
    body: item.excerpt ?? "",
    mentions: item.kind === "mention" && !item.collective ? [viewer] : [],
    collective: item.collective ?? false,
  };
  const markable = item.unread && item.read_through !== undefined;
  return (
    <InboxRow
      leaving={leaving}
      onLeft={onLeft}
      data-item={item.key}
      title={actor.name}
      meta={<time dateTime={item.at}>{timeOf(item.at)}</time>}
      leading={<ChatFace person={actor} size={36} status={false} />}
      lines={split ? 2 : "all"}
      context={context}
      contextIcon={activityMark(item)}
      selected={open}
      unread={item.unread}
      unreadLabel="Unread"
      onClick={onOpen}
      actions={
        <MessageActions label={`Actions for ${actor.name}’s ${why.toLowerCase()}`}>
          {markable && (
            <MessageAction label="Mark as read" icon={<CheckGlyph />} onClick={onRead} />
          )}
          <MessageAction
            label={item.thread_root ? "Open the thread in its room" : "Open in room"}
            icon={<ArrowUpRightGlyph />}
            onClick={onRoom}
          />
        </MessageActions>
      }
    >
      <MessageLine message={message} viewer={viewer} nameOf={nameOf} />
    </InboxRow>
  );
}

/** The mark before an activity's context line: what kind of activity it is. */
function activityMark(item: ChatActivityItem): ReactNode {
  if (item.kind === "mention") return <AtSignGlyph size="sm" />;
  if (item.kind === "reply") return <MessagesSquareGlyph size="sm" />;
  // A SHORTCODE THE TABLE DOES NOT DRAW gets a neutral face: its name is in
  // the room, and words do not fit the glyph's place.
  return glyphOf(item.emoji ?? "") ?? <FaceSlightlySmilingGlyph size="sm" />;
}

/**
 * The open activity: the thread pane a room uses, for a reply or a mention in
 * a thread, and otherwise the message in its room, with a reply box under it.
 */
function ActivityDetail({
  item,
  onClose,
  onRoom,
}: {
  item: ChatActivityItem;
  onClose: () => void;
  onRoom: () => void;
}) {
  if (item.thread_root)
    return <ThreadPane room={item.channel_id} root={item.thread_root} onClose={onClose} />;
  return <ActivityContext item={item} onClose={onClose} onRoom={onRoom} />;
}

/**
 * A message read in place: the message as the room draws it, with the replies
 * just before it, and a reply box where the room takes one from the reader.
 */
function ActivityContext({
  item,
  onClose,
  onRoom,
}: {
  item: ChatActivityItem;
  onClose: () => void;
  onRoom: () => void;
}) {
  const detail = useRoomDetail(item.channel_id);
  const words = useRoomWords();
  const read = useQuery("chat_message", {
    channel_id: item.channel_id,
    message_id: item.message_id,
  });
  const room = { id: item.channel_id, kind: item.channel_kind, name: item.channel_name };
  return (
    <RoomScopeProvider channelID={item.channel_id} detail={detail}>
      <ChatSidePanel
        title="Message"
        subtitle={words.where(room)}
        actions={
          <Button size="small" variant="ghost" onClick={onRoom}>
            Open in room
          </Button>
        }
        flush
        flow
        onClose={onClose}
        footer={<ActivityReply item={item} />}
      >
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
      </ChatSidePanel>
    </RoomScopeProvider>
  );
}

/** The reply box under an activity, where the room takes one. */
function ActivityReply({ item }: { item: ChatActivityItem }) {
  const scope = useRoomScope();
  const words = useRoomWords();
  if (scope.rules.post !== "") return <p className="chat-pane-note">{scope.rules.post}</p>;
  const where = words.where({
    id: item.channel_id,
    kind: item.channel_kind,
    name: item.channel_name,
  });
  return (
    <div className="chat-pane-foot">
      <Composer
        channelID={item.channel_id}
        compact
        label={`Message ${where}`}
        placeholder={`Reply in ${where}`}
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
  const words = useRoomWords();
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
      <Tabs
        className="chat-view-tabs"
        variant="underline"
        size="sm"
        density="chat"
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
                // HEADED BY WHERE IT WENT, never by the room's title alone: a
                // conversation's title is the other person, and a card headed
                // by them reads as their message rather than the reader's.
                const went = isDirect(room.kind)
                  ? `To ${words.title(room)}`
                  : `In ${words.where(room)}`;
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
                      <span className="chat-context__name" title={went}>
                        {went}
                      </span>
                      <span className="chat-muted-text">
                        {message.thread_root ? "· in a thread " : ""}·{" "}
                        {relTime(message.created_at, now)}
                      </span>
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
  // THE AUTHOR IS ADDED BY THE ENGINE, so the cap here is one short of its
  // own: seven others and the reader are the eight a conversation holds. At
  // the cap nobody more is offered; past it (an address that names more) the
  // message is held, and both say why.
  const full = to.length >= MAX_DIRECT_PARTICIPANTS - 1;
  const crowded =
    to.length > MAX_DIRECT_PARTICIPANTS - 1
      ? `That’s ${to.length + 1} people counting you, and a conversation holds at most ${MAX_DIRECT_PARTICIPANTS}. For more, start a room.`
      : "";
  const roomQuery = q.startsWith("#") ? q.slice(1).toLowerCase() : null;
  const matches =
    roomQuery === null && !full ? others.filter((person) => matchesPerson(person, q)) : [];
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
      : crowded
        ? crowded
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
    // not confirm waits in Drafts and sent with Retry.
    void frame.outbox.send({ channelID: room, body: words });
    setBody("");
    nav.openRoom(room);
  }

  return (
    <section className="chat-view">
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
        {full && (
          <p className="chat-pane-note">
            {crowded ||
              `A conversation holds at most ${MAX_DIRECT_PARTICIPANTS} people, you included. For more, start a room.`}
          </p>
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
