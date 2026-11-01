/**
 * What every message drawn in one room shares: who is reading, what the room
 * refuses them, and the presses its messages' changes are made through.
 *
 * ONE SCOPE PER ROOM, provided once and read by every row of it, wherever the
 * row is drawn: the transcript, the thread beside it, a view that quotes a
 * message (Activity, Saved, Threads). A row that worked these out for itself
 * would be a second reading of the room's rules, and the two would come to
 * disagree about whether a reaction may be pressed.
 *
 * # What this room refuses, and why it says so rather than greying out
 *
 * Three things close a room to new messages, and they are different facts:
 * the room is archived (readable for ever, taking no new messages), the reader
 * is not a member of a private room, or this build cannot classify the room's
 * kind at all, which the engine treats as refusing every write because a kind
 * read two-valued falls out as "not private". A fourth is about the BROWSER
 * rather than the room: every write here is a change made through the act
 * transport as the session's principal (ADR-0024), so offline, or as a
 * credential acting as no seat, nothing can be sent, and that sentence is
 * `useWriteAccess`'s for the tool a control presses.
 *
 * THOSE ARE THE RULES FOR A MESSAGE, and a message's own changes have rules of
 * their own, mirrored from the engine's write path one per tool: a reaction is
 * refused outside a private room as a post is; an edit is its author's and a
 * delete its author's or an admin's, both reaching past membership; an archive
 * closes a room to new messages and to nothing else; and a kind this build
 * cannot classify takes none of them. Each control is drawn and held with its
 * own sentence, never hidden.
 *
 * # A write is read back, never drawn ahead
 *
 * Every change lands on the chat log and raises this tab's read floor for it
 * (`protocol/session.ts`), so the room is asked again at that floor and
 * redrawn from an answer that includes the change. Nothing here patches a row.
 */

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";

import { pressable } from "~/components/WriteButton.tsx";
import { MAX_READER_MARKS } from "~/contract/chat.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useAct, type Act } from "~/lib/useAct.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import { useWriteAccess, type WriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  ActResult,
  ChatChannel,
  ChatChannelDetail,
  ChatMessageView,
} from "~/protocol/index.ts";

import { markRead, packPosition } from "./cursor.ts";
import { useChatRead } from "~/lib/chatLive.ts";
import { useChatFrame, useOpenRoom } from "./nav.tsx";
import type { OutboxHandle } from "./outbox.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { knownKind, NEWER_ROOM } from "./rooms.ts";

/** Why an archived room takes no new messages. */
export const ARCHIVED =
  "This room is archived. You can still read it and edit, delete or react to what’s in it, but it takes no new messages.";

/** Why a private room the reader is outside takes nothing from them. */
export const OUTSIDER =
  "This is a private room you’re not in, so you can’t post or react here. An admin can still take a message down.";

/** What every control says before the room's own answer has come. */
export const READING = "Reading the room before anything in it can change.";

/** What every control says when the room's own answer did not come. */
export const UNREAD =
  "This room couldn’t be read, so nothing in it can change. The reason is at the top.";

/** The presses a message's changes are made through. */
export interface MessageWrites {
  react: Act<"react_to_message">;
  edit: Act<"edit_message">;
  remove: Act<"delete_message">;
}

/**
 * Why the ROOM refuses each change from this reader, "" where it takes it,
 * ONE PER CHANGE, because the engine's rule is one per tool.
 */
export interface MessageRules {
  /** `post_message` and `reply_in_thread`: closes the composer. */
  post: string;
  /** `react_to_message`. */
  react: string;
  /** `edit_message`. */
  edit: string;
  /** `delete_message`. */
  remove: string;
  /**
   * `update_channel`: a topic, a description, a template. Taken on an
   * ARCHIVED room, which keeps its words for whoever reads it after.
   */
  settings: string;
  /**
   * `add_channel_members`. Never on a unit's room, whose membership is the
   * org chart's, nor on a direct conversation, which is the people in it, nor
   * on an archived room, and only from inside the room.
   */
  members: string;
}

/** Why a unit's room takes no members by hand. */
export const ORG_MEMBERS = "The org chart decides who is in a team’s room.";

/** Why a direct conversation takes no members. */
export const DIRECT_MEMBERS =
  "A direct conversation is the people in it. Start a new one to add someone.";

/** Why a room takes no members from somebody outside it. */
export const OUTSIDE_MEMBERS = "Join the room to add people to it.";

/**
 * The room's rules for each change, read off its detail.
 *
 * A ROOM NOT YET READ TAKES NOTHING YET: which refusal it makes, if any, is the
 * room's own answer, and a control offered before it arrived would be offered
 * on a guess. AND ONE WHOSE READ FAILED SAYS SO rather than claiming to be
 * reading: `useQuery` asks again only after `unavailable`, so every other
 * failure would hold every control "reading" for ever.
 */
