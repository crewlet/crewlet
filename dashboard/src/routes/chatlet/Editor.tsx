/**
 * Rewriting a message in place.
 *
 * THE ROW BECOMES THE MESSAGE BOX, not a lighter field: the same formatting
 * row, the same `@` and `#` lists, emoji and attach, so a rewrite is written
 * with everything the message was. Enter saves, Shift+Enter breaks the line and
 * Escape cancels; with a list open, Escape closes only the list. Cancel and Save
 * stand where Send does.
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

import { useEffect, useRef, useState } from "react";

import { Button, Kbd, SendKeysHint } from "@crewlethq/ui";

import { useDraftBox } from "~/app/assistant/boxes.ts";
import { useDraftState } from "~/app/assistant/draft.ts";
import { pressable } from "~/components/WriteButton.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import type { ChatMessageView } from "~/protocol/index.ts";

import { bodyBudget, bodyRefusal, MessageBox, SHORTEN_FIRST } from "./Composer.tsx";
import { useChatNav } from "./nav.tsx";
import { useRoomScope } from "./scope.tsx";

/** A rewrite's Enter saves it. */
const SAVE_KEYS = { send: "to save" };

/** What Save says over a rewrite that is the message as it stands. */
export const NOTHING_CHANGED = "Nothing has changed yet.";

/** What a second editor's Save says while another rewrite is out. */
export const EDIT_IN_FLIGHT = "Waiting for the last edit's answer before another.";

/** What Save says over a rewrite emptied of words. */
export const EMPTY_REWRITE = "Write the message first. To take it back, delete it.";

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

export function InlineEditor({ view }: { view: ChatMessageView }) {
  const { message } = view;
  const scope = useRoomScope();
  const nav = useChatNav();
  const [value, setValue] = useDraftState(message.body ?? "");
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
    <div className="chat-composer chat-inline-editor">
      <MessageBox
        fieldRef={box}
        value={value}
        onChange={setValue}
        label="Edit your message"
        autoFocus
        onSubmit={() => void save()}
        sendLabels={SAVE_KEYS}
        onKey={(event) => {
          if (event.key !== "Escape") return;
          event.preventDefault();
          event.stopPropagation();
          close();
        }}
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
      {!nav.narrow && (
        <div className="chat-composer-reach">
          <span className="chat-composer-hint">
            <Kbd keys={["Escape"]} subtle /> to cancel, <SendKeysHint labels={SAVE_KEYS} />
          </span>
        </div>
      )}
    </div>
  );
}
