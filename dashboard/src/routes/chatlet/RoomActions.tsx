/**
 * What a reader does to the open room, at the end of the top bar: its
 * members, search, notifications and More.
 *
 * ASK CREWLET HAS ITS OWN BUTTON in the page bar, so the room offers catching
 * up through More (first, where it is found) rather than a second Ask
 * button. Catch up reads what is unread; with nothing unread, the same entry
 * summarizes the room.
 *
 * EVERY GESTURE HERE IS HELD AS THE ENGINE WOULD HOLD IT, by the room's one
 * reading of its rules (`scope.tsx`'s `roomRules`, the same the room, its
 * thread and its details read, and `directory.ts` for a leave): a unit's room
 * takes no leave and no added people, since its membership is the org
 * chart's; a conversation is the people in it; an archived room takes no new
 * people; a kind this build cannot classify takes nothing. A gesture the
 * engine would refuse is not offered, rather than offered and refused.
 */

import {
  AvatarStack,
  Button,
  IconButton,
  Menu,
  Tooltip,
  useToast,
  type MenuEntry,
  type MenuItem,
} from "@crewlethq/ui";
import {
  BellGlyph,
  BellOffGlyph,
  CheckGlyph,
  CopyGlyph,
  EllipsisGlyph,
  InfoGlyph,
  LinkGlyph,
  LogOutGlyph,
  PencilGlyph,
  SearchGlyph,
  StarGlyph,
  UserPlusGlyph,
} from "@crewlethq/icons/glyphs";
import { ProductMark } from "@crewlethq/icons/product-mark";

import { MAX_READER_MARKS } from "~/contract/chat.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { copyLink } from "~/lib/pageLink.ts";
import { useAct } from "~/lib/useAct.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";

import { canLeave } from "./directory.ts";
import { stackMember } from "./Face.tsx";
import { useChatFrame, useChatNav, useOpenRoom } from "./nav.tsx";
import { NOTIFY_LABEL, NOTIFY_MEANS, useNotifyLevel } from "./notify.tsx";
import { useChatPeople } from "./people.ts";
import { unreadIn } from "./rooms.ts";
import { useRoomScope } from "./scope.tsx";
import { calloutFor, reportFor } from "./start.ts";

/** Why a room cannot be starred once the reader holds the engine's cap of stars. */
const STARS_FULL = `You have starred ${MAX_READER_MARKS} rooms, the most one reader keeps. Remove a star to add this room.`;

/**
 * A menu item held with why, or the item itself where nothing holds it: the
 * sentence is drawn under its label, and the item takes no press.
 */
function held(item: MenuItem, reason: string): MenuItem {
  return reason ? { ...item, disabled: true, description: reason } : item;
}

/** How many faces the members chip draws before its count. */
const CHIP_FACES = 3;

