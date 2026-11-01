/**
 * The Rooms panel: what Menu at the far left of the top bar opens. A find
 * row (a filter field, the filter and sort menu and the pin), the views
 * (Activity, Threads, Drafts and sent, Saved), the room sections (Starred,
 * Channels, Team rooms, Direct messages) and the two ways to start a
 * conversation, on the kit's SidebarNav in its dense rows.
 *
 * SLID IN OR PINNED, ONE BODY. It slides in from the start edge INSIDE the
 * chat area (a sheet on the chat area's own layer host, so the app sidebar is
 * never covered), or it is pinned there as a column beside the conversation.
 * The body is the same component in both, so a pinned panel and a slid-in
 * one cannot come to list different rooms.
 *
 * A SECTION COLLAPSES, and a collapsed one still draws the open room and the
 * rooms with unread messages that are not muted (the kit's collapsible
 * group): what is waiting is never folded away.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  DockColumn,
  EmptyState,
  IconButton,
  Input,
  Menu,
  Modal,
  SidebarNav,
  Tooltip,
} from "@crewlethq/ui";
import { CompassGlyph, PinGlyph, SearchGlyph, SquarePenGlyph } from "@crewlethq/icons/glyphs";
import { BellOffGlyph, ListFilterGlyph, PencilGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { QueryState } from "~/components/common.tsx";
import { isDirect } from "~/lib/chatRooms.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { useChatDrafts } from "./drafts.ts";
import { ChatFace } from "./Face.tsx";
import { useChatFrame, useChatNav, type ChatPlace } from "./nav.tsx";
import { useChatPeople } from "./people.ts";
import {
  roomMark,
  roomSection,
  roomTitle,
  ROOM_SECTIONS,
  unreadIn,
  unreadLabel,
  waitingIn,
  type RoomSection,
} from "./rooms.ts";
import { PANEL_VIEWS, viewCount } from "./panelViews.ts";

/** What each section is called. */
const SECTION_LABEL: Record<RoomSection, string> = {
  starred: "Starred",
  channels: "Channels",
  teams: "Team rooms",
  direct: "Direct messages",
};

/** How the list is narrowed and ordered, from the find row's menu. */
interface ListOptions {
  unreadOnly: boolean;
  sort: "activity" | "name";
}

const DEFAULT_OPTIONS: ListOptions = { unreadOnly: false, sort: "activity" };

/**
 * Runs a pick from the panel.
 *
 * SLID IN, THE PANEL CLOSES FIRST AND THE PLACE CHANGES ON THE NEXT TASK.
 * Closing a sheet hands focus back to Menu when its layer unmounts, which
 * runs after the new place has mounted: a view that takes focus on arrival
 * (Search's field, New message's To) lost it to the button. Pinned, there is
 * nothing to close and the move is immediate.
 */
type Pick = (run: () => void) => void;

/** A room's mark in a list: a face for a conversation with one person, a
 *  count for a group, else its kind's glyph. */
export function RoomMark({ room }: { room: ChatChannelSummary }) {
  const { viewer } = useChatFrame();
  const people = useChatPeople();
  const others = (room.participants ?? []).filter((handle) => handle !== viewer);
  if (room.channel.kind === "dm") {
    return <ChatFace person={people.person(others[0] ?? viewer)} size="xs" />;
  }
  if (room.channel.kind === "group") {
    return (
      <span className="chat-group-count" aria-hidden="true">
        {others.length}
      </span>
    );
  }
  return <NamedGlyph name={roomMark(room.channel.kind)} size="sm" />;
}

