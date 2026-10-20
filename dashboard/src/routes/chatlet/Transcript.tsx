/**
 * A room's transcript: a scroller that stays where the reader put it.
 *
 * # Two rules, and they are the same rule
 *
 * THE PAGE MOVES ONLY WHEN THE READER IS NOT READING. At the bottom of the
 * scroller a new message follows, because that is somebody watching a
 * conversation and holding rows back from them looks broken. Scrolled up, new
 * messages are COUNTED and the reader lets them in when they choose — the same
 * rule `lib/settled.ts` keeps for the activity feed, applied to a list that
 * grows at the bottom instead of the top.
 *
 * AND IT IS KEYED ON IDENTITY. An edit, a reaction or a delete arriving for a
 * message already on screen is not an arrival: counting by length would show
 * "1 new" for somebody fixing a typo, and the pill would never clear.
 *
 * # Only what is on screen is in the DOM
 *
 * A reader who pages back through an afternoon holds hundreds of rows, each of
 * which can be a pasted stack trace. The window is computed from measured
 * heights in `messages.ts`, and the rows that are not drawn are replaced by two
 * spacers of exactly their height — so the scrollbar describes the whole
 * conversation rather than the part of it that happens to be rendered.
 */

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import { Button, Count, NewItemsNotice, StatusDot, Tag, Textarea } from "@crewlethq/ui";
import { SplitGlyph } from "@crewlethq/icons/glyphs";
import { useDraftBox } from "~/app/assistant/boxes.ts";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import type { Act } from "~/lib/useAct.ts";
import { fmtDateTime, fmtTime } from "~/lib/format.ts";
import { kindOfAuthor } from "~/lib/seats.ts";
import type { ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { BodyField, bodyBudget, bodyRefusal, SHORTEN_FIRST } from "./Composer.tsx";
import { MessageLinks } from "./MessageLinks.tsx";
import { windowFor, type Tail } from "./messages.ts";
import type { Outgoing } from "./outbox.ts";
import { PendingMessage } from "./Pending.tsx";

/**
 * How tall a message is assumed to be before it has been drawn.
 *
 * SIXTY-FOUR PIXELS: an author line and one line of body at this density. It
 * is only ever a placeholder for rows outside the window — every row that has
 * been on screen is remembered at its measured height — so being wrong costs a
 * scrollbar that settles as the reader moves rather than a wrong layout.
 */
const ROW_ESTIMATE = 64;

/** How near the bottom still counts as watching the conversation. */
const BOTTOM_SLACK_PX = 48;

export interface TranscriptProps {
  tail: Tail;
  /** Messages this tab is still trying to say, drawn under the transcript. */
  pending: Outgoing[];
  /** Ask for the page below the oldest row held. */
  onOlder: () => void;
  paging: boolean;
  /** Which thread is open, so the row it belongs to can say so. */
  threadRoot: string;
  onOpenThread: (rootID: string) => void;
  onReact: (messageID: string, emoji: string, remove: boolean) => void;
  /** Rewrite one's own message. What the new words name is the engine's
   *  reading of them, as it is for a post. Resolves with whether the engine
   *  took it — applied or pending — which is what closes the editor: a
   *  rewrite that was refused, or that nobody can say landed, stays in front
   *  of the person who typed it. */
  onEdit: (messageID: string, body: string) => Promise<boolean>;
  /** Take a message back. The row survives as a tombstone. */
  onDelete: (messageID: string) => void;
  /** The message whose delete is out, or "" — the one row whose Delete shows
   *  it is sending, while every other row's is held with why. */
  deleting: string;
  onRetry: (opId: string) => void;
  onDiscard: (opId: string) => void;
  /**
   * Why a pending message's retry cannot be sent now — the room's refusal or
   * the browser's reason for `post_message` — or "" where it can. The retry
   * is a press like any other and is held with its sentence, never sent into
   * a refusal this tab could have said itself.
   */
  retryHold: string;
  /** The newest row drawn, reported as rows scroll past. Whether anybody could
   *  see it, and so whether it counts as read, is the room's (`Room.tsx`). */
  onSeen: (view: ChatMessageView) => void;
  viewer: string;
  /**
   * Whether this reader MODERATES — may take anybody's message down — which
   * the authority table gives an admin and nobody else (`delete_message` is
   * the author's own, or an admin's). Read off the viewer's own permissions,
   * which the engine answers, never decided here.
   */
  moderates: boolean;
  nameOf: (handle: string) => string;
  /**
   * The three changes a message takes, each its own press (`useAct`): the
   * browser's reason for each — offline, a credential acting as no seat, a
   * person the engine does not make THAT change for — is the act's own, and
   * one tool's sentence is never another's, since the engine serves each of
   * them to a person on its own (`viewer.acts`).
   */
  writes: MessageWrites;
  /**
   * Why this ROOM refuses each of a message's changes from this reader, or ""
   * where it takes it — ONE PER CHANGE, because the engine's rule is one per
   * tool rather than the room's rule for a post: a reaction is held outside a
   * private room as a post is, an edit and a delete reach past membership
   * (their author's, and an admin's moderation), and an archived room takes
   * all three, since an archive closes a room to new messages and to nothing
   * else. A kind this build cannot classify refuses every one of them.
   * Every control stays DRAWN and is held with its sentence: a write control
   * is never hidden, and one that vanished from a room would read as a
   * permission this reader lacks.
   */
  rules: MessageRules;
  /**
   * The seats running a turn against a message in this room.
   *
   * THE INDICATOR GOES WHERE THE REPLY WILL LAND, which is what the frame's
   * `thread` field is for: a seat answering in a thread is working under THAT
   * message, and an indicator at the top of the room would put it somewhere
   * the answer is not going to appear.
   */
  working: ChatWorking[];
}

/** The presses a message's changes are made through. */
export interface MessageWrites {
  react: Act<"react_to_message">;
  edit: Act<"edit_message">;
  remove: Act<"delete_message">;
}

/** Why the room refuses each of a message's changes, "" where it takes it. */
export interface MessageRules {
  /** `react_to_message`. */
  react: string;
  /** `edit_message`. */
  edit: string;
  /** `delete_message`. */
  remove: string;
}

/** What Save says over a rewrite that is the message as it stands. */
export const NOTHING_CHANGED = "Nothing has changed yet.";

/** What a second message's Delete says while the first one's is out. */
export const DELETE_IN_FLIGHT = "Waiting for the last delete's answer before another.";

/** What a second editor's Save says while another rewrite is out. */
export const EDIT_IN_FLIGHT = "Waiting for the last edit's answer before another.";

export function Transcript(props: TranscriptProps) {
  const { tail, pending, onOlder, paging, onSeen } = props;
  const scroller = useRef<HTMLDivElement | null>(null);
  const heights = useRef(new Map<string, number>());
  const [metrics, setMetrics] = useState({ scrollTop: 0, viewport: 0 });
  const [following, setFollowing] = useState(true);
  // The newest row the reader has actually had on screen. Kept by the room's
  // own sequence, which an edit does not move — so a corrected typo is not an
  // arrival.
  const [seenSeq, setSeenSeq] = useState(0);

  // OLDEST AT THE TOP, which is the order a conversation is read in and the
  // reverse of the order the engine answers in.
  const rows = useMemo(() => [...tail.messages].reverse(), [tail.messages]);

  const measure = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    setMetrics({ scrollTop: el.scrollTop, viewport: el.clientHeight });
    setFollowing(el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLACK_PX);
  }, []);

  useEffect(() => {
    measure();
    const el = scroller.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [measure]);

  const view = windowFor({
    count: rows.length,
    heightOf: (index) => heights.current.get(rows[index]?.message.id ?? "") ?? ROW_ESTIMATE,
    scrollTop: metrics.scrollTop,
    viewport: metrics.viewport,
  });
  const drawn = rows.slice(view.start, view.end);

  // FOLLOW ONLY FROM THE BOTTOM. In a layout effect so the jump happens in the
  // same frame the rows landed in: after the paint it is a visible lurch.
  useLayoutEffect(() => {
    if (!following) return;
    const el = scroller.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [rows.length, pending.length, following]);

  // WHAT THE READER HAS SEEN is the newest row drawn, and it is reported
  // rather than decided here: how far that moves a durable cursor is the
  // room's business, and a transcript that wrote one would be a second policy.
  useEffect(() => {
    const newest = drawn[drawn.length - 1];
    if (!newest) return;
    if (newest.channel_seq > seenSeq) setSeenSeq(newest.channel_seq);
    onSeen(newest);
    // `drawn` is rebuilt every render; the row that matters is its last one.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [drawn[drawn.length - 1]?.message.id, onSeen]);

  const held = rows.filter((row) => row.channel_seq > seenSeq).length;

  return (
    <>
      <div className="chat-scroller" ref={scroller} onScroll={measure} role="log" tabIndex={0}>
        {tail.older ? (
          <div className="chat-older">
            <Button variant="ghost" onClick={onOlder} disabled={paging}>
              {paging ? "Reading…" : "Older messages"}
            </Button>
          </div>
        ) : (
          tail.loaded &&
          rows.length > 0 && <div className="chat-older t-caption muted">The start of the room</div>
        )}

        {view.padTop > 0 && <div style={{ height: view.padTop }} aria-hidden="true" />}
        {drawn.map((row, index) => (
          <Row
            key={row.message.id}
            row={row}
            previous={index === 0 ? undefined : drawn[index - 1]}
            measure={(id, height) => heights.current.set(id, height)}
            threadRoot={props.threadRoot}
            onOpenThread={props.onOpenThread}
            onReact={props.onReact}
            viewer={props.viewer}
            moderates={props.moderates}
            nameOf={props.nameOf}
            writes={props.writes}
            rules={props.rules}
            onEdit={props.onEdit}
            onDelete={props.onDelete}
            deleting={props.deleting}
            working={props.working.filter((seat) => seat.thread === row.message.id)}
          />
        ))}
        {view.padBottom > 0 && <div style={{ height: view.padBottom }} aria-hidden="true" />}

        {pending.map((row) => (
          <PendingMessage
            key={row.opId}
            row={row}
            nameOf={props.nameOf}
            viewer={props.viewer}
            onRetry={props.onRetry}
            onDiscard={props.onDiscard}
            blocked={props.retryHold}
          />
        ))}
      </div>

      {!following && (
        <div className="chat-jump">
          <NewItemsNotice
            count={held}
            noun="new message"
            onShow={() => {
              const el = scroller.current;
              if (el) el.scrollTop = el.scrollHeight;
              setFollowing(true);
            }}
          />
        </div>
      )}
    </>
  );
}

/**
 * What one row needs.
 *
 * A ROW RE-RENDERS WITH THE TRANSCRIPT, on every page merge and every press's
 * busy flag: it is not memoised, and the writes and rules it is handed are
 * rebuilt on every render of the room. What bounds that cost is the WINDOW —
 * only the rows the reader is over, and a screen and a half either side
 * (`OVERSCAN_PX`), are rendered at all — not the props, so a room paged back
 * through an afternoon redraws a screenful rather than every row it holds.
 */
interface RowProps {
  row: ChatMessageView;
  /** The row above it in the WINDOW, for the run-of-speech grouping. The first
   *  row of a window has none and draws its author, which is right: a row that
   *  scrolled into view has to say who said it. */
  previous: ChatMessageView | undefined;
  measure: (id: string, height: number) => void;
  threadRoot: string;
  onOpenThread: (rootID: string) => void;
  onReact: (messageID: string, emoji: string, remove: boolean) => void;
  onEdit: (messageID: string, body: string) => Promise<boolean>;
  onDelete: (messageID: string) => void;
  deleting: string;
  viewer: string;
  moderates: boolean;
  nameOf: (handle: string) => string;
  writes: MessageWrites;
  rules: MessageRules;
  /** Seats working on an answer UNDER this message. */
  working: ChatWorking[];
}

/**
 * Who said a message, and when — for the transcript's rows and the thread's
 * alike, so the two cannot come to name one author two ways.
 *
 * THE PERSON BY NAME, AND AN AGENT SAID, by the one reading every surface
 * gives an author kind (`kindOfAuthor`): a person's message — `human`, the
 * seat they hold, however they wrote it (ADR-0024) — and a credential's
 * `operator` both read as a person, so neither is tagged; a tag naming the
 * kind would put a fact about how somebody signed in on every human message
 * in every room. An AGENT is said, because a seat speaking for itself is a
 * fact a reader weighs a remark by.
 */
export function AuthorLine({
  message,
  nameOf,
}: {
  message: ChatMessageView["message"];
  nameOf: (handle: string) => string;
}) {
  return (
    <div className="row gap-2 chat-author">
      <strong className="truncate t-cell">{nameOf(message.author)}</strong>
      {kindOfAuthor(message.author_kind) === "agent" && <Tag appearance="outline">agent</Tag>}
      <span className="t-caption muted" title={fmtDateTime(message.created_at)}>
        {fmtTime(message.created_at)}
      </span>
    </div>
  );
}

/** Why opening the editor is held: the room's rule, then the browser's. */
function editHold(rule: string, edit: Act<"edit_message">): string {
  return rule || (edit.access.can ? "" : edit.access.reason);
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
 * A rewrite open in its row's editor, a field of the room's one draft for the
 * person's own assistant (`app/assistant/boxes.ts`): named by the message,
 * set through the editor's own setter, and refused while its save is out. Put
 * there by this child only while the editor is open, so a transcript of
 * hundreds of rows adds the one box being written rather than an empty one for
 * every row.
 */
function RewriteBox({
  view,
  value,
  apply,
  saving,
}: {
  view: ChatMessageView;
  value: string;
  apply: (next: string) => void;
  saving: boolean;
}) {
  useDraftBox({
    path: `rewrite.${view.channel_seq}`,
    label: rewriteLabel(view.message),
    hint: "Plain text",
    // AFTER THE ROOM'S AND THE THREAD'S BOXES, in the order the room said them.
    rank: 2 + view.channel_seq,
    value,
    set: apply,
    refuses: (next) => (saving ? "That rewrite is being saved." : bodyRefusal(next)),
  });
  return null;
}

/** One message. */
function Row({
  row,
  previous,
  measure,
  threadRoot,
  onOpenThread,
  onReact,
  onEdit,
  onDelete,
  deleting,
  viewer,
  moderates,
  nameOf,
  writes,
  rules,
  working,
}: RowProps) {
  const { message } = row;
  const node = useRef<HTMLDivElement | null>(null);
  // THE REWRITE, or null while nothing is being rewritten. Not the empty
  // string: a person clearing the box to keep only a message's links has
  // emptied the rewrite, not closed the editor.
  const [editing, setEditing] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  // TWO PRESSES, NOT A DIALOG. A delete is a tombstone rather than an erasure
  // — the row survives with its body blanked — so the cost of a misclick is
  // real and recoverable-looking rather than catastrophic, and a modal over a
  // conversation to take back one line is heavier than the act.
  const [confirming, setConfirming] = useState(false);
  // MEASURED AFTER EVERY RENDER, because a row's height changes with its own
  // content: an edit, a reaction appearing, a link unfurling into a second
  // line. A height recorded once would make the spacers wrong for the rest of
  // the session.
  useLayoutEffect(() => {
    if (node.current) measure(message.id, node.current.offsetHeight);
  });

  // ONE RUN OF SPEECH, drawn as one: the same author inside a few minutes
  // repeats neither their name nor the clock. It is the same message either
  // way — the grouping is about reading, and every row still carries its own
  // time on hover.
  const grouped =
    previous !== undefined &&
    previous.message.author === message.author &&
    !previous.message.thread_root === !message.thread_root &&
    Date.parse(message.created_at) - Date.parse(previous.message.created_at) < 5 * 60_000;

  const deleted = !!message.deleted_at;
  const mine = message.author === viewer;
  // WHY THE REWRITE WOULD CHANGE NOTHING THE ENGINE TAKES, after the room's
  // own rule: the same words again are no edit at all — the engine has no
  // same-content no-op, so a Save there would write a record and draw
  // "edited" on a message nobody changed — `edit_message` refuses an empty
  // body whatever links the message keeps, because emptying a message is a
  // delete — and a rewrite past the cap is refused as the composer's message
  // would be, measured the same way (`bodyBudget`), before the round trip
  // rather than after it. A save that silently did nothing is a button
  // somebody presses twice.
  const rewrite = (editing ?? "").trim();
  const unchanged = rewrite === (message.body ?? "").trim();
  const budget = bodyBudget(editing ?? "");
  const saveRule =
    rules.edit ||
    (unchanged
      ? NOTHING_CHANGED
      : !rewrite
        ? "Write the message first. To take it back, delete it."
        : budget.over
          ? SHORTEN_FIRST
          : "");
  // ONE REWRITE OUT AT A TIME, said on every other editor's Save rather than
  // dropped: the press is the hook's, and it takes one at a time.
  const saveBlocked = saveRule || (writes.edit.busy && !saving ? EDIT_IN_FLIGHT : "");
  const save = async () => {
    // THE BUTTON'S OWN GATE, because a form submits on its own: a second
    // submit while the first answer was out would rewrite the message again
    // under a new operation key.
    if (saving || !pressable(writes.edit, saveBlocked || undefined)) return;
    setSaving(true);
    try {
      if (await onEdit(message.id, rewrite)) setEditing(null);
    } finally {
      setSaving(false);
    }
  };
  // A DELETE IS HELD BY THE ROOM'S RULE FOR IT, then — while another
  // message's delete is out — with why, rather than taking a press it drops.
  const deleteBlocked =
    rules.remove || (writes.remove.busy && deleting !== message.id ? DELETE_IN_FLIGHT : "");
  // A REACTION IS HELD BY THE ROOM'S RULE FOR IT, then the browser's own
  // reason for `react_to_message`. Not by a reaction in flight: two chips are
  // two independent toggles.
  const reactHold = rules.react || (writes.react.access.can ? "" : writes.react.access.reason);

  return (
    <div
      className={`chat-message${deleted ? " tombstone" : ""}`}
      ref={node}
      // AN ATTRIBUTE RATHER THAN AN `id`, because an element id is an anchor
      // and this product's addresses live in the hash: `#m-…` would be a
      // promise that pasting it scrolls somebody to the message, which needs
      // the transcript to page back until it finds one and does not exist.
      data-message-id={message.id}
    >
      {!grouped && <AuthorLine message={message} nameOf={nameOf} />}

      {deleted ? (
        // THE ROW SURVIVES SO THE THREAD DOES. A delete blanks the body and
        // keeps the message, which is what lets a reply still resolve to what
        // it answered — so the row says what happened rather than disappearing
        // and taking its thread's first line with it.
        <p className="chat-body muted">
          Deleted{message.deleted_by ? ` by ${nameOf(message.deleted_by)}` : ""}.
        </p>
      ) : editing !== null ? (
        <form
          className="col gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            void save();
          }}
        >
          <RewriteBox view={row} value={editing} apply={setEditing} saving={saving} />
          <BodyField budget={budget}>
            {(field) => (
              <Textarea
                id={field.id}
                aria-describedby={field.describedBy}
                autoResize
                autoFocus
                value={editing}
                error={budget.over}
                aria-label="Rewrite this message"
                onChange={(event) => setEditing(event.target.value)}
              />
            )}
          </BodyField>
          <div className="row gap-2">
            {/* THE REFUSAL IS DRAWN ABOVE THE TRANSCRIPT, once for every
                row's Save, so this button draws none of its own. */}
            <WriteButton
              variant="primary"
              size="small"
              type="submit"
              write={writes.edit}
              pressing={saving}
              blocked={saveBlocked || undefined}
              showRefusal={false}
              onPress={() => void save()}
            >
              Save
            </WriteButton>
            <Button variant="ghost" size="small" onClick={() => setEditing(null)} disabled={saving}>
              Cancel
            </Button>
          </div>
        </form>
      ) : (
        <p className="chat-body">{message.body}</p>
      )}

      <MessageLinks links={message.links} />

      <div className="row wrap gap-2 chat-meta">
        {message.edited_at && (
          <span className="t-caption muted" title={fmtDateTime(message.edited_at)}>
            edited
          </span>
        )}
        {message.collective && <span className="t-caption muted">to the whole room</span>}
        {(row.reactions ?? []).map((reaction) => (
          // THE KIT'S TOGGLE: pressed is the reaction this reader put there,
          // drawn from the state a screen reader is told, so the two cannot
          // disagree. Held — focusable, with its sentence — by the room's
          // rule for a reaction or the browser's own reason.
          <Button
            key={reaction.emoji}
            variant="outline"
            size="small"
            shape="pill"
            pressed={!!reaction.mine}
            disabledReason={reactHold || undefined}
            title={reactHold || undefined}
            onClick={() => onReact(message.id, reaction.emoji, !!reaction.mine)}
            aria-label={`${reaction.emoji}, ${reaction.count}`}
          >
            <span aria-hidden="true">{reaction.emoji}</span>
            <Count value={reaction.count} />
          </Button>
        ))}

        {working.map((seat) => (
          <StatusDot key={seat.handle} tone="info" pulse className="t-caption">
            {nameOf(seat.handle)} {seat.status || "is working on a reply"}
          </StatusDot>
        ))}

        {/* EDIT IS THE AUTHOR'S ALONE, not even an admin's: a remark somebody
            else can rewrite is a remark attributed to a person who did not
            make it, and the engine refuses it from anybody else. Opening the
            editor writes nothing, so it is held by what would hold its Save
            before any word is changed — the room's rule, then the browser's
            reason — rather than by a rewrite not yet typed. */}
        {mine && !deleted && editing === null && (
          <Button
            variant="ghost"
            size="small"
            disabledReason={editHold(rules.edit, writes.edit) || undefined}
            title={editHold(rules.edit, writes.edit) || undefined}
            onClick={() => setEditing(message.body ?? "")}
          >
            Edit
          </Button>
        )}
        {/* DELETE IS THE AUTHOR'S, AND AN ADMIN'S MODERATION: offered on the
            reader's own messages, and on everybody's to a reader who
            moderates — the authority table's rule, which the engine judges
            against the message's stored author — and the tombstone records
            who did it. Nowhere else, for Edit's reason: a Delete on somebody
            else's words that the engine refuses every reader but an admin
            reads as a gesture this reader could make. Held by the room's rule
            for a delete, which reaches past membership and past an archive.
            Its refusal is drawn above the transcript, once. */}
        {(mine || moderates) && !deleted && editing === null && (
          <WriteButton
            variant={confirming ? "danger" : "ghost"}
            size="small"
            write={writes.remove}
            pressing={deleting === message.id}
            blocked={deleteBlocked || undefined}
            showRefusal={false}
            onPress={() => {
              if (confirming) {
                onDelete(message.id);
                setConfirming(false);
              } else {
                setConfirming(true);
              }
            }}
            onBlur={() => setConfirming(false)}
          >
            {confirming ? "Really delete?" : "Delete"}
          </WriteButton>
        )}

        {/* A THREAD IS ONE LEVEL DEEP, so a message that is ALREADY a reply
            offers no thread of its own: it says which thread it is in, and
            that thread is the only one it can ever belong to. */}
        {message.thread_root ? (
          <Button
            variant="ghost"
            size="small"
            onClick={() => onOpenThread(message.thread_root ?? "")}
          >
            In thread
          </Button>
        ) : (
          <Button
            variant="ghost"
            size="small"
            leadingIcon={<SplitGlyph size="sm" />}
            pressed={threadRoot === message.id}
            onClick={() => onOpenThread(message.id)}
          >
            {threadRoot === message.id ? "Thread open" : "Reply in thread"}
          </Button>
        )}
      </div>
    </div>
  );
}
