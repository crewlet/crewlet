/**
 * The conversation's top bar: one row, every control one height.
 *
 *   Menu      opens the Rooms panel from the start edge (or hides it while
 *             pinned); its badge is everything waiting on the reader
 *   Rooms     the open room's name with a chevron: a switcher listing every
 *             room the reader is in, starred first, with Browse rooms and
 *             New room at its foot
 *   Messages  the open conversation's name, or Direct messages: a switcher
 *             listing conversations, then the people and agents to start one
 *             with, and New message
 *   actions   the open room's members, search, notifications and More. Ask
 *             Crewlet is the page bar's own button, so the bar has none.
 *
 * WHILE THE ROOMS PANEL IS PINNED it lists everything the two switchers do,
 * beside the conversation, so the bar keeps Menu and the room's actions.
 *
 * THE BAR TAKES ITS SHAPE FROM ITS OWN WIDTH, not the window's: the pinned
 * panel and a pane beside the conversation take width the window never
 * reports. Under [BAR_NARROW_PX] Menu drops its word and the Messages
 * switcher shortens its own; under [BAR_TIGHT_PX] Search and the
 * notification choices move into More and the switchers drop their badges
 * (Menu's carries the total).
 */

import { useLayoutEffect, useRef, useState, type Ref, type RefObject } from "react";
import {
  Button,
  Count,
  IconButton,
  Switcher,
  Tooltip,
  type SwitcherGroup,
  type SwitcherOption,
} from "@crewlethq/ui";
import {
  CompassGlyph,
  PanelLeftGlyph,
  PlusGlyph,
  SearchGlyph,
  SquarePenGlyph,
} from "@crewlethq/icons/glyphs";

import { useToastTop } from "~/app/toastInset.tsx";
import { attentionFigure, isDirect, useChatAttention } from "~/lib/chatRooms.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { ChatFace } from "./Face.tsx";
import { useChatFrame, useChatNav, type ChatPlace } from "./nav.tsx";
import { matchesPerson, useChatPeople } from "./people.ts";
import { RoomActions } from "./RoomActions.tsx";
import { RoomMark } from "./RoomsPanel.tsx";
import { roomTitle, unreadIn, unreadLabel, waitingIn } from "./rooms.ts";

/** Under this width of the bar, Menu drops its word. */
export const BAR_NARROW_PX = 760;

/** Under this width of the bar, Search and notifications fold into More. */
export const BAR_TIGHT_PX = 480;

/**
 * The bar's own width, measured again as it changes. Zero until the first
 * measurement, which reads as wide: the wide bar is the one that works
 * before anything has been measured.
 */
function useBarWidth(bar: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = bar.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const measure = () => setWidth(el.getBoundingClientRect().width);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [bar]);
  return width;
}

/** A switcher's open state and its query, cleared on every close. */
function useSwitcher() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  return {
    open,
    onOpenChange: (next: boolean) => {
      setOpen(next);
      if (!next) setQuery("");
    },
    query,
    onQueryChange: setQuery,
  };
}

/** Whether a word matches what was typed in a switcher. */
function matches(query: string, ...words: readonly (string | undefined)[]): boolean {
  const q = query.trim().toLowerCase();
  return !q || words.some((word) => word?.toLowerCase().includes(q));
}

/** Groups that hold rows, with nothing drawn for an empty one. */
const filled = (groups: SwitcherGroup[]) => groups.filter((group) => group.options.length > 0);

/** What waits on the reader across these rooms, the open one excepted. */
function waitingAcross(
  rooms: readonly ChatChannelSummary[],
  measured: boolean,
  except = "",
): number {
  return rooms.reduce(
    (sum, room) => sum + (room.channel.id === except ? 0 : waitingIn(room, measured)),
    0,
  );
}

/** A room's row in a switcher: its mark, its name, and what waits in it. */
function useRoomOption() {
  const nav = useChatNav();
  const { viewer, nameOf, measured } = useChatFrame();
  return (room: ChatChannelSummary, current: string): SwitcherOption => {
    const name = roomTitle(room.channel, { participants: room.participants, viewer, nameOf });
    const waiting = waitingIn(room, measured);
    const unread = unreadIn(room, measured) ? unreadLabel(room.unread, room.unread_capped) : "";
    const place: ChatPlace = { kind: "room", id: room.channel.id };
    return {
      id: room.channel.id,
      label: name,
      icon: <RoomMark room={room} />,
      meta: waiting ? (
        <Count tone="attention" value={unreadLabel(waiting, room.unread_capped)} />
      ) : unread ? (
        <span className="chat-switch-figure">{unread} new</span>
      ) : undefined,
      accessibleLabel: waiting
        ? `${name}, ${waiting} for you`
        : unread
          ? `${name}, ${unread} new`
          : name,
      href: nav.hrefOf(place),
      current: room.channel.id === current,
      onSelect: () => nav.go(place),
    };
  };
}

