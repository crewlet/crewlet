/**
 * Saying something.
 *
 * # One bordered box
 *
 * The kit's `Composer`, laid out top to bottom: the formatting row, one line
 * of words that grows as they do, and the tools along the foot (attach, the
 * formatting toggle, emoji, mention, Draft with Ask Crewlet) with Send at its
 * end. Enter sends and Shift+Enter breaks the line, as in every message box
 * (the kit's send rule, `compose.send` in the keymap's legend).
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
 * A ROOM that takes no messages (archived, a members-only room the reader is
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
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";

import {
  Button,
  caretPoint,
  Composer as KitComposer,
  DeliveryReceipt,
  EmojiPicker,
  FormField,
  IconButton,
  MentionMenu,
  mentionQuery,
  Menu,
  SendButton,
  SendKeysHint,
  Toolbar,
  Tooltip,
  formatMarkdown,
  insertMarkdown,
  type FormFieldRender,
  type MarkdownFormat,
  type MarkdownTextFormat,
  type MentionOption,
  type SendKeysLabels,
} from "@crewlethq/ui";
import {
  AtSignGlyph,
  BaselineGlyph,
  BoldGlyph,
  CodeGlyph,
  EllipsisGlyph,
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
import { useQuery } from "~/lib/useQuery.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

import { AttachDialog, previewLinks, recordLink, type AttachKind } from "./Attach.tsx";
import { ChatFace } from "./Face.tsx";
import { draftKey, useChatDrafts } from "./drafts.ts";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { matchesPerson, useChatNames, useChatPeopleWhile } from "./people.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { quietHint, reachRecipients } from "./receipts.ts";
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

/** One control of the formatting row: what it writes, its name and its glyph. */
interface FormatControl {
  kind: MarkdownFormat | MarkdownTextFormat;
  label: string;
  glyph: ReactNode;
}

/**
 * The formatting row, in groups. The FIRST group always stands; the rest fold
 * into a "More formatting" menu where the box is narrow (a thread's pane), so
 * the row collapses rather than wrapping onto a second line.
 */
const FORMAT_GROUPS: readonly (readonly FormatControl[])[] = [
  [
    { kind: "bold", label: "Bold", glyph: <BoldGlyph size="sm" /> },
    { kind: "italic", label: "Italic", glyph: <ItalicGlyph size="sm" /> },
    { kind: "strikethrough", label: "Strikethrough", glyph: <StrikethroughGlyph size="sm" /> },
  ],
  [{ kind: "link", label: "Link", glyph: <LinkGlyph size="sm" /> }],
  [
    { kind: "numberedList", label: "Numbered list", glyph: <ListOrderedGlyph size="sm" /> },
    { kind: "bulletList", label: "Bulleted list", glyph: <ListGlyph size="sm" /> },
  ],
  [
    { kind: "quote", label: "Quote", glyph: <TextQuoteGlyph size="sm" /> },
    { kind: "code", label: "Code", glyph: <CodeGlyph size="sm" /> },
  ],
];

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
  const names = useChatNames();
  const assistant = useAssistantControl();
  const drafts = useChatDrafts();
  const key = draftKey(channelID, threadRoot);
  const value = drafts.get(key);
  const setValue = useCallback(
    (next: string) => drafts.set(key, next, channelID, threadRoot),
    [drafts, key, channelID, threadRoot],
  );
  const field = useRef<HTMLTextAreaElement | null>(null);
  const access = threadRoot ? scope.outbox.reply : scope.outbox.post;
  const held = access.can ? "" : access.reason;
  const budget = bodyBudget(value);
  // WHY THIS PRESS WOULD SEND NOTHING, in the order a person clears them:
  // the same sentence on the button and on the send key, so the key never
  // sends what the button would refuse.
  const hold =
    scope.rules.post || held || (budget.over ? SHORTEN_FIRST : value.trim() ? "" : WRITE_FIRST);

  // WHOM THE POST WILL REACH, before it is sent: the post's own routing run
  // dry by the engine on what the box says once the typing settles, so the
  // line names exactly whom the send will wake, and warns when that is no
  // agent. Asked only of words that could be sent.
  const settled = useQuiet(budget.over ? "" : value.trim());
  const reach = useQuery(
    "chat_reach",
    { channel_id: channelID, body: settled, ...(threadRoot ? { reply_to: threadRoot } : {}) },
    { enabled: settled !== "" && !scope.rules.post && !held },
  );
  const draftReach = value.trim() && settled ? reach.data?.reach : undefined;

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

  const send = () => {
    if (hold) return;
    const body = value.trim();
    void scope.outbox.send({
      channelID,
      ...(threadRoot ? { threadRoot } : {}),
      body,
      links: previewLinks(body),
    });
    setValue("");
    onTyping?.(false);
    field.current?.focus();
    onSent?.();
  };

  // UP IN AN EMPTY BOX opens the reader's last message for a rewrite
  // (`chatbox.edit-last` in the keymap's legend).
  const onKey = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (matchesRow("chatbox.edit-last", event.nativeEvent) && value === "" && onEditLast) {
      event.preventDefault();
      onEditLast();
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

  return (
    <div className="chat-composer">
      <MessageBox
        fieldRef={field}
        value={value}
        onChange={(next) => {
          setValue(next);
          onTyping?.(next.length > 0);
        }}
        label={label}
        placeholder={placeholder}
        onSubmit={send}
        onKey={onKey}
        extraTools={
          <>
            {!compact && <Toolbar.Separator />}
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
        }
        actions={<SendButton mode="send" onPress={send} disabledReason={hold || undefined} />}
      />
      {/* WHOM THE DRAFT WILL REACH, on a line the box keeps while it is open,
          as the activity line keeps its own: arriving once the typing
          settles, it moved the conversation above the box. */}
      <div className="chat-composer-reach">
        {draftReach && (
          <DeliveryReceipt
            tense="draft"
            announce
            recipients={reachRecipients(draftReach, undefined, names.nameOf)}
            truncated={draftReach.truncated}
            hint={quietHint(reach.data?.quiet)}
          />
        )}
        {/* THE SEND KEYS, while there are words to send and room to say
            it; the kit's hint says nothing on a touch keyboard. */}
        {value !== "" && !nav.narrow && <SendKeysHint className="chat-composer-hint" />}
      </div>
      {/* THE BROWSER'S REASON, beside the Send it holds: the button's own
          description reaches neither a touch screen nor a reader who has not
          found the button yet. */}
      {held && <p className="t-caption chat-composer-held">{held}</p>}
    </div>
  );
}

