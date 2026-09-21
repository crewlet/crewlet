/**
 * Saying something.
 *
 * # The mentions are resolved here, and that is the engine's own rule
 *
 * A message carries the handles it named as a FIELD, already resolved: the
 * engine does not re-read the prose, here or for a seat's own posting tool. So
 * the composer reads `@name` out of what somebody typed and matches it against
 * the roster — and a handle the company no longer has is dropped by the write
 * path rather than waking nobody silently.
 *
 * `@channel` is not a handle: it sets `collective`, which wakes at most
 * 32 agents and says on the record when it truncated.
 *
 * # Enter sends, and the draft is not in the URL
 *
 * A draft is per room and lives in this component. It is deliberately not a
 * `?draft=` — an address somebody pastes into a room would carry half a
 * sentence with it — and deliberately not in browser storage either: what this
 * tab has not said is not state anything else should be able to read.
 */

import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";

import { Button, Kbd, Textarea } from "@crewlethq/ui";

/**
 * The most a message may carry — `chat.MaxBody`, 32 KiB.
 *
 * REFUSED RATHER THAN CUT, which is the tree's rule for a value with a vendor
 * limit: cutting somebody's message to fit is losing what they wrote, and the
 * engine refuses it by name anyway. The counter appears only as the cap comes
 * into view, because a character count over an ordinary remark is noise.
 */
export const MAX_BODY_BYTES = 32 * 1024;

/** When to start showing how much room is left. */
const SHOW_REMAINING_AT = 0.9;

export interface ComposerProps {
  /** Who this is addressed to, for the placeholder: "#general", "Ada". */
  what: string;
  /** Whether this composer answers in a thread rather than the room. */
  inThread?: boolean;
  disabled?: boolean;
  /** Why it is disabled, said rather than left to a greyed-out box. */
  disabledReason?: string;
  /** The roster, for resolving what somebody typed to a handle. */
  handles: readonly string[];
  send: (message: { body: string; mentions: string[]; collective: boolean }) => void;
  /** Somebody is composing. Throttled by the caller — it is a fleet-wide
   *  probe, not a keystroke. */
  onTyping?: (typing: boolean) => void;
}

const encoder = new TextEncoder();

/**
 * The handles a body names, and whether it addressed the whole room.
 *
 * PURE, and matched against the roster rather than trusted: `@` is a character
 * people type, and a mention that reaches the record is a WAKE — so a name
 * that is not a seat is prose rather than a person who never answers.
 */
export function mentionsIn(
  body: string,
  handles: readonly string[],
): {
  mentions: string[];
  collective: boolean;
} {
  const named = new Set<string>();
  let collective = false;
  for (const [, word] of body.matchAll(/@([A-Za-z0-9._-]+)/g)) {
    if (!word) continue;
    if (word.toLowerCase() === "channel" || word.toLowerCase() === "here") {
      collective = true;
      continue;
    }
    const match = handles.find((handle) => handle.toLowerCase() === word.toLowerCase());
    if (match) named.add(match);
  }
  return { mentions: [...named], collective };
}

export function Composer({
  what,
  inThread = false,
  disabled = false,
  disabledReason = "",
  handles,
  send,
  onTyping,
}: ComposerProps) {
  const [body, setBody] = useState("");
  const box = useRef<HTMLTextAreaElement | null>(null);
  const bytes = encoder.encode(body).length;
  const tooLong = bytes > MAX_BODY_BYTES;

  // THE TYPING FLAG IS WITHDRAWN WHEN THIS COMPOSER GOES, because a person who
  // navigated away is demonstrably not typing and the server's own expiry is
  // six seconds of somebody else watching a lie.
  useEffect(() => () => onTyping?.(false), [onTyping]);

  const submit = useCallback(() => {
    const text = body.trim();
    if (!text || disabled || tooLong) return;
    const { mentions, collective } = mentionsIn(text, handles);
    send({ body: text, mentions, collective });
    setBody("");
    onTyping?.(false);
    box.current?.focus();
  }, [body, disabled, tooLong, handles, send, onTyping]);

  const onKey = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // ENTER SENDS AND SHIFT-ENTER IS A NEWLINE, which is what every chat
    // product this replaces does — and the modifier is what a reader reaches
    // for without being told. A composer where Enter inserts a line feed
    // needs a button press per message, which is the one interaction this
    // screen has thousands of.
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
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
      <Textarea
        ref={box}
        rows={inThread ? 2 : 3}
        autoResize
        value={body}
        disabled={disabled}
        error={tooLong}
        aria-label={inThread ? `Reply in the thread` : `Message ${what}`}
        placeholder={
          disabled
            ? disabledReason || "This room takes no messages from here."
            : inThread
              ? "Reply in this thread…"
              : `Message ${what} — @name to wake somebody, @channel for the room`
        }
        onChange={(event) => {
          setBody(event.target.value);
          onTyping?.(event.target.value.length > 0);
        }}
        onKeyDown={onKey}
        trailing={<Kbd subtle>Enter</Kbd>}
      />
      <div className="row gap-2">
        {/* THE REASON IS NOT REPEATED HERE. It is the box's own placeholder,
            and it is said once more above the transcript where a reader
            arrives — a third copy in the same viewport reads as three
            different problems. */}
        {tooLong && (
          <span className="t-caption">
            {bytes.toLocaleString()} bytes, and a message may carry{" "}
            {MAX_BODY_BYTES.toLocaleString()}. Nothing is cut to fit — shorten it, or put the long
            part somewhere with an address.
          </span>
        )}
        {!tooLong && bytes > MAX_BODY_BYTES * SHOW_REMAINING_AT && (
          <span className="t-caption muted">
            {(MAX_BODY_BYTES - bytes).toLocaleString()} bytes left
          </span>
        )}
        <span className="spacer" />
        <Button variant="primary" type="submit" disabled={disabled || tooLong || !body.trim()}>
          Send
        </Button>
      </div>
    </form>
  );
}