/** The rooms the switchers and the Menu badge read: the open, unarchived ones. */
function useListedRooms(): readonly ChatChannelSummary[] {
  const { channels } = useChatFrame();
  return channels.filter((room) => !room.channel.archived_at);
}

/** Rooms: the open room's name, and every room the reader is in under it. */
function RoomSwitcher({ tight }: { tight: boolean }) {
  const nav = useChatNav();
  const { viewer, nameOf, measured, starred } = useChatFrame();
  const sw = useSwitcher();
  const option = useRoomOption();
  const all = useListedRooms().filter((room) => !isDirect(room.channel.kind));
  const open = nav.place.kind === "room" ? nav.place.id : "";
  const here = all.find((room) => room.channel.id === open);
  const current = here?.channel.id ?? "";
  const rooms = all.filter((room) => matches(sw.query, room.channel.name, room.channel.topic));
  const isStarred = (room: ChatChannelSummary) => starred.has(room.channel.id);
  const elsewhere = waitingAcross(all, measured, current);
  const title = here
    ? roomTitle(here.channel, { participants: here.participants, viewer, nameOf })
    : "";
  const browse: SwitcherOption = {
    id: "browse",
    label: "Browse rooms",
    icon: <CompassGlyph size="sm" />,
    href: nav.hrefOf({ kind: "browse" }),
    onSelect: () => nav.go({ kind: "browse" }),
  };
  return (
    <span className="chat-switch">
      <Switcher
        label="Switch room"
        {...sw}
        trigger={{
          // A TEAM'S ROOM IS ITS TEAM'S NAME, never a channel's `#`.
          label: here ? (here.channel.kind === "unit" ? title : `#${title}`) : "Rooms",
          title: "Switch room",
          ...(elsewhere && !tight ? { tag: <Count tone="attention" value={elsewhere} /> } : {}),
        }}
        placeholder="Find a room"
        listLabel="Rooms"
        groups={filled([
          {
            id: "starred",
            label: "Starred",
            options: rooms.filter(isStarred).map((room) => option(room, current)),
          },
          {
            id: "rooms",
            label: "Rooms",
            options: rooms
              .filter((room) => !isStarred(room) && room.channel.kind !== "unit")
              .map((room) => option(room, current)),
          },
          {
            id: "teams",
            label: "Team rooms",
            options: rooms
              .filter((room) => !isStarred(room) && room.channel.kind === "unit")
              .map((room) => option(room, current)),
          },
        ])}
        {...(rooms.length === 0
          ? {
              message: {
                title: "No rooms match",
                body: "Browse rooms to find one you are not in yet.",
                actions: [browse],
              },
            }
          : {})}
        footer={[
          browse,
          {
            id: "new-room",
            label: "New room",
            icon: <PlusGlyph size="sm" />,
            onSelect: () => nav.openDialog({ kind: "new-room" }),
          },
        ]}
      />
    </span>
  );
}

