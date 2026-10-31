/**
 * Saying something.
 *
 * # One bordered box
 *
 * The kit's `Composer`, laid out top to bottom: the formatting row, one line
 * of words that grows as they do, and the tools along the foot (attach, the
 * formatting toggle, emoji, mention, Draft with Ask Crewlet) with Send at its
 * end. Enter sends and Shift+Enter breaks the line, as every chat box does
 * (`chatbox.send` in the keymap's legend); a reply in the Inbox and a comment on
 * a task keep the command key (`compose.send`), because those are letters
 * more often than lines.
 *
 * # What a message names is the engine's reading of it
 *
 * The box sends what was typed and nothing about it: the tool reads the
 * handles a body names against the company's seats, and `@channel` as the
 * room, itself. The `@` and `#` lists here only help somebody type a handle
 * or a room's name correctly; who a message reaches is decided where it is
 * applied, and a copy of that reading here would be a second answer that
 * could disagree with the first.
 *
 * # The formatting row writes Markdown, by the design system's rules
 *
 * Each control applies the kit's pure `formatMarkdown` to the field's value
 * and selection, the same function the page editor's bar applies, so what the
 * row writes is what every reader of the grammar reads back (`Body.tsx`).
 * Whether the row shows is a per-browser convenience, remembered: shown by
 * default where there is room for it, hidden by default on a narrow screen,
 * where it would take a line from the words.
 *
 * # The room closes the box; the browser holds the button
 *
 * A ROOM that takes no messages (archived, a private room the reader is
 * outside, a kind this build cannot classify) is a fact nothing on this
 * screen changes, so the room draws a bar in place of the box. A BROWSER that
 * cannot send now (offline, a credential acting as no seat) is the reason
 * every write control in the dashboard is held with (`lib/useWriteAccess.ts`),
 * so the box stays open and Send is held with that sentence: a field that
 * went dead under somebody's hands when the socket blinked took their focus
 * with it, mid-word.
 *
 * # A draft is per box and per tab
 *
 * Kept in the screen's draft store (`drafts.ts`), so it outlives a change of
 * room, and nowhere else: never in the address and never in browser storage.
 * Its one other reader is the person's own assistant: the box is a field of
 * the room's one draft (`app/assistant/boxes.ts`), and the assistant proposes
 * text into it and never sends, so Send stays the person's.
 */

import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";

import {
  Button,
  Composer as KitComposer,
  EmojiPicker,
  FormField,
  IconButton,
  MentionMenu,
  mentionQuery,
  Menu,
  SendButton,
  Toolbar,
  Tooltip,
  formatMarkdown,
  insertMarkdown,
  type FormFieldRender,
  type MarkdownFormat,
  type MarkdownTextFormat,
  type MentionOption,
} from "@crewlethq/ui";
import {
  AtSignGlyph,
  BaselineGlyph,
  BoldGlyph,
  CodeGlyph,
  FaceSlightlySmilingGlyph,
  FileTextGlyph,
  LayersGlyph,
  SquareKanbanGlyph,
  ItalicGlyph,
  LinkGlyph,
  ListGlyph,
  ListOrderedGlyph,
  PlusGlyph,
  StrikethroughGlyph,
  TextQuoteGlyph,
} from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useAssistantControl } from "~/app/assistant/control.ts";
import { matchesRow } from "~/app/keymap.ts";
import { MAX_BODY_BYTES } from "~/contract/chat.ts";
import { textBudget, type TextBudget } from "~/lib/format.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

import { AttachDialog, recordLink, recordLinks, type AttachKind } from "./Attach.tsx";
import { ChatFace } from "./Face.tsx";
import { draftKey, useChatDrafts } from "./drafts.ts";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { matchesPerson, useChatPeople } from "./people.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { roomMark } from "./rooms.ts";
import { useRoomScope } from "./scope.tsx";
import { SHORTEN } from "./start.ts";

/** What a message's Send or Save says over a body past the cap. */
export const SHORTEN_FIRST = "Shorten it first.";

/** What Send says over an empty box. */
export const WRITE_FIRST = "Write a message first.";

