/**
 * Saying something.
 *
 * # What a message names is the engine's reading of it
 *
 * The composer sends what was typed and nothing about it: the tool reads the
 * handles a body names against the company's seats, and `@channel` as the
 * room, itself — a handle the company does not have stays prose rather than a
 * wake to nobody, and the whole room is capped at 32 agents and says so on the
 * record when it truncated. A copy of that reading here would be a second
 * answer to "who did this wake" that could disagree with the first.
 *
 * # The command key sends, and the draft is not in the URL
 *
 * ONE SEND KEY FOR WHAT IS SAID TO SOMEBODY, read from the keymap's own row
 * (`compose.send`) rather than spelled here: a reply in the Inbox, a comment on
 * a task and a message in a room are all somebody writing to somebody, and two
 * opposite send keys a screen apart is the one inconsistency a hand learns
 * wrong. Enter alone is a new line, which a message that is a pasted list or a
 * stack trace needs.
 *
 * THE ASSISTANT'S QUESTION BOX IS THE EXCEPTION, on purpose: a question sends
 * on Enter there (`assistant.send`), the design system's own rule for a box
 * whose text is one line far more often than not. It is a question put to the
 * assistant rather than a message to a person, and the legend lists it under
 * a heading of its own.
 *
 * # The room closes the box; the browser holds the button
 *
 * TWO DIFFERENT REASONS NOT TO SEND, and they are drawn differently on
 * purpose. A ROOM that takes no messages — archived, a private room the reader
 * is outside, a kind this build cannot classify — is a fact about the room
 * that nothing on this screen changes, so the box itself is closed and says
 * why in its own placeholder. A BROWSER that cannot send now — offline, a
 * credential acting as no seat, a person the engine posts nothing for — is
 * the reason every write control in the dashboard is held with
 * (`lib/useWriteAccess.ts`), so the box stays open and Send is held with that
 * sentence, as every other composer's is: a field that went dead under
 * somebody's hands when the socket blinked took their focus with it,
 * mid-word, and a person may well want the words written while they
 * reconnect.
 *
 * A draft is per room and lives in this component. It is deliberately not a
 * `?draft=` — an address somebody pastes into a room would carry half a
 * sentence with it — and deliberately not in browser storage either: what this
 * tab has not said is not state anything else should be able to read. Its one
 * reader is the person's own assistant, because a person who asks it for help
 * with a message is asking about exactly this: the box is a field of the
 * room's one draft while the room takes messages (`app/assistant/boxes.ts`,
 * declared by `Room.tsx`), beside the other boxes open on the room, and the
 * assistant proposes text into it and never sends, so Send stays the
 * person's.
 */

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";

import { Button, FormField, Kbd, Textarea, type FormFieldRender } from "@crewlethq/ui";
import { SendGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { capsOf, keyRow, matchesRow } from "~/app/keymap.ts";
import { MAX_BODY_BYTES } from "~/contract/chat.ts";
import { textBudget, type TextBudget } from "~/lib/format.ts";
import { SHORTEN } from "./start.ts";

/** What a message's Send or Save says over a body past the cap. */
export const SHORTEN_FIRST = "Shorten it first.";

/**
 * A message body against the engine's cap, measured and said as every text
 * field's is (`textBudget`): UTF-8 bytes OF WHAT IS SENT, which is the trimmed
 * text, and the LINE under the field — a count from the point every field
 * starts counting, the refusal and this field's own remedy past the cap. It is
 * drawn as every field draws it ([BodyField]), muted while it counts and in
 * the error's tone past the cap.
 *
 * ONE MEASURE FOR EVERY BODY — the composer's and the editor's — because the
 * engine checks both against one cap, and two measures are two answers about
 * one message: the editor sent a rewrite past the cap to be refused after the
 * round trip, and the composer counted trailing whitespace it never sent and
 * held a message that fit. AND ONE LINE: this screen once threw `textBudget`'s
 * line away and wrote its own, from a threshold of its own, so a message
 * started counting at a different point, and in different words, from the
 * topic field beside it.
 */
export function bodyBudget(body: string): TextBudget {
  return textBudget(body, MAX_BODY_BYTES, "a message", SHORTEN);
}

/**
 * A message body's box and the line its budget says under it, as every field
 * in the dashboard draws one (`FormField`): the count as help, the refusal as
 * the error, both joined into the box's description so a screen reader hears
 * them with it. No visible label — the box names itself (`aria-label`), as a
 * composer does.
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

export interface ComposerProps {
  /** Who this is addressed to, for the placeholder: "#general", "Ada". */
  what: string;
  /** Whether this composer answers in a thread rather than the room. */
  inThread?: boolean;
  /**
   * Why the ROOM takes no messages from this reader, or "" when it takes
   * them. Closes the box, with the sentence as its placeholder.
   */
  closed?: string;
  /**
   * Why this BROWSER cannot send now, or "" when it can — the sentence
   * `useWriteAccess` holds the composer's tool with. Holds Send, beside which
   * it is written, and leaves the box open.
   */
  held?: string;
  /** Say what was typed, trimmed. */
  send: (body: string) => void;
  /** Somebody is composing. Throttled by the caller — it is a fleet-wide
   *  probe, not a keystroke. */
  onTyping?: (typing: boolean) => void;
}