export function roomRules(
  channel: ChatChannel | undefined,
  detail: Pick<QueryResult<ChatChannelDetail>, "data" | "error"> | null,
): MessageRules {
  if (!channel) {
    const waiting = detail?.error ? UNREAD : READING;
    return {
      post: waiting,
      react: waiting,
      edit: waiting,
      remove: waiting,
      settings: waiting,
      members: waiting,
    };
  }
  const archived = !!channel.archived_at;
  const unknown = !knownKind(channel.kind);
  const outsider = channel.kind === "private" && detail?.data?.member === false;
  const inside = detail?.data?.member === true;
  return {
    post: archived ? ARCHIVED : unknown ? NEWER_ROOM : outsider ? OUTSIDER : "",
    react: unknown ? NEWER_ROOM : outsider ? OUTSIDER : "",
    edit: unknown ? NEWER_ROOM : "",
    remove: unknown ? NEWER_ROOM : "",
    settings: unknown ? NEWER_ROOM : outsider ? OUTSIDER : "",
    members: unknown
      ? NEWER_ROOM
      : channel.kind === "unit"
        ? ORG_MEMBERS
        : isDirect(channel.kind)
          ? DIRECT_MEMBERS
          : archived
            ? ARCHIVED
            : !inside
              ? OUTSIDE_MEMBERS
              : "",
  };
}

/**
 * The room's rule for a change, then the browser's reason for its tool: the
 * one sentence a control that opens the change is held with.
 */
function held(rule: string, access: WriteAccess): string {
  return rule || (access.can ? "" : access.reason);
}

/** What every row of one room reads. */
export interface RoomScope {
  channelID: string;
  /** The seat this tab reads as. */
  viewer: string;
  nameOf: (handle: string) => string;
  /**
   * Whether this reader MODERATES, may take anybody's message down, which the
   * authority table gives an admin and nobody else. Read off the viewer's own
   * permissions, never decided here.
   */
  moderates: boolean;
  /** Whether the reader is in the room, as the room's own detail says. */
  member: boolean;
  /** The room's detail, when it has been read. */
  channel: ChatChannel | undefined;
  rules: MessageRules;
  writes: MessageWrites;
  /** React, or take a reaction back. Quiet: a toast per emoji would be louder than the emoji. */
  react: (messageID: string, emoji: string, removing: boolean) => void;
  /**
   * Rewrite one's own message. Resolves with whether the engine took it,
   * applied or pending, which is what closes the editor: a rewrite that was
   * refused stays in front of the person who typed it.
   */
  edit: (messageID: string, body: string) => Promise<boolean>;
  /** The message open in its row's editor, or "". One at a time per room. */
  editing: string;
  setEditing: (messageID: string) => void;
  /**
   * Close a message's editor if it is still the one open: a save that lands
   * after the reader moved on to another message closes nothing of theirs.
   */
  stopEditing: (messageID: string) => void;
  /** The screen's outbox, which outlives the room. */
  outbox: OutboxHandle;
}

const RoomScopeContext = createContext<RoomScope | null>(null);

/** The scope of the room a row is drawn in. Throws outside one: a wiring bug. */
export function useRoomScope(): RoomScope {
  const scope = useContext(RoomScopeContext);
  if (!scope) throw new Error("useRoomScope outside a RoomScopeProvider");
  return scope;
}

/**
 * Provide one room's scope. Under a provider for the SAME room it adds
 * nothing, so the room, its thread pane and its details can each ask for it
 * without making a second set of presses.
 *
 * `detail` is for a room that is not the open one (a view quoting a message
 * from elsewhere); the open room's is the frame's, read once.
 */
export function RoomScopeProvider({
  channelID,
  detail,
  children,
}: {
  channelID: string;
  detail?: Pick<QueryResult<ChatChannelDetail>, "data" | "error"> | null;
  children: ReactNode;
}) {
  const outer = useContext(RoomScopeContext);
  if (outer && outer.channelID === channelID) return <>{children}</>;
  return (
    <OwnScope channelID={channelID} detail={detail ?? null}>
      {children}
    </OwnScope>
  );
}