/**
 * A message body against the engine's cap, measured and said as every text
 * field's is (`textBudget`): UTF-8 bytes OF WHAT IS SENT, which is the trimmed
 * text, and the LINE under the field. ONE MEASURE FOR EVERY BODY, the box's
 * and the editor's, because the engine checks both against one cap.
 */
export function bodyBudget(body: string): TextBudget {
  return textBudget(body, MAX_BODY_BYTES, "a message", SHORTEN);
}

/**
 * The line a body's budget says under its box, as every field in the
 * dashboard draws one (`FormField`): the count as help, the refusal as the
 * error, both joined into the box's description.
 */
export function BodyField({
  budget,
  children,
}: {
  budget: TextBudget;
  children: (field: FormFieldRender) => ReactNode;
}) {
  return (
    <FormField
      helper={budget.over ? undefined : budget.line}
      error={budget.over ? budget.line : undefined}
    >
      {children}
    </FormField>
  );
}

/**
 * Why a message box would not take `body`, in the line its field says it in,
 * or null: what the person's assistant is refused with when it proposes a
 * message past the cap, as the person is when they write one.
 */
export function bodyRefusal(body: string): string | null {
  const budget = bodyBudget(body);
  return budget.over ? (budget.line ?? SHORTEN_FIRST) : null;
}

/** How many people, agents or rooms the `@` and `#` lists offer at once. */
const MENTION_OPTIONS = 8;

/**
 * Whether the formatting row shows, one choice for every box on this browser.
 * A refused store (a private window) only means the choice is not remembered.
 */
function useFormatRow(narrow: boolean): [boolean, () => void] {
  const [shown, setShown] = useState(() => {
    try {
      const stored = window.localStorage.getItem(STORAGE_KEYS.chatletFormatting);
      return stored === null ? !narrow : stored === "1";
    } catch {
      return !narrow;
    }
  });
  const toggle = useCallback(
    () =>
      setShown((was) => {
        try {
          window.localStorage.setItem(STORAGE_KEYS.chatletFormatting, was ? "0" : "1");
        } catch {
          // Refused storage: the choice lasts until the tab reloads.
        }
        return !was;
      }),
    [],
  );
  return [shown, toggle];
}

export interface ComposerProps {
  channelID: string;
  /** The thread this box replies in; absent for the room's own box. */
  threadRoot?: string;
  /** The thread pane's box: no Draft, no divider. */
  compact?: boolean;
  /** Names the box for a screen reader: "Message #general". */
  label: string;
  placeholder: string;
  /** After a send: the room scrolls to the newest message. */
  onSent?: () => void;
  /** Up in an empty box: open the reader's last message for a rewrite. */
  onEditLast?: () => void;
  /** Somebody is composing. Throttled by the caller (`useChatFocus`). */
  onTyping?: (typing: boolean) => void;
}