/** The four views above the rooms, from the chat row's own sections. */
function ViewItems({ pick }: { pick: Pick }) {
  const nav = useChatNav();
  const drafts = useChatDrafts();
  const { outbox, counts } = useChatFrame();
  return (
    <SidebarNav.Group>
      {PANEL_VIEWS.map((view) => {
        const figure = viewCount(view.kind, { counts, drafts, outbox });
        const place: ChatPlace = { kind: view.kind };
        return (
          <SidebarNav.Item
            key={view.kind}
            label={view.label}
            icon={<NamedGlyph name={view.icon} size="sm" />}
            href={nav.hrefOf(place)}
            current={nav.place.kind === view.kind}
            onClick={(event) => {
              event.preventDefault();
              pick(() => nav.go(place));
            }}
            {...(figure && figure.tone === "attention"
              ? { badge: { value: figure.value, label: figure.label } }
              : {})}
            {...(figure && figure.tone === "quiet"
              ? { count: { value: figure.value, label: figure.label } }
              : {})}
          />
        );
      })}
    </SidebarNav.Group>
  );
}

/** The rooms, in their sections. */
function RoomSections({
  options,
  filter,
  pick,
}: {
  options: ListOptions;
  filter: string;
  pick: Pick;
}) {
  const nav = useChatNav();
  const { channels, measured, viewer, nameOf, starred } = useChatFrame();
  const create = useWriteAccess("create_channel");
  const [collapsed, setCollapsed] = useState<Partial<Record<RoomSection, boolean>>>({});
  const current = nav.place.kind === "room" ? nav.place.id : "";
  const q = filter.trim().toLowerCase();
  const titleOf = (room: ChatChannelSummary) =>
    roomTitle(room.channel, { participants: room.participants, viewer, nameOf });

  // AN ARCHIVED ROOM IS NOT LISTED: it takes no new messages, so nothing in
  // it can wait for the reader, and it is still found by Browse rooms and
  // by search.
  const listed = channels
    .filter((room) => !room.channel.archived_at)
    .filter((room) => !q || titleOf(room).toLowerCase().includes(q))
    .filter(
      (room) => !options.unreadOnly || unreadIn(room, measured) || room.channel.id === current,
    );
  const ordered =
    options.sort === "name"
      ? [...listed].sort((a, b) => titleOf(a).localeCompare(titleOf(b)))
      : listed;

  const sections = ROOM_SECTIONS.map((key) => ({
    key,
    rooms: ordered.filter(
      (room) => roomSection(room.channel.kind, starred.has(room.channel.id)) === key,
    ),
  }));
  // A SECTION WITH AN ADD KEEPS ITS HEADING WHEN EMPTY, so the way to make
  // the first channel or conversation is there before there is one; while a
  // filter narrows the list, only what matched is drawn.
  const add = (key: RoomSection) =>
    key === "channels"
      ? {
          label: "New room",
          icon: <PlusGlyph size="sm" />,
          onClick: () => pick(() => nav.openDialog({ kind: "new-room" })),
        }
      : key === "direct"
        ? {
            label: "New message",
            icon: <PlusGlyph size="sm" />,
            onClick: () => pick(() => nav.go({ kind: "new" })),
          }
        : undefined;
  const shown = sections.filter(
    (section) =>
      section.rooms.length > 0 ||
      (!q &&
        !options.unreadOnly &&
        add(section.key) !== undefined &&
        (section.key !== "channels" || create.can)),
  );

  if (shown.length === 0) {
    return (
      <EmptyState
        size="compact"
        icon={<SearchGlyph size="xl" />}
        title="No matches"
        description={
          q
            ? `None of your rooms or direct messages match “${filter.trim()}”. Browse rooms to find one you have not joined.`
            : "None of your rooms has unread messages."
        }
      />
    );
  }

  return (
    <>
      {shown.map((section) => (
        <SidebarNav.Group
          key={section.key}
          label={SECTION_LABEL[section.key]}
          collapsible
          // A FILTER OPENS EVERY SECTION: what the reader typed is what they
          // are looking for, and a match folded away is a match not found.
          collapsed={!q && collapsed[section.key] === true}
          onCollapsedChange={(next: boolean) =>
            setCollapsed((was) => ({ ...was, [section.key]: next }))
          }
          action={add(section.key)}
        >
          {section.rooms.map((room) => (
            <RoomItem
              key={room.channel.id}
              room={room}
              title={titleOf(room)}
              current={room.channel.id === current}
              measured={measured}
              pick={pick}
            />
          ))}
        </SidebarNav.Group>
      ))}
    </>
  );
}