export function RoomActions({ tight }: { tight: boolean }) {
  const nav = useChatNav();
  const open = useOpenRoom();
  const frame = useChatFrame();
  const people = useChatPeople();
  const toast = useToast();
  const notify = useNotifyLevel(open?.id ?? "");
  const marks = useWriteAccess("mark_chat_read");
  const leave = useAct("leave_channel");
  const { rules } = useRoomScope();
  if (!open) return null;

  const { id, channel, detail, summary } = open;
  const direct = channel ? isDirect(channel.kind) : false;
  const member = detail.data?.member === true;
  // WHAT THIS ROOM TAKES, from its own answer: nothing is offered on a guess
  // before it has come.
  const standing = detail.data
    ? { kind: detail.data.channel.kind, member, unit: detail.data.channel.unit }
    : null;
  const members = detail.data?.members ?? [];
  const starred = frame.starred.has(id);
  const starsFull = !starred && frame.starred.size >= MAX_READER_MARKS;
  const unread = summary ? unreadIn(summary, frame.measured) : false;
  const membersOpen =
    nav.pane?.kind === "details" && nav.pane.room === id && nav.pane.tab === "members";

  const search = () => nav.go({ kind: "search", room: id });

  const notifyItems: MenuEntry[] = (["all", "mentions", "muted"] as const).map((level) => ({
    key: `notify-${level}`,
    label: NOTIFY_LABEL[level],
    description: NOTIFY_MEANS[level],
    checked: notify.known && notify.level === level,
    disabled: !!notify.held || notify.busy,
    onSelect: () => notify.set(level),
  }));

  const copy = () =>
    void copyLink(nav.hrefOf({ kind: "room", id })).then((word) =>
      word === "Copied"
        ? toast.ok("Copied the link to this room")
        : toast.show({
            variant: "warning",
            message: "The browser blocked the clipboard, so the link was not copied.",
          }),
    );

  const leaveRoom = async () => {
    const result = await leave.run({ channel: id }, { done: `Left ${open.where}`, quiet: true });
    if (result === null) return;
    const answer = reportFor("leave", result);
    // LEAVING TAKES THE READER OUT OF THE ROOM, and a private room they have
    // just left is one they can no longer read at all, so staying on it
    // would turn into "no such room" under them a moment later.
    if (answer.tone === "success") {
      toast.ok(`You left ${open.where}`);
      nav.go({ kind: "activity" });
    } else {
      toast.show({ variant: calloutFor(answer.tone), message: answer.note });
    }
  };

  const more: MenuEntry[] = [
    // A TIGHT BAR KEEPS ONLY THE MEMBERS CHIP AND MORE, so Search and the
    // notification choices come here, first, where the buttons were.
    ...(tight
      ? [
          {
            key: "search",
            label: "Search this room",
            icon: <SearchGlyph size="sm" />,
            onSelect: search,
          },
          { kind: "separator" as const, key: "after-search" },
          ...notifyItems,
          { kind: "separator" as const, key: "after-notify" },
        ]
      : []),
    {
      key: "catchup",
      label: unread ? "Catch up with Ask Crewlet" : "Summarize with Ask Crewlet",
      icon: <ProductMark product="assistant" size="sm" />,
      onSelect: () =>
        nav.openPane({ kind: "catchup", room: id, mode: unread ? "catchup" : "summarize" }),
    },
    { kind: "separator", key: "after-ask" },
    {
      key: "details",
      label: direct ? "Conversation details" : "Room details",
      icon: <InfoGlyph size="sm" />,
      onSelect: () => nav.openPane({ kind: "details", room: id, tab: "about" }),
    },
    ...(!direct
      ? [
          {
            key: "shared",
            label: "Shared in this room",
            icon: <LinkGlyph size="sm" />,
            onSelect: () => nav.openPane({ kind: "details", room: id, tab: "shared" }),
          },
        ]
      : []),
    // A ROOM'S OWN GESTURES are drawn for every reader of a named room, and
    // held, with the sentence that says why, wherever the room's one reading
    // of its rules or the browser would refuse them: an option that vanished
    // would teach somebody the room cannot take it, which is not what is true.
    ...(!direct
      ? [
          held(
            {
              key: "people",
              label: "Add people",
              icon: <UserPlusGlyph size="sm" />,
              onSelect: () => nav.openDialog({ kind: "people", room: id }),
            },
            rules.members,
          ),
          held(
            {
              key: "description",
              label: channel?.purpose ? "Edit description" : "Add a description",
              icon: <PencilGlyph size="sm" />,
              onSelect: () => nav.openDialog({ kind: "description", room: id }),
            },
            rules.settings,
          ),
          held(
            {
              key: "topic",
              label: channel?.topic ? "Edit topic" : "Add a topic",
              icon: <PencilGlyph size="sm" />,
              onSelect: () => nav.openDialog({ kind: "topic", room: id }),
            },
            rules.settings,
          ),
        ]
      : []),
    // SAID, NOT GREYED: past the engine's cap a star is refused rather than
    // dropping an older one, and a press that only meets a refusal is a press
    // the reader should not have to make to find that out.
    held(
      {
        key: "star",
        label: starred ? "Remove from Starred" : "Add to Starred",
        icon: <StarGlyph size="sm" />,
        onSelect: () => frame.star(id, !starred),
      },
      marks.can ? (starsFull ? STARS_FULL : "") : marks.reason,
    ),
    {
      key: "read",
      label: "Mark as read",
      icon: <CheckGlyph size="sm" />,
      hint: "Esc",
      onSelect: () => frame.markRead(id),
    },
    {
      key: "copy",
      label: "Copy link",
      icon: <CopyGlyph size="sm" />,
      onSelect: copy,
    },
    // A LEAVE ONLY WHERE THE ROOM TAKES ONE (`directory.ts`): a unit's room
    // is the org chart's and a conversation is its people, and each says so
    // in the room's details rather than here.
    ...(standing && canLeave(standing)
      ? [
          { kind: "separator" as const, key: "before-leave" },
          held(
            {
              key: "leave",
              label: `Leave ${open.where}`,
              icon: <LogOutGlyph size="sm" />,
              danger: true,
              disabled: leave.busy,
              onSelect: () => void leaveRoom(),
            },
            leave.access.can ? "" : leave.access.reason,
          ),
        ]
      : []),
  ];

  return (
    <div className="chat-room-actions">
      {/* THE FACES AND THE TOTAL: the stack's own "+N" beside the total said
          the same number twice, so the stack is handed only the faces it
          draws. A COUNT ONLY WHERE THERE IS ONE: before the room's answer,
          "0 members" would be a fact nobody stated. */}
      {!direct && detail.data && (
        <Tooltip content="Members">
          <Button
            className="chat-members"
            variant="secondary"
            size="medium"
            leadingIcon={
              <AvatarStack
                members={members
                  .slice(0, CHIP_FACES)
                  .map((m) => stackMember(people.person(m.handle)))}
                max={CHIP_FACES}
                size="xs"
                decorative
              />
            }
            pressed={membersOpen}
            aria-label={`${members.length} member${members.length === 1 ? "" : "s"}`}
            onClick={() => nav.openPane({ kind: "details", room: id, tab: "members" })}
          >
            {members.length}
          </Button>
        </Tooltip>
      )}
      {!tight && (
        <Tooltip content="Search this room">
          <IconButton
            label="Search this room"
            size="md"
            variant="ghost"
            icon={<SearchGlyph size="sm" />}
            onClick={search}
          />
        </Tooltip>
      )}
      {!tight && (
        <Menu
          label="Notifications"
          align="end"
          renderTrigger={(props) => (
            <IconButton
              {...props}
              label="Notifications"
              size="md"
              variant="ghost"
              icon={notify.level === "muted" ? <BellOffGlyph size="sm" /> : <BellGlyph size="sm" />}
            />
          )}
          items={notifyItems}
        />
      )}
      <Menu
        label="More"
        align="end"
        renderTrigger={(props) => (
          <IconButton
            {...props}
            label="More"
            size="md"
            variant="ghost"
            icon={<EllipsisGlyph size="sm" />}
          />
        )}
        items={more}
      />
    </div>
  );
}