export function Composer({
  channelID,
  threadRoot,
  compact = false,
  label,
  placeholder,
  onSent,
  onEditLast,
  onTyping,
}: ComposerProps) {
  const scope = useRoomScope();
  const nav = useChatNav();
  const { channels } = useChatFrame();
  const people = useChatPeople();
  const assistant = useAssistantControl();
  const drafts = useChatDrafts();
  const key = draftKey(channelID, threadRoot);
  const value = drafts.get(key);
  const setValue = useCallback(
    (next: string) => drafts.set(key, next, channelID, threadRoot),
    [drafts, key, channelID, threadRoot],
  );
  const [format, toggleFormat] = useFormatRow(nav.narrow);
  const [caret, setCaret] = useState(value.length);
  const [active, setActive] = useState(0);
  const [dismissed, setDismissed] = useState<number | null>(null);
  const [attaching, setAttaching] = useState<AttachKind | null>(null);
  const field = useRef<HTMLTextAreaElement | null>(null);
  const listID = useId();

  const access = threadRoot ? scope.outbox.reply : scope.outbox.post;
  const held = access.can ? "" : access.reason;
  const budget = bodyBudget(value);
  // WHY THIS PRESS WOULD SEND NOTHING, in the order a person clears them:
  // the same sentence on the button and on the send key, so the key never
  // sends what the button would refuse.
  const hold =
    scope.rules.post || held || (budget.over ? SHORTEN_FIRST : value.trim() ? "" : WRITE_FIRST);

  // THE BOX AS A FIELD of the room's one draft for the person's own
  // assistant. A room that takes no messages holds no box.
  useDraftBox(
    scope.rules.post
      ? null
      : {
          path: threadRoot ? "reply" : "message",
          label: threadRoot ? "Reply in the thread" : label,
          hint: "Markdown",
          rank: threadRoot ? 1 : 0,
          value,
          set: setValue,
          refuses: bodyRefusal,
        },
  );

  // THE TYPING FLAG IS WITHDRAWN WHEN THIS BOX GOES: a person who navigated
  // away is demonstrably not typing.
  useEffect(() => () => onTyping?.(false), [onTyping]);

  const query = mentionQuery(value, caret);
  const options = useMemo<MentionOption[]>(() => {
    if (!query) return [];
    const q = query.query.toLowerCase();
    if (query.trigger === "@") {
      const found = people.everyone
        .filter((p) => p.handle !== scope.viewer && (!q || matchesPerson(p, q)))
        .map<MentionOption>((p) => ({
          id: p.handle,
          insert: `@${p.handle}`,
          label: p.name,
          detail: p.handle,
          mark: <ChatFace person={p} size="sm" />,
          ...(p.doing ? { note: p.doing } : {}),
        }));
      const room: MentionOption[] =
        !isDirect(scope.channel?.kind ?? "public") && (!q || "channel".startsWith(q))
          ? [
              {
                id: "channel",
                insert: "@channel",
                label: "@channel",
                detail: "Notify everyone in this room",
                mark: <AtSignGlyph size="md" />,
              },
            ]
          : [];
      return [...found, ...room].slice(0, MENTION_OPTIONS);
    }
    // ROOMS THE READER IS IN, by name: a room they cannot read is not a room
    // to point somebody else at from here.
    return channels
      .filter((row) => !!row.channel.name && !row.channel.archived_at)
      .filter((row) => !q || (row.channel.name ?? "").includes(q))
      .slice(0, MENTION_OPTIONS)
      .map<MentionOption>((row) => ({
        id: row.channel.id,
        insert: `#${row.channel.name}`,
        label: row.channel.name ?? "",
        ...(row.channel.topic ? { detail: row.channel.topic } : {}),
        mark: <NamedGlyph name={roomMark(row.channel.kind)} size="md" />,
      }));
  }, [query?.trigger, query?.query, people.everyone, channels, scope.viewer, scope.channel?.kind]);
  const menuOpen = !!query && options.length > 0 && dismissed !== query.start;
  const activeIndex = Math.min(active, Math.max(0, options.length - 1));

  /** Apply an edit to the box and put the selection where it says. */
  const apply = (next: { value: string; selection: { start: number; end: number } }) => {
    setValue(next.value);
    setDismissed(null);
    requestAnimationFrame(() => {
      const el = field.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(next.selection.start, next.selection.end);
      setCaret(next.selection.end);
    });
  };
  const selection = () => {
    const el = field.current;
    return { start: el?.selectionStart ?? value.length, end: el?.selectionEnd ?? value.length };
  };
  const formatAs = (kind: MarkdownFormat | MarkdownTextFormat) =>
    apply(formatMarkdown(value, selection(), kind));
  const insert = (text: string) => apply(insertMarkdown(value, selection(), text));
  const pick = (option: MentionOption) => {
    if (!query) return;
    apply(insertMarkdown(value, { start: query.start, end: caret }, `${option.insert} `));
    setActive(0);
  };

  const send = () => {
    if (hold) return;
    const body = value.trim();
    void scope.outbox.send({
      channelID,
      ...(threadRoot ? { threadRoot } : {}),
      body,
      links: recordLinks(body),
    });
    setValue("");
    onTyping?.(false);
    field.current?.focus();
    onSent?.();
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (menuOpen) {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        const step = event.key === "ArrowDown" ? 1 : -1;
        setActive((a) => (a + step + options.length) % options.length);
        return;
      }
      if (event.key === "Enter" || event.key === "Tab") {
        event.preventDefault();
        const option = options[activeIndex];
        if (option) pick(option);
        return;
      }
      if (event.key === "Escape") {
        // THE LIST CLOSES, AND ONLY THE LIST: the layer under it (a pane, the
        // room's Escape that marks it read) never sees this press.
        event.preventDefault();
        event.stopPropagation();
        setDismissed(query!.start);
        return;
      }
    }
    // THE KEYMAP'S ROWS (`app/keymap.ts`, "Writing a chat message").
    const press = event.nativeEvent;
    if (matchesRow("chatbox.edit-last", press) && value === "" && onEditLast) {
      event.preventDefault();
      onEditLast();
    } else if (matchesRow("chatbox.bold", press)) {
      event.preventDefault();
      formatAs("bold");
    } else if (matchesRow("chatbox.italic", press)) {
      event.preventDefault();
      formatAs("italic");
    }
  };

  // DRAFT WITH ASK CREWLET: the assistant is asked to write into this box,
  // which is a field of the room's draft it can propose text for. It never
  // sends; the words land in the box for the person to change and send.
  const draft = () =>
    assistant.ask(
      threadRoot
        ? "Draft a reply for the thread I have open, in the reply box."
        : `Draft a message for me in ${label.replace(/^Message /, "")}, in the message box.`,
    );

  const formatBar = format ? (
    <Toolbar label="Formatting">
      <FormatTool label="Bold" icon={<BoldGlyph size="sm" />} onPress={() => formatAs("bold")} />
      <FormatTool
        label="Italic"
        icon={<ItalicGlyph size="sm" />}
        onPress={() => formatAs("italic")}
      />
      <FormatTool
        label="Strikethrough"
        icon={<StrikethroughGlyph size="sm" />}
        onPress={() => formatAs("strikethrough")}
      />
      <span className="chat-tool-sep" aria-hidden="true" />
      <FormatTool label="Link" icon={<LinkGlyph size="sm" />} onPress={() => formatAs("link")} />
      <span className="chat-tool-sep" aria-hidden="true" />
      <FormatTool
        label="Numbered list"
        icon={<ListOrderedGlyph size="sm" />}
        onPress={() => formatAs("numberedList")}
      />
      <FormatTool
        label="Bulleted list"
        icon={<ListGlyph size="sm" />}
        onPress={() => formatAs("bulletList")}
      />
      <span className="chat-tool-sep" aria-hidden="true" />
      <FormatTool
        label="Quote"
        icon={<TextQuoteGlyph size="sm" />}
        onPress={() => formatAs("quote")}
      />
      <FormatTool label="Code" icon={<CodeGlyph size="sm" />} onPress={() => formatAs("code")} />
    </Toolbar>
  ) : undefined;

  const tools = (
    <>
      <Menu
        label="Attach"
        align="start"
        renderTrigger={(props) => (
          <Tooltip content="Attach a task, page or artifact" disabled={props["aria-expanded"]}>
            <IconButton
              {...props}
              className="chat-composer-attach"
              label="Attach a task, page or artifact"
              size="sm"
              variant="ghost"
              icon={<PlusGlyph size="sm" />}
            />
          </Tooltip>
        )}
        items={[
          {
            key: "task",
            label: "A Worklet task",
            icon: <SquareKanbanGlyph size="sm" />,
            onSelect: () => setAttaching("task"),
          },
          {
            key: "page",
            label: "A Doclet page",
            icon: <FileTextGlyph size="sm" />,
            onSelect: () => setAttaching("page"),
          },
          {
            key: "artifact",
            label: "An artifact",
            icon: <LayersGlyph size="sm" />,
            onSelect: () => setAttaching("artifact"),
          },
          { kind: "separator", key: "s" },
          {
            key: "link",
            label: "A link",
            description: "An address, with the words it is shown by",
            icon: <LinkGlyph size="sm" />,
            onSelect: () => formatAs("link"),
          },
        ]}
      />
      <Tooltip content={format ? "Hide formatting" : "Show formatting"}>
        <IconButton
          label="Formatting"
          size="sm"
          variant="ghost"
          pressed={format}
          className="chat-composer-format"
          icon={<BaselineGlyph size="sm" />}
          onClick={toggleFormat}
        />
      </Tooltip>
      <EmojiPicker
        label="Insert an emoji"
        tooltip="Emoji"
        side="top"
        align="start"
        onPick={(emoji) => insert(emoji)}
        trigger={(_open, toggle) => (
          <IconButton
            label="Emoji"
            size="sm"
            variant="ghost"
            icon={<FaceSlightlySmilingGlyph size="sm" />}
            onClick={toggle}
          />
        )}
      />
      <Tooltip content="Mention someone">
        <IconButton
          label="Mention someone"
          size="sm"
          variant="ghost"
          icon={<AtSignGlyph size="sm" />}
          onClick={() => insert(value && !/\s$/.test(value.slice(0, caret)) ? " @" : "@")}
        />
      </Tooltip>
      {!compact && <span className="chat-tool-sep" aria-hidden="true" />}
      {!compact && (
        <Tooltip content={assistant.unavailable?.short ?? "Draft with Ask Crewlet"}>
          <Button
            variant="ghost"
            size="small"
            leadingIcon={<ProductMark product="assistant" size="sm" />}
            disabledReason={assistant.unavailable?.short}
            onClick={draft}
          >
            Draft
          </Button>
        </Tooltip>
      )}
    </>
  );

  return (
    <div className="chat-composer">
      {menuOpen && (
        <MentionMenu
          id={listID}
          label={query!.trigger === "@" ? "People and agents" : "Rooms"}
          options={options}
          active={activeIndex}
          onPick={pick}
          onHover={setActive}
          footer={
            <>
              <span>Up and Down to move</span>
              <span>Enter or Tab to choose</span>
              <span>Esc to close</span>
            </>
          }
        />
      )}
      <BodyField budget={budget}>
        {(form) => (
          <KitComposer
            ref={field}
            id={form.id}
            aria-describedby={form.describedBy}
            value={value}
            onChange={(next) => {
              setValue(next);
              setDismissed(null);
              setActive(0);
              setCaret(field.current?.selectionStart ?? next.length);
              onTyping?.(next.length > 0);
            }}
            onSelect={(event) => setCaret((event.target as HTMLTextAreaElement).selectionStart)}
            onKeyUp={(event) => setCaret((event.target as HTMLTextAreaElement).selectionStart)}
            onKeyDown={onKeyDown}
            onSubmit={send}
            submitOn="enter"
            appearance="chat"
            label={label}
            placeholder={placeholder}
            rows={1}
            toolbar={formatBar}
            tools={tools}
            actions={<SendButton mode="send" onPress={send} disabledReason={hold || undefined} />}
            aria-controls={menuOpen ? listID : undefined}
            aria-activedescendant={menuOpen ? `${listID}-${activeIndex}` : undefined}
            aria-autocomplete="list"
          />
        )}
      </BodyField>
      {attaching && (
        <AttachDialog
          kind={attaching}
          onClose={() => setAttaching(null)}
          onPick={(hit) => {
            setAttaching(null);
            insert(`${recordLink(hit)} `);
          }}
        />
      )}
      {/* THE BROWSER'S REASON, beside the Send it holds: the button's own
          description reaches neither a touch screen nor a reader who has not
          found the button yet. */}
      {held && <p className="t-caption chat-composer-held">{held}</p>}
    </div>
  );
}

/** One control of the formatting row. */
function FormatTool({
  label,
  icon,
  onPress,
}: {
  label: string;
  icon: ReactNode;
  onPress: () => void;
}) {
  return (
    <Tooltip content={label}>
      <IconButton label={label} size="sm" variant="ghost" icon={icon} onClick={onPress} />
    </Tooltip>
  );
}