/** One room's row. */
function RoomItem({
  room,
  title,
  current,
  measured,
  pick,
}: {
  room: ChatChannelSummary;
  title: string;
  current: boolean;
  measured: boolean;
  pick: Pick;
}) {
  const nav = useChatNav();
  const drafts = useChatDrafts();
  const people = useChatPeople();
  const { viewer } = useChatFrame();
  const id = room.channel.id;
  const waiting = waitingIn(room, measured);
  const muted = room.muted === true;
  // A DRAFT SHOWS WHERE THE READER IS NOT: in the open room the words are in
  // the box in front of them.
  const draft = !current && drafts.has(id);
  // THE FULL NAME IS THE ROW'S TITLE, since a long room or group name is cut
  // in a narrow panel; a conversation with an agent adds what it is doing.
  const other = (room.participants ?? []).find((handle) => handle !== viewer);
  const doing = room.channel.kind === "dm" && other ? people.person(other).doing : "";
  const place: ChatPlace = { kind: "room", id };
  return (
    <SidebarNav.Item
      label={
        <span className="chat-room-label">
          <span className="chat-room-label-text">{title}</span>
          {draft && <PencilGlyph size="xs" className="chat-room-label-mark" title="Draft" />}
          {muted && <BellOffGlyph size="xs" className="chat-room-label-mark" title="Muted" />}
        </span>
      }
      icon={<RoomMark room={room} />}
      href={nav.hrefOf(place)}
      current={current}
      unread={unreadIn(room, measured)}
      muted={muted}
      title={doing ? `${title}. ${doing}` : title}
      onClick={(event) => {
        event.preventDefault();
        pick(() => nav.go(place));
      }}
      {...(waiting > 0
        ? {
            badge: {
              value: unreadLabel(waiting, room.unread_capped),
              label: isDirect(room.channel.kind) ? "unread" : "for you",
            },
          }
        : {})}
    />
  );
}

/** The find row's menu: narrow to unread rooms, and the order. */
function ListMenu({
  options,
  onChange,
}: {
  options: ListOptions;
  onChange: (next: ListOptions) => void;
}) {
  const changed = options.unreadOnly || options.sort !== DEFAULT_OPTIONS.sort;
  return (
    <Menu
      label="Filter and sort"
      align="end"
      renderTrigger={(props) => (
        <IconButton
          {...props}
          label="Filter and sort"
          size="md"
          variant="ghost"
          pressed={changed}
          icon={<ListFilterGlyph size="sm" />}
        />
      )}
      items={[
        {
          key: "unread",
          label: "Unread rooms only",
          checked: options.unreadOnly,
          onSelect: () => onChange({ ...options, unreadOnly: !options.unreadOnly }),
        },
        { kind: "separator", key: "order" },
        {
          key: "activity",
          label: "Sort by recent activity",
          checked: options.sort === "activity",
          onSelect: () => onChange({ ...options, sort: "activity" }),
        },
        {
          key: "name",
          label: "Sort by name",
          checked: options.sort === "name",
          onSelect: () => onChange({ ...options, sort: "name" }),
        },
      ]}
    />
  );
}

/**
 * Pins the panel beside the conversation, or unpins it back to the sheet
 * Menu slides in. At the end of the find row, so the panel spends no row on
 * a title: Menu, which opened it, already names it. A narrow window has no
 * room to pin, so there it is not offered.
 */
function DockToggle() {
  const nav = useChatNav();
  if (nav.narrow) return null;
  const docked = nav.rooms.docked;
  return (
    <Tooltip
      content={
        docked ? "Unpin, so the panel slides in from Menu" : "Pin the panel beside the conversation"
      }
    >
      <IconButton
        label={docked ? "Unpin the panel" : "Pin the panel"}
        size="md"
        variant="ghost"
        pressed={docked}
        icon={<PinGlyph size="sm" />}
        onClick={() => nav.rooms.dock(!docked)}
      />
    </Tooltip>
  );
}