/** Messages: the open conversation's name, conversations, then whom to start one with. */
function MessageSwitcher({ narrow, tight }: { narrow: boolean; tight: boolean }) {
  const nav = useChatNav();
  const { viewer, nameOf, measured } = useChatFrame();
  const people = useChatPeople();
  const sw = useSwitcher();
  const option = useRoomOption();
  const all = useListedRooms().filter((room) => isDirect(room.channel.kind));
  const open = nav.place.kind === "room" ? nav.place.id : "";
  const here = all.find((room) => room.channel.id === open);
  const current = here?.channel.id ?? "";
  const titleOf = (room: ChatChannelSummary) =>
    roomTitle(room.channel, { participants: room.participants, viewer, nameOf });
  const conversations = all.filter((room) => matches(sw.query, titleOf(room)));
  // EVERYONE THE READER HAS NO ONE-TO-ONE CONVERSATION WITH YET. Starting one
  // goes to New message with them chosen, which opens the conversation they
  // already share if there is one: the switcher writes nothing itself.
  const talking = new Set(
    all.filter((room) => room.channel.kind === "dm").flatMap((room) => room.participants ?? []),
  );
  const others = people.everyone.filter(
    (person) =>
      person.handle !== viewer && !talking.has(person.handle) && matchesPerson(person, sw.query),
  );
  const elsewhere = waitingAcross(all, measured, current);
  return (
    <span className="chat-switch">
      <Switcher
        label="Switch conversation"
        {...sw}
        trigger={{
          label: here ? titleOf(here) : tight ? "DMs" : narrow ? "Messages" : "Direct messages",
          title: "Switch conversation",
          ...(elsewhere && !tight ? { tag: <Count tone="attention" value={elsewhere} /> } : {}),
        }}
        placeholder="Find a person or agent"
        listLabel="Conversations"
        groups={filled([
          {
            id: "conversations",
            label: "Direct messages",
            options: conversations.map((room) => option(room, current)),
          },
          {
            id: "start",
            label: "Start a conversation",
            options: others.map((person) => {
              const what = person.kind === "agent" ? "Agent" : person.team;
              const place: ChatPlace = { kind: "new", to: [person.handle] };
              return {
                id: `start-${person.handle}`,
                label: person.name,
                icon: <ChatFace person={person} size="xs" />,
                meta: what ? <span className="chat-switch-figure">{what}</span> : undefined,
                accessibleLabel: what ? `${person.name}, ${what}` : person.name,
                href: nav.hrefOf(place),
                onSelect: () => nav.go(place),
              };
            }),
          },
        ])}
        {...(conversations.length + others.length === 0
          ? { message: { title: "No one matches", body: "Try a name or a team." } }
          : {})}
        footer={[
          {
            id: "new-message",
            label: "New message",
            icon: <SquarePenGlyph size="sm" />,
            href: nav.hrefOf({ kind: "new" }),
            onSelect: () => nav.go({ kind: "new" }),
          },
        ]}
      />
    </span>
  );
}

export function ChatTopBar({ menu }: { menu?: Ref<HTMLButtonElement> | undefined } = {}) {
  const nav = useChatNav();
  const bar = useRef<HTMLDivElement | null>(null);
  const width = useBarWidth(bar);
  // TOASTS STAND UNDER THIS BAR, at the conversation's top right: at the
  // window's foot they lay over the room's composer and a thread's, which
  // grow as they are typed in, so no fixed inset from the foot clears them.
  useToastTop(bar);
  const narrow = width > 0 && width < BAR_NARROW_PX;
  const tight = width > 0 && width < BAR_TIGHT_PX;
  // THE SIDEBAR'S OWN FIGURE (`lib/chatRooms.ts`), so the Chatlet row and
  // Menu never disagree about what waits.
  const attention = useChatAttention();
  const waiting = attention ? attentionFigure(attention) : "";
  const shown = nav.rooms.docked || nav.rooms.open;
  const switchers = !nav.rooms.docked;

  return (
    <div
      ref={bar}
      className={narrow ? "chat-topbar is-narrow" : "chat-topbar"}
      role="navigation"
      aria-label="Chatlet"
    >
      <Tooltip content={nav.rooms.docked ? "Hide the panel" : "Rooms, direct messages and views"}>
        <Button
          ref={menu}
          className="chat-topbar-menu"
          variant="secondary"
          size="medium"
          leadingIcon={<PanelLeftGlyph size="sm" />}
          pressed={shown}
          aria-expanded={shown}
          aria-label={waiting ? `Menu, ${waiting} waiting for you` : "Menu"}
          onClick={nav.rooms.toggle}
        >
          <span className="chat-topbar-menu-label">Menu</span>
          {waiting && <Count tone="attention" value={waiting} />}
        </Button>
      </Tooltip>
      {switchers && (
        <>
          {!tight && <span className="chat-topbar-sep" aria-hidden="true" />}
          <RoomSwitcher tight={tight} />
          <MessageSwitcher narrow={narrow} tight={tight} />
        </>
      )}
      <span className="chat-topbar-end">
        {nav.place.kind === "room" ? (
          <RoomActions tight={tight} />
        ) : (
          <Tooltip content="Search Chatlet">
            <IconButton
              label="Search Chatlet"
              size="md"
              variant="ghost"
              icon={<SearchGlyph size="sm" />}
              onClick={() => nav.go({ kind: "search" })}
            />
          </Tooltip>
        )}
      </span>
    </div>
  );
}
