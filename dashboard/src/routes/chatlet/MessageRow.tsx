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

import { useEffect, useRef, useState, type KeyboardEvent } from "react";

import {
  ActionDivider,
  AddReaction,
  ChatMessage,
  Composer as KitComposer,
  EmojiPicker,
  Menu,
  MessageAction,
  MessageActions,
  QuickReaction,
  ReactionBar,
  Tag,
  ThinkingIndicator,
  ThreadSummary,
  Button,
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
  MessagesSquareGlyph,
  PencilGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { matchesRow } from "~/app/keymap.ts";
import { useAssistantControl } from "~/app/assistant/control.ts";
import { pressable } from "~/components/WriteButton.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { copyLink } from "~/lib/pageLink.ts";
import type { ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { MessageBody } from "./Body.tsx";
import { bodyBudget, bodyRefusal, BodyField, SHORTEN_FIRST } from "./Composer.tsx";
import { packPosition } from "./cursor.ts";
import { clockOf, timeOf } from "./days.ts";
import { ChatFace, stackMember } from "./Face.tsx";
import { useChatRead } from "~/lib/chatLive.ts";
import { MessageUnfurls } from "./MessageLinks.tsx";
import { useChatNav } from "./nav.tsx";
import { useChatPeople } from "./people.ts";
import { EventRow } from "./RoomIntro.tsx";
import { useReaderMarks, useRoomScope } from "./scope.tsx";

/** The three reactions the tray offers at one press. */
export const QUICK_REACTIONS = ["✅", "👀", "🙌"] as const;

/** What Save says over a rewrite that is the message as it stands. */
export const NOTHING_CHANGED = "Nothing has changed yet.";

/** What a second editor's Save says while another rewrite is out. */
export const EDIT_IN_FLIGHT = "Waiting for the last edit's answer before another.";

/** What Save says over a rewrite emptied of words. */
export const EMPTY_REWRITE = "Write the message first. To take it back, delete it.";

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

export function MessageRow({
  view,
  continuation = false,
  where = "room",
  working = [],
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
}

function SpokenRow({ view, continuation, where, working, flash }: Required<MessageRowProps>) {
  const { message } = view;
  const scope = useRoomScope();
  const nav = useChatNav();
  const people = useChatPeople();
  const marks = useReaderMarks();
  const read = useChatRead();
  const toast = useToast();
  const assistant = useAssistantControl();
  const now = useNow();
  // A MENU OR A PICKER OPEN FROM THE TRAY holds the row in its pointed state,
  // so the tray does not vanish from under the menu it opened.
  const [busy, setBusy] = useState(0);
  const hold = (open: boolean) => setBusy((n) => Math.max(0, n + (open ? 1 : -1)));

  const author = people.person(message.author, message.author_kind);
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

  const react = (emoji: string) => {
    if (reactHold) return;
    const held = (view.reactions ?? []).find((r) => r.emoji === emoji);
    scope.react(message.id, emoji, !!held?.mine);
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
  const link = nav.hrefOf({ kind: "room", id: scope.channelID, message: message.id });
  const copyMessageLink = () =>
    void copyLink(link).then((said) =>
      toast.show(
        said === "Copied"
          ? { variant: "success", message: "Link copied", duration: 1600 }
          : { variant: "info", message: "This browser did not allow copying the link." },
      ),
    );

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
    } else if (matchesRow("message.move", press)) {
      event.preventDefault();
      const list = event.currentTarget.closest("[role='log']");
      const rows = [...(list?.querySelectorAll<HTMLElement>("[data-message-id][tabindex]") ?? [])];
      const at = rows.indexOf(event.currentTarget);
      rows[at + (event.key === "ArrowUp" ? -1 : 1)]?.focus();
    }
  };

  const more: MenuEntry[] = [
    { key: "copy", label: "Copy link", icon: <CopyGlyph size="sm" />, onSelect: copyMessageLink },
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
    ...(mine || scope.moderates ? [{ kind: "separator" as const, key: "own" }] : []),
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
        ]
      : []),
    ...(mine || scope.moderates
      ? [
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
            />
          ))}
        {!quoted && (
          <EmojiPicker
            label="Add a reaction"
            tooltip="Add a reaction"
            onPick={react}
            onOpenChange={hold}
            trigger={(_open, toggle) => (
              <MessageAction
                label="Add a reaction"
                icon={<FaceSlightlySmilingPlusGlyph size="md" />}
                disabled={!!reactHold}
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
          onClick={save}
        />
        <ActionDivider />
        <Menu
          label="More actions"
          align="end"
          onOpenChange={hold}
          renderTrigger={(props) => (
            <MessageAction {...props} label="More actions" icon={<EllipsisGlyph size="md" />} />
          )}
          items={more}
        />
      </MessageActions>
    );

  const thread = where === "room" ? view.thread : undefined;
  const threadRead = Math.max(
    (thread && read?.thread_cursors?.[message.id]) || 0,
    read?.cursors?.[scope.channelID] ?? 0,
  );
  const newReplies =
    thread && !threadOpen && (packPosition(thread.last_reply_position) ?? 0) > threadRead;
  const answering = working.filter((seat) => seat.thread === message.id);
  const answeringLine = answering[0]
    ? `${people.nameOf(answering[0].handle)} ${answering[0].status || "is writing a reply"}`
    : "";

  const footer = (
    <>
      {!deleted && !editing && <MessageUnfurls links={message.links} />}
      {!deleted && (view.reactions ?? []).length > 0 && (
        <ReactionBar
          reactions={(view.reactions ?? []).map((r) => ({
            emoji: r.emoji,
            count: r.count,
            mine: !!r.mine,
          }))}
          onToggle={react}
          disabledReason={reactHold || undefined}
          add={quoted || reactHold ? undefined : <AddReaction onPick={react} onOpenChange={hold} />}
        />
      )}
      {thread && thread.replies > 0 && (
        <ThreadSummary
          replies={thread.replies}
          participants={thread.repliers.map((handle) => stackMember(people.person(handle)))}
          last={relTime(thread.last_reply_at, now)}
          unread={newReplies ? 1 : 0}
          open={threadOpen}
          onOpen={openThread}
          working={
            answeringLine ? (
              <ThinkingIndicator
                className="chat-thread-working"
                status={answeringLine}
                label={answeringLine}
              />
            ) : undefined
          }
        />
      )}
      {(!thread || thread.replies === 0) && answeringLine && (
        <ThinkingIndicator
          className="chat-thread-working"
          status={answeringLine}
          label={answeringLine}
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
      timeTitle={fmtDateTime(message.created_at)}
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
            ? ` by ${people.nameOf(message.deleted_by)}`
            : ""}
          .
        </p>
      ) : editing ? (
        <InlineEditor view={view} />
      ) : (
        <MessageBody
          message={message}
          viewer={scope.viewer}
          nameOf={people.nameOf}
          onPerson={(handle) => nav.openPane({ kind: "profile", handle })}
          after={
            message.edited_at ? (
              <span className="chat-edited" title={`Edited ${fmtDateTime(message.edited_at)}`}>
                (edited)
              </span>
            ) : null
          }
        />
      )}
    </ChatMessage>
  );
}

/**
 * How much of a message the field of its rewrite quotes to name it: enough to
 * tell two of the person's messages apart, short enough for a row of the
 * approval card.
 */
const REWRITE_QUOTE_CHARS = 40;

/** The field a rewrite is, named by the message: its first words, or when it was said. */
function rewriteLabel(message: ChatMessageView["message"]): string {
  const words = [...(message.body ?? "").replace(/\s+/g, " ").trim()];
  if (words.length === 0) return `Rewrite of your message from ${fmtDateTime(message.created_at)}`;
  const quoted =
    words.length > REWRITE_QUOTE_CHARS
      ? `${words.slice(0, REWRITE_QUOTE_CHARS).join("").trimEnd()}…`
      : words.join("");
  return `Rewrite of your message “${quoted}”`;
}

/**
 * Editing in place: the row becomes a small box. Escape cancels, Enter saves.
 *
 * WHY THE REWRITE WOULD CHANGE NOTHING THE ENGINE TAKES is said on Save before
 * the round trip: the same words again are no edit at all (the engine has no
 * same-content no-op, so a Save there would draw "edited" on a message nobody
 * changed), an empty body is refused because emptying a message is a delete,
 * and a rewrite past the cap is refused as the composer's message would be,
 * measured the same way. ONE REWRITE OUT AT A TIME, said on every other
 * editor's Save rather than dropped.
 *
 * FOCUS GOES BACK TO WHERE THE EDIT CAME FROM when it is saved or cancelled:
 * the message box an Up arrow left, or else the row itself (a menu item that
 * opened the editor is gone by then). Left to unmount with focus inside it,
 * the reader was left on the page's body.
 *
 * THE REWRITE IS A FIELD of the room's one draft for the person's own
 * assistant, named by the message and refused while its save is out.
 */
export function InlineEditor({ view }: { view: ChatMessageView }) {
  const { message } = view;
  const scope = useRoomScope();
  const [value, setValue] = useState(message.body ?? "");
  const [saving, setSaving] = useState(false);
  const box = useRef<HTMLTextAreaElement | null>(null);
  const [origin] = useState(() => {
    const at = document.activeElement;
    return at instanceof HTMLElement && at !== document.body ? at : null;
  });

  useEffect(() => {
    const el = box.current;
    if (el) el.setSelectionRange(el.value.length, el.value.length);
    return () => {
      requestAnimationFrame(() => {
        if (document.activeElement && document.activeElement !== document.body) return;
        const back = origin?.isConnected
          ? origin
          : document.querySelector<HTMLElement>(`[data-message-id="${CSS.escape(message.id)}"]`);
        back?.focus({ preventScroll: true });
      });
    };
    // Once, for the editor's whole life: the origin is where it opened from.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useDraftBox({
    path: `rewrite.${view.channel_seq}`,
    label: rewriteLabel(message),
    hint: "Markdown",
    // AFTER THE ROOM'S AND THE THREAD'S BOXES, in the order the room said them.
    rank: 2 + view.channel_seq,
    value,
    set: setValue,
    refuses: (next) => (saving ? "That rewrite is being saved." : bodyRefusal(next)),
  });

  const rewrite = value.trim();
  const budget = bodyBudget(value);
  const rule =
    scope.rules.edit ||
    (rewrite === (message.body ?? "").trim()
      ? NOTHING_CHANGED
      : !rewrite
        ? EMPTY_REWRITE
        : budget.over
          ? SHORTEN_FIRST
          : "");
  const blocked =
    rule ||
    (scope.writes.edit.access.can ? "" : scope.writes.edit.access.reason) ||
    (scope.writes.edit.busy && !saving ? EDIT_IN_FLIGHT : "");

  const close = () => scope.stopEditing(message.id);
  const save = async () => {
    // THE BUTTON'S OWN GATE, because the box submits on its own key: a second
    // Enter while the first answer was out would rewrite the message again
    // under a new operation key.
    if (saving || blocked || !pressable(scope.writes.edit)) return;
    setSaving(true);
    try {
      if (await scope.edit(message.id, rewrite)) close();
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="chat-inline-editor">
      <BodyField budget={budget}>
        {(form) => (
          <KitComposer
            ref={box}
            id={form.id}
            aria-describedby={form.describedBy}
            value={value}
            onChange={setValue}
            onSubmit={() => void save()}
            submitOn="enter"
            appearance="chat"
            label="Edit your message"
            autoFocus
            onKeyDown={(event) => {
              if (event.key === "Escape") {
                event.preventDefault();
                event.stopPropagation();
                close();
              }
            }}
            tools={<span className="chat-inline-editor-hint">Escape to cancel, Enter to save</span>}
            actions={
              <>
                <Button size="small" variant="ghost" onClick={close} disabled={saving}>
                  Cancel
                </Button>
                <Button
                  size="small"
                  variant="primary"
                  loading={saving}
                  disabledReason={blocked || undefined}
                  title={blocked || undefined}
                  onClick={() => void save()}
                >
                  Save
                </Button>
              </>
            }
          />
        )}
      </BodyField>
    </div>
  );
}