/** What the rail's own answer says about itself, where the badges are. */
function RailNotes() {
  const { rail, channels, measured } = useChatFrame();
  if (!rail.data) return null;
  return (
    <>
      {rail.data.truncated && (
        <Callout variant="warning">
          You&rsquo;re in more rooms than this list holds ({channels.length}). The rest still open
          from a link and still show up in search.
        </Callout>
      )}
      {!measured && (
        <Callout variant="warning">
          Unread counts couldn&rsquo;t be read, so the badges are missing. That doesn&rsquo;t mean
          you&rsquo;re caught up.
        </Callout>
      )}
    </>
  );
}

function RoomsPanelBody({ onPick }: { onPick?: () => void }) {
  const nav = useChatNav();
  const { rail } = useChatFrame();
  const [filter, setFilter] = useState("");
  const [options, setOptions] = useState<ListOptions>(DEFAULT_OPTIONS);
  const pick: Pick = (run) => {
    if (!onPick) return run();
    onPick();
    window.setTimeout(run, 0);
  };
  const go = (place: ChatPlace) => pick(() => nav.go(place));
  return (
    <div className="chat-rooms">
      <div className="chat-rooms-find">
        <Input
          inputSize="md"
          leading={<SearchGlyph size="sm" />}
          placeholder="Find a room or person"
          aria-label="Find a room or person"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          onClear={() => setFilter("")}
        />
        <ListMenu options={options} onChange={setOptions} />
        <DockToggle />
      </div>
      <div className="chat-rooms-scroll">
        <RailNotes />
        <SidebarNav label="Rooms" dense>
          {!filter && <ViewItems pick={pick} />}
          {rail.data ? (
            <RoomSections options={options} filter={filter} pick={pick} />
          ) : (
            <RailState />
          )}
        </SidebarNav>
      </div>
      <div className="chat-rooms-foot">
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<CompassGlyph size="sm" />}
          onClick={() => go({ kind: "browse" })}
        >
          Browse rooms
        </Button>
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<SquarePenGlyph size="sm" />}
          onClick={() => go({ kind: "new" })}
        >
          New message
        </Button>
      </div>
    </div>
  );
}

/** The rail before its answer, or why there is none. */
function RailState() {
  const { rail } = useChatFrame();
  return (
    <QueryState
      error={rail.error}
      refusal={rail.refusal}
      detail={rail.detail}
      loading={rail.loading}
    />
  );
}

/** Slid in from the start edge of the chat area. */
export function RoomsSheet() {
  const nav = useChatNav();
  return (
    <Modal
      open
      onClose={nav.rooms.close}
      variant="sheet"
      side="start"
      flush
      className="chat-rooms-sheet"
      ariaLabel="Rooms"
      showCloseButton={false}
    >
      <RoomsPanelBody onPick={nav.rooms.close} />
    </Modal>
  );
}

/**
 * Pinned beside the conversation: the kit's `DockColumn`, as wide as the
 * panel while it is pinned and as wide as nothing while it is not, easing
 * between the two so the conversation resizes rather than jumps. The panel
 * keeps its own width, slides in and out from the start edge, and stays drawn
 * and inert until the column has closed over it.
 *
 * A PLAIN COLUMN, NOT A SECOND LANDMARK: the panel's own navigation inside it
 * is the one named Rooms, and two landmarks of one name are two a screen
 * reader offers with nothing to tell them apart.
 *
 * UNPINNED FROM INSIDE, focus goes to `back`: the top bar's Menu, which is
 * how the reader brings the panel back, rather than staying in a panel that
 * takes nothing.
 */
export function RoomsDock({
  docked,
  back,
}: {
  docked: boolean;
  /** The control that brings the panel back once it is unpinned. */
  back: () => HTMLElement | null;
}) {
  return (
    <DockColumn open={docked} width="var(--chat-dock)" className="chat-dock" focusOnClose={back}>
      <RoomsPanelBody />
    </DockColumn>
  );
}