/** The keycaps of the send key, as the legend draws them. */
const SEND_CAPS = capsOf(keyRow("compose.send").presses[0]!);

export function Composer({
  what,
  inThread = false,
  closed = "",
  held = "",
  send,
  onTyping,
}: ComposerProps) {
  const [body, setBody] = useState("");
  const box = useRef<HTMLTextAreaElement | null>(null);
  const budget = bodyBudget(body);
  // WHY THIS PRESS WOULD SEND NOTHING, in the order a person clears them —
  // the same sentence on the button and on the send key, so the key never
  // sends what the button would refuse.
  const hold =
    closed || held || (budget.over ? SHORTEN_FIRST : body.trim() ? "" : "Write the message first.");

  // WHAT THE BOX HOLDS, as a field of the room's one draft for the person's
  // own assistant (see this file's doc). A room that takes no messages holds
  // no box.
  useDraftBox(
    closed
      ? null
      : {
          path: inThread ? "reply" : "message",
          label: inThread ? "Reply in the thread" : `Message to ${what}`,
          hint: "Plain text",
          rank: inThread ? 1 : 0,
          value: body,
          set: setBody,
          refuses: bodyRefusal,
        },
  );

  // THE TYPING FLAG IS WITHDRAWN WHEN THIS COMPOSER GOES, because a person who
  // navigated away is demonstrably not typing and the server's own expiry is
  // six seconds of somebody else watching a lie.
  useEffect(() => () => onTyping?.(false), [onTyping]);

  const submit = useCallback(() => {
    if (hold) return;
    send(body.trim());
    setBody("");
    onTyping?.(false);
    box.current?.focus();
  }, [body, hold, send, onTyping]);

  const onKey = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // THE KEYMAP'S ROW, NEVER A KEY SPELLED HERE — see this file's doc. A
    // composition in progress (an IME's candidate list) is the input method's
    // own Enter, not a send.
    if (!event.nativeEvent.isComposing && matchesRow("compose.send", event.nativeEvent)) {
      event.preventDefault();
      submit();
    }
  };

  return (
    <form
      className="chat-composer"
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      <BodyField budget={budget}>
        {(field) => (
          <Textarea
            ref={box}
            id={field.id}
            aria-describedby={field.describedBy}
            rows={inThread ? 2 : 3}
            autoResize
            value={body}
            disabled={closed !== ""}
            error={budget.over}
            aria-label={inThread ? `Reply in the thread` : `Message ${what}`}
            placeholder={
              closed
                ? closed
                : inThread
                  ? "Reply in this thread…"
                  : `Message ${what} (@name reaches one person, @channel the whole room)`
            }
            onChange={(event) => {
              setBody(event.target.value);
              onTyping?.(event.target.value.length > 0);
            }}
            onKeyDown={onKey}
            trailing={<Kbd subtle keys={SEND_CAPS} />}
          />
        )}
      </BodyField>
      <div className="row gap-2">
        {/* THE ROOM'S REASON IS NOT REPEATED HERE. It is the box's own
            placeholder, and it is said once more above the transcript where
            a reader arrives — a third copy in the same viewport reads as
            three different problems. The BROWSER'S is written here, beside
            the Send it holds: the button's own description reaches neither a
            touch screen nor a reader who has not found the button yet. */}
        {!closed && held && <span className="t-caption">{held}</span>}
        <span className="spacer" />
        <Button
          variant="primary"
          size="small"
          type="submit"
          leadingIcon={<SendGlyph size="sm" />}
          disabledReason={hold || undefined}
          title={hold || undefined}
        >
          {inThread ? "Reply" : "Send"}
        </Button>
      </div>
    </form>
  );
}
