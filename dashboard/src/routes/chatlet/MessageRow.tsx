/**
 * One message, drawn on the kit's `ChatMessage`: the face, the author line,
 * the body, the previews, the reactions, the thread line under a message that
 * has replies, and the tray of actions over it on hover or keyboard focus.
 *
 * ONE ROW FOR EVERY PLACE A MESSAGE IS DRAWN, the room, a thread and a view
 * that quotes one, so a message reads one way wherever it is met. Where it is
 * drawn changes only what it offers: a reply offers no thread of its own (a
 * thread is ONE LEVEL DEEP), and a quoted message in a view offers what a
 * reader may do from there, never a reaction it would have to read the room
 * to place.
 *
 * EVERY CONTROL IS DRAWN AND HELD WITH ITS SENTENCE, never hidden: the room's
 * rule for its change ([RoomScope.rules]), then the browser's reason for its
 * tool. A control that vanished would read as a permission this reader
 * lacks. Edit is offered on the reader's own messages only, and Delete on
 * theirs and, to an admin, on everybody's: the authority table's rules,
 * which the engine judges against the stored author.
 *
 * THE KEYS ON A FOCUSED MESSAGE: E edits it, T opens its thread, S saves it,
 * Delete deletes it, and Up and Down move to the message above and below.
 */

import { memo, useState, useSyncExternalStore, type KeyboardEvent } from "react";