function OwnScope({
  channelID,
  detail: given,
  children,
}: {
  channelID: string;
  detail: Pick<QueryResult<ChatChannelDetail>, "data" | "error"> | null;
  children: ReactNode;
}) {
  const { viewer, nameOf, outbox } = useChatFrame();
  const open = useOpenRoom();
  const moderates = useViewer().admin;
  const detail = open && open.id === channelID ? open.detail : given;
  const channel = detail?.data?.channel ?? (open?.id === channelID ? open.channel : undefined);
  // THE TWO RULES ABOUT THE ROOM ITSELF hold their opener by the browser's
  // reason too: a dialog whose Save could never be pressed is a dialog
  // nobody should have to open to find that out.
  const settingsAccess = useWriteAccess("update_channel");
  const membersAccess = useWriteAccess("add_channel_members");
  const room = roomRules(channel, detail);
  const rules: MessageRules = {
    ...room,
    settings: held(room.settings, settingsAccess),
    members: held(room.members, membersAccess),
  };

  const react = useAct("react_to_message");
  const edit = useAct("edit_message");
  const remove = useAct("delete_message");
  const [editing, setEditing] = useState("");
  const stopEditing = useCallback(
    (messageID: string) => setEditing((now) => (now === messageID ? "" : now)),
    [],
  );

  const onReact = useCallback(
    (messageID: string, emoji: string, removing: boolean) => {
      // ACCESS, NOT BUSY: two chips are two independent toggles, and one
      // still in flight is no reason to drop a press on the other.
      if (!react.access.can) return;
      void react.run(
        { channel: channelID, message_id: messageID, emoji, remove: removing },
        { done: removing ? "Took the reaction back" : "Reacted", quiet: true },
      );
    },
    [react, channelID],
  );
  const onEdit = useCallback(
    async (messageID: string, body: string) => {
      // THE BUTTON'S OWN GATE, because a form submits on its own: a second
      // submit while the first answer was out would rewrite the message again
      // under a new operation key.
      if (!pressable(edit)) return false;
      const result = await edit.run(
        { channel: channelID, message_id: messageID, body },
        { done: "Edited the message" },
      );
      return result !== null && (result.kind === "applied" || result.kind === "pending");
    },
    [edit, channelID],
  );

  const member = detail?.data?.member === true;
  const scope = useMemo<RoomScope>(
    () => ({
      channelID,
      viewer,
      nameOf,
      moderates,
      member,
      channel,
      rules,
      writes: { react, edit, remove },
      react: onReact,
      edit: onEdit,
      editing,
      setEditing,
      stopEditing,
      outbox,
    }),
    // `rules` is rebuilt every render from the values below it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [
      channelID,
      viewer,
      nameOf,
      moderates,
      member,
      channel,
      rules.post,
      rules.react,
      rules.edit,
      rules.remove,
      rules.settings,
      rules.members,
      react,
      edit,
      remove,
      onReact,
      onEdit,
      editing,
      outbox,
    ],
  );
  return <RoomScopeContext.Provider value={scope}>{children}</RoomScopeContext.Provider>;
}

/** What the reader keeps for themselves about messages, and the presses that keep it. */
export interface ReaderMarks {
  /** Whether a message is saved for later. */
  isSaved: (messageID: string) => boolean;
  /**
   * Why saving one more message is refused, or "": Saved holds the engine's
   * cap ([MAX_READER_MARKS]) and refuses a save past it rather than dropping
   * an older one, so the press says so instead of sending a refused write.
   */
  saveHold: string;
  /** Save a message for later, or take it out of Saved. Says what it did. */
  save: (channelID: string, messageID: string, saving: boolean) => Promise<boolean>;
  /**
   * Put a room's place back to just before a message: the one way the place
   * moves back, set exactly. The cursor writer's advance-only rule then keeps
   * it there until the reader reads past what they had already read.
   */
  rewind: (channelID: string, view: ChatMessageView) => Promise<boolean>;
}

/**
 * The reader's own marks, read off the read state the screen already holds
 * (the rail's `read`, moved by every `chat_cursor_moved` push) and written
 * through `mark_chat_read`, whose answer is applied as the push would be
 * ([markRead]).
 */
export function useReaderMarks(): ReaderMarks {
  const { store } = useClient();
  const read = useChatRead();
  const mark = useAct("mark_chat_read");
  const saved = useMemo(() => new Set((read?.saved ?? []).map((s) => s.message_id)), [read?.saved]);
  const saveHold = saved.size >= MAX_READER_MARKS ? FULL_SAVED : "";
  return useMemo<ReaderMarks>(
    () => ({
      isSaved: (id) => saved.has(id),
      saveHold,
      save: async (channelID, messageID, saving) => {
        const which = [{ channel_id: channelID, message_id: messageID }];
        const result = await markRead(mark, store, saving ? { save: which } : { unsave: which }, {
          done: saving ? "Saved for later" : "Removed from Saved",
        });
        return landed(result);
      },
      rewind: async (channelID, view) => {
        const at = packPosition(view.position);
        if (at === null) return false;
        const result = await markRead(
          mark,
          store,
          { rewind: { [channelID]: at - 1 } },
          { done: "Marked unread from here" },
        );
        return landed(result);
      },
    }),
    [saved, saveHold, mark, store],
  );
}

/** What Save says once Saved holds as many messages as the engine keeps. */
export const FULL_SAVED = `Saved holds ${MAX_READER_MARKS} messages, the most it keeps. Take one out of Saved to save this one.`;

function landed(result: ActResult | null): boolean {
  return result !== null && (result.kind === "applied" || result.kind === "pending");
}