export interface MessageBoxProps {
  value: string;
  onChange: (next: string) => void;
  /** Names the field for a screen reader. */
  label: string;
  placeholder?: string;
  autoFocus?: boolean;
  /** The field itself, for a caller that moves focus or the caret. */
  fieldRef?: RefObject<HTMLTextAreaElement | null>;
  /** Enter, unless a list is open. */
  onSubmit: () => void;
  /** The words the field's description says around its keys: "to save" for a rewrite. */
  sendLabels?: SendKeysLabels;
  /** A key the box does not answer itself: Up for the last message, Escape for an editor. */
  onKey?: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  /** Tools after the box's own, at the footer's start. */
  extraTools?: ReactNode;
  /** The footer's end: Send, or Cancel and Save. */
  actions: ReactNode;
}

/**
 * The bordered box every message is written in: a new message, a reply and a
 * rewrite in place alike. The formatting row, the `@` and `#` lists, emoji,
 * attach and the length line are the box's; what Enter does and what stands
 * at the footer's end are the caller's.
 */
export function MessageBox({
  value,
  onChange,
  label,
  placeholder,
  autoFocus,
  fieldRef,
  onSubmit,
  sendLabels,
  onKey,
  extraTools,
  actions,
}: MessageBoxProps) {
  const scope = useRoomScope();
  const nav = useChatNav();
  const { channels } = useChatFrame();
  const [format, toggleFormat] = useFormatRow(nav.narrow);
  const [caret, setCaret] = useState(value.length);
  const [active, setActive] = useState(0);
  // WHETHER THE ARROWS HAVE TAKEN THE LIST: until then its first row rests
  // as what Enter takes, and the first arrow commits to it.
  const [keyed, setKeyed] = useState(false);
  const [dismissed, setDismissed] = useState<number | null>(null);
  const [attaching, setAttaching] = useState<AttachKind | null>(null);
  const own = useRef<HTMLTextAreaElement | null>(null);
  const field = fieldRef ?? own;
  const listID = useId();
  const budget = bodyBudget(value);
  const setValue = onChange;

  const query = mentionQuery(value, caret);
  const everyone = useChatPeopleWhile(query?.trigger === "@");
  const options = useMemo<MentionOption[]>(() => {
    if (!query) return [];
    const q = query.query.toLowerCase();
    if (query.trigger === "@") {
      const found = everyone
        .filter((p) => p.handle !== scope.viewer && (!q || matchesPerson(p, q)))
        .map<MentionOption>((p) => ({
          id: p.handle,
          insert: `@${p.handle}`,
          label: p.name,
          detail: p.handle,
          mark: <ChatFace person={p} size="xs" />,
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
                mark: <AtSignGlyph size="sm" />,
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
  }, [query?.trigger, query?.query, everyone, channels, scope.viewer, scope.channel?.kind]);
  const menuOpen = !!query && options.length > 0 && dismissed !== query.start;
  const activeIndex = Math.min(active, Math.max(0, options.length - 1));

  // THE LIST STANDS UNDER ITS TRIGGER: the trigger's distance from the box the
  // list is placed in, read again as the text or the field's width moves it.
  const list = useRef<HTMLUListElement | null>(null);
  const [anchor, setAnchor] = useState<number | undefined>(undefined);
  const trigger = menuOpen && query ? query.start : -1;
  useLayoutEffect(() => {
    const el = field.current;
    const box = list.current?.parentElement?.offsetParent;
    if (trigger < 0 || !el || !box) return;
    const place = () => setAnchor(caretPoint(el, trigger, box).left);
    place();
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(place);
    observer?.observe(el);
    return () => observer?.disconnect();
  }, [trigger, value, field]);

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
    setKeyed(false);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (menuOpen) {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        setKeyed(true);
        // THE FIRST DOWN COMMITS TO THE RESTING ROW rather than skipping it.
        const step = event.key === "ArrowDown" ? (keyed ? 1 : 0) : -1;
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
    onKey?.(event);
    if (event.defaultPrevented) return;
    // THE KEYMAP'S ROWS (`app/keymap.ts`, "Writing a chat message").
    const press = event.nativeEvent;
    if (matchesRow("chatbox.bold", press)) {
      event.preventDefault();
      formatAs("bold");
    } else if (matchesRow("chatbox.italic", press)) {
      event.preventDefault();
      formatAs("italic");
    }
  };

  const [always = [], ...folding] = FORMAT_GROUPS;
  const formatBar = format ? (
    <Toolbar label="Formatting" fold="container">
      {always.map((control) => (
        <FormatTool key={control.kind} control={control} onPress={formatAs} />
      ))}
      <Toolbar.Wide>
        {folding.map((group, index) => [
          <Toolbar.Separator key={`rule-${index}`} />,
          ...group.map((control) => (
            <FormatTool key={control.kind} control={control} onPress={formatAs} />
          )),
        ])}
      </Toolbar.Wide>
      <Toolbar.Overflow>
        <Toolbar.Separator />
        <Menu
          label="More formatting"
          align="start"
          activeDescendant
          entrance="instant"
          renderTrigger={(props) => (
            <Tooltip content="More formatting" disabled={props["aria-expanded"]}>
              <IconButton
                {...props}
                label="More formatting"
                size="sm"
                variant="ghost"
                icon={<EllipsisGlyph size="sm" />}
              />
            </Tooltip>
          )}
          items={folding.flatMap((group, index) => [
            ...(index > 0 ? [{ kind: "separator" as const, key: `rule-${index}` }] : []),
            ...group.map((control) => ({
              key: control.kind,
              label: control.label,
              icon: control.glyph,
              onSelect: () => formatAs(control.kind),
            })),
          ])}
        />
      </Toolbar.Overflow>
    </Toolbar>
  ) : undefined;

  const tools = (
    <>
      <Menu
        label="Attach"
        align="start"
        activeDescendant
        entrance="instant"
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
        entrance="instant"
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
      {extraTools}
    </>
  );

  return (
    <>
      {menuOpen && (
        <MentionMenu
          id={listID}
          listRef={list}
          appearance="chat"
          anchor={anchor}
          label={query!.trigger === "@" ? "People and agents" : "Rooms"}
          options={options}
          active={activeIndex}
          onPick={pick}
          onHover={(index) => {
            setActive(index);
            setKeyed(false);
          }}
          keyed={keyed}
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
              setKeyed(false);
              setCaret(field.current?.selectionStart ?? next.length);
            }}
            onSelect={(event) => setCaret((event.target as HTMLTextAreaElement).selectionStart)}
            onKeyUp={(event) => setCaret((event.target as HTMLTextAreaElement).selectionStart)}
            onKeyDown={onKeyDown}
            onSubmit={onSubmit}
            sendHint={false}
            sendLabels={sendLabels}
            appearance="chat"
            label={label}
            placeholder={placeholder}
            autoFocus={autoFocus}
            rows={1}
            toolbar={formatBar}
            tools={tools}
            actions={actions}
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
    </>
  );
}

/** One control of the formatting row. */
function FormatTool({
  control,
  onPress,
}: {
  control: FormatControl;
  onPress: (kind: FormatControl["kind"]) => void;
}) {
  return (
    <Tooltip content={control.label}>
      <IconButton
        label={control.label}
        size="sm"
        variant="ghost"
        icon={control.glyph}
        onClick={() => onPress(control.kind)}
      />
    </Tooltip>
  );
}