import {
  AddReaction,
  ChatMessage,
  DeliveryReceipt,
  EmojiPicker,
  Menu,
  MessageAction,
  MessageActions,
  QuickReaction,
  ReactionBar,
  Tag,
  ThreadSummary,
  Tooltip,
  WorkingLine,
  emojiName,
  useToast,
  type MenuEntry,
} from "@crewlethq/ui";
import {
  BookmarkGlyph,
  CopyGlyph,
  EllipsisGlyph,
  EyeOffGlyph,
  FaceSlightlySmilingPlusGlyph,
  LinkGlyph,
  MessagesSquareGlyph,
  PencilGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { matchesRow } from "~/app/keymap.ts";
import { useAssistantControl } from "~/app/assistant/control.ts";
import { useClock } from "~/lib/clock.ts";
import { fmtDateTimeFull, relTime } from "~/lib/format.ts";
import { copyLink, copyText } from "~/lib/pageLink.ts";
import type { ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { reactionName, reactionShown, shortcodeOf } from "./emoji.ts";
import { MessageBody } from "./Body.tsx";
import { InlineEditor } from "./Editor.tsx";
import { packPosition } from "./cursor.ts";
import { clockOf, timeOf } from "./days.ts";
import { ChatFace, stackMember } from "./Face.tsx";
import { useSliceValue } from "~/lib/store-hooks.ts";
import { workingAgents } from "./live.ts";
import { MessageUnfurls } from "./MessageLinks.tsx";
import { useChatNav, type ChatPane, type ChatPlace } from "./nav.tsx";
import { useChatNames, useChatPerson } from "./people.ts";
import { reachRecipients, showsReceipt } from "./receipts.ts";
import { EventRow } from "./RoomIntro.tsx";
import { useRoomScope } from "./scope.tsx";

/** The three reactions the tray offers at one press. */
export const QUICK_REACTIONS = ["✅", "👀", "🙌"] as const;

/**
 * Where a link to a message opens: the room at the message, and for a reply
 * the room at its thread's root with the thread open beside it, since a room
 * page holds the room's own posts and never a reply.
 */
export function messageAddress(
  channelID: string,
  message: Pick<ChatMessageView["message"], "id" | "thread_root">,
): { place: ChatPlace; pane: ChatPane | null } {
  const root = message.thread_root;
  return root
    ? {
        place: { kind: "room", id: channelID, message: root },
        pane: { kind: "thread", room: channelID, root },
      }
    : { place: { kind: "room", id: channelID, message: message.id }, pane: null };
}

/** Where a row is drawn, which decides what it offers. */
export type RowPlace = "room" | "root" | "thread" | "context";

export interface MessageRowProps {
  view: ChatMessageView;
  /** The same author went on: no face and no name, the time in the gutter. */
  continuation?: boolean;
  /**
   * `room`: everything. `root`: a thread's first message, with no thread line
   * of its own. `thread`: a reply. `context`: a message quoted in a view.
   */
  where?: RowPlace;
  /** The seats answering under this message (`ChatWorking.thread`). */
  working?: readonly ChatWorking[];
  /** Drawn flashed: a link, a mention or a search landed on it. */
  flash?: boolean;
}

/**
 * MEMOIZED: a row renders when its own message, its place or the seats
 * answering under it change, never because the transcript around it did (a
 * scroll re-windowing it, a page landing below it).
 */
export const MessageRow = memo(function MessageRow({
  view,
  continuation = false,
  where = "room",
  working = NO_WORKING,
  flash = false,
}: MessageRowProps) {
  const { message } = view;
  if (message.event) return <EventRow view={view} />;
  return (
    <SpokenRow
      view={view}
      continuation={continuation}
      where={where}
      working={working}
      flash={flash}
    />
  );
});

const NO_WORKING: readonly ChatWorking[] = [];

function SpokenRow({ view, continuation, where, working, flash }: Required<MessageRowProps>) {
  const { message } = view;
  const scope = useRoomScope();
  const nav = useChatNav();
  const names = useChatNames();
  const isAgent = (handle: string) => names.person(handle).kind === "agent";
  const marks = scope.marks;
  // THE PLACE THIS ROW'S THREAD WAS READ TO, the one fact the row needs from
  // the read state, so it renders when that place moves and not with every
  // other mark the reader makes.
  const threadReadHeld = useSliceValue(["chatRead"], (state) =>
    where === "room" && view.thread
      ? Math.max(
          state.chatRead?.thread_cursors?.[view.message.id] || 0,
          state.chatRead?.cursors?.[scope.channelID] ?? 0,
        )
      : 0,
  );
  // AND WHAT THIS TAB HAS READ THERE, which the engine learns at the next
  // flush: a reply the reader just wrote or read in the pane is not new.
  const { cursors } = scope;
  const threadSeen = useSyncExternalStore(cursors.watch, () =>
    where === "room" && view.thread ? cursors.threadSeen(view.message.id) : 0,
  );
  const threadRead = Math.max(threadReadHeld, threadSeen);
  const toast = useToast();
  const assistant = useAssistantControl();
  // THE THREAD LINE'S AGE, read as its words: the row renders when "5m ago"
  // becomes "6m ago", not on every tick of the clock.
  const lastReply = useClock((now) =>
    where === "room" && view.thread ? relTime(view.thread.last_reply_at, now) : "",
  );
  // A MENU OR A PICKER OPEN FROM THE TRAY holds the row in its pointed state,
  // so the tray does not vanish from under the menu it opened.
  const [busy, setBusy] = useState(0);
  const hold = (open: boolean) => setBusy((n) => Math.max(0, n + (open ? 1 : -1)));

  const author = useChatPerson(message.author, message.author_kind);
  const mine = message.author === scope.viewer;
  const deleted = !!message.deleted_at;
  const editing = scope.editing === message.id;
  const saved = marks.isSaved(message.id);
  const quoted = where === "context";
  const named = !mine && (message.collective || (message.mentions ?? []).includes(scope.viewer));
  const threadOpen =
    nav.pane?.kind === "thread" && nav.pane.root === (message.thread_root || message.id);

  // EACH CHANGE HELD BY ITS OWN RULE, then the browser's reason for its tool.
  const reactHold =
    scope.rules.react || (scope.writes.react.access.can ? "" : scope.writes.react.access.reason);
  const editHold =
    scope.rules.edit || (scope.writes.edit.access.can ? "" : scope.writes.edit.access.reason);
  const removeHold =
    scope.rules.remove || (scope.writes.remove.access.can ? "" : scope.writes.remove.access.reason);

  // A GESTURE NAMES AN EMOJI AS DRAWN (a picked glyph, or a pill's face); the
  // write names it as stored, a shortcode (`emoji.ts`).
  const react = (shown: string) => {
    if (reactHold) return;
    const reactions = view.reactions ?? [];
    const code = shortcodeOf(shown);
    const held =
      reactions.find((r) => r.emoji === code) ??
      reactions.find((r) => reactionShown(r.emoji) === shown);
    scope.react(message.id, held?.emoji ?? code, !!held?.mine);
  };
  const openThread = () => nav.openThread(scope.channelID, message.thread_root || message.id);
  const save = () => {
    // SAVED IS FULL: said, rather than a write the engine would refuse.
    if (!saved && marks.saveHold) {
      toast.show({ variant: "info", message: marks.saveHold });
      return;
    }
    void marks.save(scope.channelID, message.id, !saved);
  };
  const askDelete = () =>
    nav.openDialog({ kind: "delete", channelID: scope.channelID, message: view });
  const address = messageAddress(scope.channelID, message);
  const link = nav.hrefOf(address.place, address.pane);
  const copied = (what: "link" | "text") => (said: "Copied" | "Blocked") =>
    toast.show(
      said === "Copied"
        ? {
            variant: "success",
            message: what === "link" ? "Link copied" : "Text copied",
            duration: 1600,
          }
        : { variant: "info", message: `This browser did not allow copying the ${what}.` },
    );
  const copyMessageLink = () => void copyLink(link).then(copied("link"));
  const copyMessageText = () => void copyText(message.body ?? "").then(copied("text"));

  // THE KEYMAP'S ROWS, NEVER A KEY SPELLED HERE (`app/keymap.ts`, "On a
  // focused chat message"), and only while the row itself has the focus.
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.target !== event.currentTarget || deleted || editing) return;
    const press = event.nativeEvent;
    if (matchesRow("message.edit", press) && mine && !editHold) {
      event.preventDefault();
      scope.setEditing(message.id);
    } else if (matchesRow("message.thread", press) && where === "room") {
      event.preventDefault();
      openThread();
    } else if (matchesRow("message.save", press)) {
      event.preventDefault();
      save();
    } else if (matchesRow("message.delete", press) && (mine || scope.moderates)) {
      event.preventDefault();
      if (!removeHold) askDelete();
    }
  };

  // THE MENU READS AS THE MESSAGE'S OWN FIRST: the reader's own change, then
  // what they keep or take from it, then a question about it, and a delete
  // last, apart from everything else.
  const more: MenuEntry[] = [
    ...(mine
      ? [
          {
            key: "edit",
            label: "Edit message",
            icon: <PencilGlyph size="sm" />,
            hint: "E",
            disabled: !!editHold,
            ...(editHold ? { description: editHold } : {}),
            onSelect: () => scope.setEditing(message.id),
          },
          { kind: "separator" as const, key: "edited" },
        ]
      : []),
    ...(quoted || mine
      ? []
      : [
          {
            key: "unread",
            label: "Mark unread",
            icon: <EyeOffGlyph size="sm" />,
            onSelect: () => void marks.rewind(scope.channelID, view),
          },
        ]),
    { key: "copy", label: "Copy link", icon: <LinkGlyph size="sm" />, onSelect: copyMessageLink },
    ...(message.body
      ? [
          {
            key: "copy-text",
            label: "Copy text",
            icon: <CopyGlyph size="sm" />,
            onSelect: copyMessageText,
          },
        ]
      : []),
    { kind: "separator" as const, key: "asking" },
    {
      key: "ask",
      label: "Ask Crewlet about this",
      icon: <ProductMark product="assistant" size="sm" />,
      disabled: assistant.unavailable !== null,
      ...(assistant.unavailable ? { description: assistant.unavailable.short } : {}),
      // THE MESSAGE BY ITS ADDRESS, NEVER ITS WORDS: the body is somebody
      // else's, and typed into the person's own question it would speak as
      // them. The assistant reads the message itself, fenced as words
      // somebody else wrote.
      onSelect: () =>
        assistant.ask(
          `Help me with this message from ${author.name}: ${location.origin}${location.pathname}${link}`,
        ),
    },
    ...(mine || scope.moderates
      ? [
          { kind: "separator" as const, key: "own" },
          {
            key: "delete",
            label: "Delete message",
            icon: <TrashGlyph size="sm" />,
            danger: true,
            hint: "Del",
            disabled: !!removeHold,
            ...(removeHold ? { description: removeHold } : {}),
            onSelect: askDelete,
          },
        ]
      : []),
  ];

  const actions =
    deleted || editing ? null : (
      <MessageActions>
        {!quoted &&
          QUICK_REACTIONS.map((emoji) => (
            <QuickReaction
              key={emoji}
              emoji={emoji}
              name={emojiName(emoji)}
              onPick={() => react(emoji)}
              disabledReason={reactHold || undefined}
            />
          ))}
        {!quoted && (
          <EmojiPicker
            entrance="instant"
            label="Add a reaction"
            tooltip="Add a reaction"
            onPick={react}
            onOpenChange={hold}
            trigger={(_open, toggle) => (
              <MessageAction
                label="Add a reaction"
                icon={<FaceSlightlySmilingPlusGlyph size="md" />}
                disabledReason={reactHold || undefined}
                onClick={toggle}
              />
            )}
          />
        )}
        {where === "room" && (
          <MessageAction
            label="Reply in thread"
            icon={<MessagesSquareGlyph size="md" />}
            onClick={openThread}
          />
        )}
        <MessageAction
          label={saved ? "Remove from Saved" : "Save for later"}
          pressed={saved}
          icon={<BookmarkGlyph size="md" />}
          // SAVED IS FULL: held with why, rather than a press the engine
          // would refuse. Taking one out of Saved is never held.
          disabledReason={(!saved && marks.saveHold) || undefined}
          onClick={save}
        />
        <Menu
          label="More actions"
          align="end"
          activeDescendant
          entrance="instant"
          onOpenChange={hold}
          renderTrigger={(props) => (
            <MessageAction {...props} label="More actions" icon={<EllipsisGlyph size="md" />} />
          )}
          items={more}
        />
      </MessageActions>
    );

  const thread = where === "room" ? view.thread : undefined;
  const newReplies =
    thread && !threadOpen && (packPosition(thread.last_reply_position) ?? 0) > threadRead;
  const answering = working.filter((seat) => seat.thread === message.id);
  // EVERY SEAT ANSWERING, on one line. While the thread is open beside the
  // room, the pane's own line announces them, so this one stays quiet.
  const answeringLine =
    answering.length > 0 ? (
      <WorkingLine agents={workingAgents(answering, names.person)} announce={!threadOpen} />
    ) : undefined;

  const footer = (
    <>
      {!deleted && !editing && <MessageUnfurls links={message.links} />}
      {!deleted && (view.reactions ?? []).length > 0 && (
        <ReactionBar
          reactions={(view.reactions ?? []).map((r) => ({
            emoji: reactionShown(r.emoji),
            name: reactionName(r.emoji),
            count: r.count,
            mine: !!r.mine,
          }))}
          onToggle={react}
          disabledReason={reactHold || undefined}
          add={
            quoted ? undefined : (
              <AddReaction
                onPick={react}
                onOpenChange={hold}
                disabledReason={reactHold || undefined}
              />
            )
          }
        />
      )}
      {thread && thread.replies > 0 && (
        <ThreadSummary
          replies={thread.replies}
          participants={thread.repliers.map((handle) => stackMember(names.person(handle)))}
          last={lastReply}
          unread={newReplies ? 1 : 0}
          open={threadOpen}
          onOpen={openThread}
          working={answeringLine}
        />
      )}
      {(!thread || thread.replies === 0) && answeringLine}
      {/* WHOM A PERSON'S MESSAGE REACHED, AND WHO ANSWERED, under its words:
          the post's own routing as the engine kept it. */}
      {showsReceipt(message, isAgent) && (
        <DeliveryReceipt
          recipients={reachRecipients(message.reach, view.answered, names.nameOf)}
          truncated={message.reach?.truncated}
        />
      )}
    </>
  );

  return (
    <ChatMessage
      id={message.id}
      label={`${author.name}, ${timeOf(message.created_at)}`}
      avatar={
        <button
          type="button"
          aria-label={`Open ${author.name}'s profile`}
          onClick={() => nav.openPane({ kind: "profile", handle: author.handle })}
        >
          <ChatFace person={author} size={36} />
        </button>
      }
      author={
        <button
          type="button"
          onClick={() => nav.openPane({ kind: "profile", handle: author.handle })}
        >
          {author.name}
        </button>
      }
      badges={
        <>
          {author.kind === "agent" && <Tag appearance="quiet">Agent</Tag>}
          {saved && !quoted && (
            <span className="chat-saved-flag">
              <BookmarkGlyph size="xs" /> Saved
            </span>
          )}
        </>
      }
      time={timeOf(message.created_at)}
      gutterTime={clockOf(message.created_at)}
      timeTitle={fmtDateTimeFull(message.created_at)}
      dateTime={message.created_at}
      continuation={continuation && !saved}
      highlight={named && !quoted ? "mention" : null}
      active={busy > 0}
      flash={flash}
      editing={editing}
      actions={actions}
      footer={footer}
      onKeyDown={onKeyDown}
    >
      {deleted ? (
        // THE ROW SURVIVES SO THE THREAD DOES: a delete blanks the body and
        // keeps the message, which is what lets a reply still resolve to what
        // it answered.
        <p className="chat-deleted">
          This message was deleted
          {message.deleted_by && message.deleted_by !== message.author
            ? ` by ${names.nameOf(message.deleted_by)}`
            : ""}
          .
        </p>
      ) : editing ? (
        <InlineEditor view={view} />
      ) : (
        <MessageBody
          message={message}
          viewer={scope.viewer}
          nameOf={names.nameOf}
          onPerson={(handle) => nav.openPane({ kind: "profile", handle })}
          after={
            message.edited_at ? (
              // A STOP AMONG THE ROW'S OWN CONTROLS, as the row's times are, so
              // a keyboard reader can read when it was edited.
              <Tooltip content={`Edited ${fmtDateTimeFull(message.edited_at)}`}>
                <span className="chat-edited" tabIndex={0}>
                  (edited)
                </span>
              </Tooltip>
            ) : null
          }
        />
      )}
    </ChatMessage>
  );
}
