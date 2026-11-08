/**
 * Direct messages: every conversation the reader is in, newest first, with the
 * one they open beside the list.
 *
 * ONE LIST, THE RAIL'S. The conversations are the frame's own rooms
 * (`chat_channels`), the same answer the Rooms panel and the top bar's
 * switcher read, so the three never disagree about what waits where. Nothing
 * here is read twice.
 *
 * THE OPEN CONVERSATION IS THE OPEN ROOM. Its address is `?room=` on this page
 * (`nav.tsx`'s `openRoomOf`), so its scope, its detail, its actions and a
 * thread opened from it are the room's own, and it is drawn by the room's own
 * component: one conversation, wherever it is read.
 *
 * ON A PHONE the list is the whole screen, and an open conversation is too,
 * with the way back at its start (`ListDetail.Back`).
 */

import { useRef, useState } from "react";

import {
  AvatarStack,
  Button,
  EmptyState,
  IconButton,
  InboxGroup,
  InboxList,
  InboxRow,
  Input,
  ListDetail,
  ListDetailBack,
  Menu,
  MessageAction,
  MessageActions,
  Switch,
  Tooltip,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  ChevronDownGlyph,
  SearchGlyph,
  SquarePenGlyph,
  StarGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";

import { useClock } from "~/lib/clock.ts";
import { useWidth } from "~/lib/useWidth.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { readerDay } from "~/lib/format.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { MessageLine } from "./Body.tsx";
import { listDate } from "./days.ts";
import { ChatFace, stackMember } from "./Face.tsx";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { useChatPeople } from "./people.ts";
import { Room } from "./Room.tsx";
import { RoomActions } from "./RoomActions.tsx";
import { roomTitle, unreadIn } from "./rooms.ts";
import { RoomScopeProvider } from "./scope.tsx";
import { focusRow } from "./Views.tsx";

/**
 * Under this width of the list's own column, a row's date takes its short
 * form and the Unreads switch drops its word (still its name and hint), so
 * the head keeps New message in view.
 */
export const DMS_NARROW_PX = 384;

/** Whether a conversation's title holds what was typed in the find field. */
function finds(query: string, title: string): boolean {
  const words = query.trim().toLowerCase();
  return words === "" || title.toLowerCase().includes(words);
}

/** Newest last message first; a conversation nothing was said in yet, last. */
function newestFirst(a: ChatChannelSummary, b: ChatChannelSummary): number {
  return (b.last?.at ?? "").localeCompare(a.last?.at ?? "");
}

export function DirectMessagesView() {
  const nav = useChatNav();
  const frame = useChatFrame();
  const { viewer, nameOf, measured } = frame;
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [query, setQuery] = useState("");
  const listBox = useRef<HTMLDivElement>(null);
  const width = useWidth(listBox);
  const narrow = width > 0 && width < DMS_NARROW_PX;
  const open = nav.place.kind === "dms" ? (nav.place.room ?? "") : "";

  const titleOf = (row: ChatChannelSummary) =>
    roomTitle(row.channel, { participants: row.participants, viewer, nameOf });
  const all = frame.channels
    .filter((row) => isDirect(row.channel.kind) && !row.channel.archived_at)
    .sort(newestFirst);
  const waiting = all.filter((row) => unreadIn(row, measured));
  // THE OPEN CONVERSATION STAYS IN A FILTERED LIST: reading it is what takes
  // it out of the unread ones, and the list should not lose the row it shows.
  const shown = all.filter(
    (row) =>
      (!unreadOnly || unreadIn(row, measured) || row.channel.id === open) &&
      finds(query, titleOf(row)),
  );
  const current = all.find((row) => row.channel.id === open) ?? null;

  const choose = (row: ChatChannelSummary) =>
    nav.go({ kind: "dms", room: row.channel.id }, { replace: open !== "" });
  const close = () => {
    const key = open;
    nav.go({ kind: "dms" }, { replace: true });
    window.requestAnimationFrame(() => focusRow(listBox.current, key));
  };

  const list = (
    <div className="chat-dms-list" ref={listBox}>
      {/* THE HEAD AND THE FIND FIELD STAY while the conversations scroll under them. */}
      <div className="chat-dms-top">
        <div className="chat-dms-head">
          {/* THE LIST'S MENU IS A CONTROL, NOT A HEADING: the page's one `h1`
            is the page trail's last crumb, which names it. */}
          <Menu
            label="Direct messages"
            activeDescendant
            entrance="instant"
            renderTrigger={(props) => (
              <Button
                {...props}
                variant="ghost"
                size="medium"
                trailingIcon={<ChevronDownGlyph size="sm" />}
              >
                <span className="chat-dms-title">Direct messages</span>
              </Button>
            )}
            items={[
              {
                key: "new",
                label: "New message",
                icon: <SquarePenGlyph size="sm" />,
                onSelect: () => nav.go({ kind: "new" }),
              },
              {
                key: "read-all",
                label: "Mark all as read",
                icon: <CheckGlyph size="sm" />,
                disabled: waiting.length === 0,
                onSelect: () => {
                  for (const row of waiting) frame.markRead(row.channel.id);
                },
              },
            ]}
          />
          <span className="chat-dms-tools">
            <Tooltip content="Unreads" disabled={!narrow}>
              <Switch
                label="Unreads"
                size="sm"
                labelHidden={narrow}
                checked={unreadOnly}
                onCheckedChange={setUnreadOnly}
              />
            </Tooltip>
            <Tooltip content="New message">
              <IconButton
                label="New message"
                size="md"
                variant="ghost"
                icon={<SquarePenGlyph />}
                onClick={() => nav.go({ kind: "new" })}
              />
            </Tooltip>
          </span>
        </div>
        <div className="chat-dms-find">
          <Input
            inputSize="md"
            leading={<SearchGlyph size="sm" />}
            placeholder="Find a DM"
            aria-label="Find a direct message"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onClear={() => setQuery("")}
          />
        </div>
      </div>
      {shown.length === 0 ? (
        <EmptyState
          size="compact"
          title={
            all.length === 0
              ? "No direct messages yet"
              : unreadOnly && query.trim() === ""
                ? "You’re all caught up"
                : "No conversation matches"
          }
          description={
            all.length === 0
              ? "Start a conversation with a person or an agent, and it is listed here."
              : unreadOnly && query.trim() === ""
                ? "No direct message is unread."
                : "Try another name."
          }
          action={
            all.length === 0 ? (
              <Button size="small" variant="secondary" onClick={() => nav.go({ kind: "new" })}>
                New message
              </Button>
            ) : undefined
          }
        />
      ) : (
        <InboxList label="Direct messages" density="chat">
          <InboxGroup label="Conversations" hideLabel>
            {shown.map((row) => (
              <ConversationRow
                key={row.channel.id}
                row={row}
                title={titleOf(row)}
                open={row.channel.id === open}
                narrow={narrow}
                onOpen={() => choose(row)}
              />
            ))}
          </InboxGroup>
        </InboxList>
      )}
    </div>
  );

  return (
    <section className="chat-view">
      <ListDetail className="chat-dms-split" list={list} showing={open ? "detail" : "list"} flush>
        {current ? (
          <Conversation key={open} row={current} title={titleOf(current)} onClose={close} />
        ) : (
          <div className="chat-dms-none">
            <EmptyState
              size="compact"
              title="Choose a conversation"
              description="Pick one from the list, or start a new one."
              action={
                <Button size="small" variant="secondary" onClick={() => nav.go({ kind: "new" })}>
                  New message
                </Button>
              }
            />
          </div>
        )}
      </ListDetail>
    </section>
  );
}

/**
 * Who a conversation is with, as faces: the other person's, with their state,
 * or a group's first two, stacked.
 */
function ConversationFace({ row, size }: { row: ChatChannelSummary; size: number }) {
  const { viewer } = useChatFrame();
  const people = useChatPeople();
  const others = (row.participants ?? []).filter((handle) => handle !== viewer);
  if (row.channel.kind === "group") {
    return (
      <span className="chat-dms-group-face" style={{ height: size }}>
        <AvatarStack
          members={others.slice(0, 2).map((handle) => stackMember(people.person(handle)))}
          max={2}
          // TWO FACES SIDE BY SIDE IN ONE FACE'S WIDTH, so a group's words start
          // where everyone else's do and neither face's initials are covered.
          size={Math.max(12, Math.floor(size * 0.44))}
          spread
          decorative
        />
      </span>
    );
  }
  return <ChatFace person={people.person(others[0] ?? viewer)} size={size} />;
}

/**
 * One conversation: the face with its presence, the name, when it was last
 * spoken in (long where the column is wide, short where it is narrow), and
 * the last message in two lines.
 */
function ConversationRow({
  row,
  title,
  open,
  narrow,
  onOpen,
  leaving,
  onLeft,
}: {
  row: ChatChannelSummary;
  title: string;
  open: boolean;
  /** The list's column is narrow: the date in its short form. */
  narrow: boolean;
  onOpen: () => void;
  /** Handed by the kit's group while the row closes its height on its way out. */
  leaving?: boolean | undefined;
  onLeft?: (() => void) | undefined;
}) {
  const frame = useChatFrame();
  const { viewer, nameOf, measured, starred } = frame;
  const people = useChatPeople();
  const today = useClock(readerDay);
  const group = row.channel.kind === "group";
  const last = row.last;
  const when = last ? listDate(last.at, today) : null;
  const unread = unreadIn(row, measured);
  const isStarred = starred.has(row.channel.id);
  // THE LAST WORDS AS SAID, with who said them where it is not plain: the
  // reader's own last word, or a group's.
  const by =
    last?.author && (last.author === viewer || group)
      ? `${last.author === viewer ? "You" : people.person(last.author).name}: `
      : "";
  return (
    <InboxRow
      leaving={leaving}
      onLeft={onLeft}
      data-item={row.channel.id}
      title={title}
      meta={when ? <time dateTime={last?.at}>{narrow ? when.short : when.long}</time> : undefined}
      leading={<ConversationFace row={row} size={36} />}
      lines={2}
      selected={open}
      unread={unread}
      unreadLabel="Unread"
      onClick={onOpen}
      actions={
        <MessageActions label={`Actions for ${title}`}>
          {unread && (
            <MessageAction
              label="Mark as read"
              icon={<CheckGlyph />}
              onClick={() => frame.markRead(row.channel.id)}
            />
          )}
          <MessageAction
            label={isStarred ? "Remove the star" : "Star"}
            icon={<StarGlyph />}
            pressed={isStarred}
            onClick={() => frame.star(row.channel.id, !isStarred)}
          />
        </MessageActions>
      }
    >
      {last?.deleted ? (
        <span className="chat-muted-text">This message was deleted.</span>
      ) : last ? (
        <>
          {by}
          <MessageLine message={{ body: last.excerpt ?? "" }} viewer={viewer} nameOf={nameOf} />
        </>
      ) : (
        <span className="chat-muted-text">Nothing said yet.</span>
      )}
    </InboxRow>
  );
}

/**
 * The open conversation, as a room draws it: a head with who it is with, the
 * room's own actions and Close (and on a phone the way back), then the room.
 */
function Conversation({
  row,
  title,
  onClose,
}: {
  row: ChatChannelSummary;
  title: string;
  onClose: () => void;
}) {
  // THE ROOM'S OWN SCOPE around its head as well as its transcript, so the
  // head's actions read the rules the room reads, wherever the page is drawn.
  return (
    <RoomScopeProvider channelID={row.channel.id}>
      <div className="chat-dms-open">
        <header className="chat-dms-open__head">
          <ListDetailBack label="Back to Direct messages" onClick={onClose} />
          <ConversationFace row={row} size={24} />
          <h2 className="chat-dms-open__title">{title}</h2>
          <span className="chat-dms-open__actions">
            <RoomActions tight />
            <Tooltip content="Close">
              <IconButton
                label="Close"
                size="header"
                variant="ghost"
                icon={<XGlyph />}
                onClick={onClose}
              />
            </Tooltip>
          </span>
        </header>
        <Room key={row.channel.id} channelID={row.channel.id} />
      </div>
    </RoomScopeProvider>
  );
}
