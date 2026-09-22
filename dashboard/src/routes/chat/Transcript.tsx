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

import { Button, Count, NewItemsNotice, Tag, Textarea } from "@crewlethq/ui";
import { ForkRightGlyph } from "@crewlethq/icons/glyphs";
import { fmtDateTime, fmtTime } from "~/lib/format.ts";
import type { ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { mentionsIn } from "./Composer.tsx";
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
  /** Rewrite one's own message. The mentions travel again because the body
   *  they were derived from did. */
  onEdit: (messageID: string, body: string, mentions: string[]) => void;
  /** Take one's own words back. The row survives as a tombstone. */
  onDelete: (messageID: string) => void;
  /** The roster, for re-resolving what an edit names. */
  handles: readonly string[];
  onRetry: (operationID: string) => void;
  onDiscard: (operationID: string) => void;
  /** The furthest point the reader can currently see, as rows scroll past. */
  onSeen: (view: ChatMessageView) => void;
  viewer: string;
  nameOf: (handle: string) => string;
  /** Whether this room takes writes from this reader at all. */
  writable: boolean;
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
            <Button variant="tertiary" onClick={onOlder} disabled={paging}>
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
            nameOf={props.nameOf}
            writable={props.writable}
            onEdit={props.onEdit}
            onDelete={props.onDelete}
            handles={props.handles}
            working={props.working.filter((seat) => seat.thread === row.message.id)}
          />
        ))}
        {view.padBottom > 0 && <div style={{ height: view.padBottom }} aria-hidden="true" />}

        {pending.map((row) => (
          <PendingMessage
            key={row.operationID}
            row={row}
            nameOf={props.nameOf}
            viewer={props.viewer}
            onRetry={props.onRetry}
            onDiscard={props.onDiscard}
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

/** What one row needs, which is deliberately not the whole transcript's
 *  props: a row that took them would re-render on a page merge it has nothing
 *  to do with, hundreds of times over. */
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
  onEdit: (messageID: string, body: string, mentions: string[]) => void;
  onDelete: (messageID: string) => void;
  handles: readonly string[];
  viewer: string;
  nameOf: (handle: string) => string;
  writable: boolean;
  /** Seats working on an answer UNDER this message. */
  working: ChatWorking[];
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
  handles,
  viewer,
  nameOf,
  writable,
  working,
}: RowProps) {
  const { message } = row;
  const node = useRef<HTMLDivElement | null>(null);
  const [editing, setEditing] = useState("");
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

  return (
    <div
      className={`chat-message${deleted ? " tombstone" : ""}`}
      ref={node}
      data-mine={mine || undefined}
      // AN ATTRIBUTE RATHER THAN AN `id`, because an element id is an anchor
      // and this product's addresses live in the hash: `#m-…` would be a
      // promise that pasting it scrolls somebody to the message, which needs
      // the transcript to page back until it finds one and does not exist.
      data-message-id={message.id}
    >
      {!grouped && (
        <div className="row gap-2 chat-author">
          <strong className="truncate t-cell">{nameOf(message.author)}</strong>
          {message.author_kind === "agent" && <Tag appearance="outline">agent</Tag>}
          {message.author_kind === "operator" && <Tag appearance="outline">operator</Tag>}
          <span className="t-caption muted" title={fmtDateTime(message.created_at)}>
            {fmtTime(message.created_at)}
          </span>
        </div>
      )}

      {deleted ? (
        // THE ROW SURVIVES SO THE THREAD DOES. A delete blanks the body and
        // keeps the message, which is what lets a reply still resolve to what
        // it answered — so the row says what happened rather than disappearing
        // and taking its thread's first line with it.
        <p className="chat-body muted">
          Deleted{message.deleted_by ? ` by ${nameOf(message.deleted_by)}` : ""}.
        </p>
      ) : editing ? (
        <form
          className="col gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            const body = editing.trim();
            if (!body) return;
            onEdit(message.id, body, mentionsIn(body, handles).mentions);
            setEditing("");
          }}
        >
          <Textarea
            autoResize
            autoFocus
            value={editing}
            aria-label="Rewrite this message"
            onChange={(event) => setEditing(event.target.value)}
          />
          <div className="row gap-2">
            <Button variant="primary" type="submit">
              Save
            </Button>
            <Button variant="tertiary" onClick={() => setEditing("")}>
              Cancel
            </Button>
          </div>
        </form>
      ) : (
        <p className="chat-body">{message.body}</p>
      )}

      {message.links && message.links.length > 0 && (
        <div className="row wrap gap-2">
          {message.links.map((link) => (
            <a key={link} className="t-link t-caption truncate" href={link} rel="noreferrer">
              {link}
            </a>
          ))}
        </div>
      )}

      <div className="row wrap gap-2 chat-meta">
        {message.edited_at && (
          <span className="t-caption muted" title={fmtDateTime(message.edited_at)}>
            edited
          </span>
        )}
        {message.collective && <span className="t-caption muted">to the whole room</span>}
        {(row.reactions ?? []).map((reaction) => (
          <button
            key={reaction.emoji}
            type="button"
            className={`chat-reaction${reaction.mine ? " selected" : ""}`}
            disabled={!writable}
            onClick={() => onReact(message.id, reaction.emoji, !!reaction.mine)}
            aria-pressed={!!reaction.mine}
            aria-label={`${reaction.emoji}, ${reaction.count}`}
          >
            <span aria-hidden="true">{reaction.emoji}</span>
            <Count value={reaction.count} />
          </button>
        ))}

        {working.map((seat) => (
          <span key={seat.handle} className="row gap-1 chat-working t-caption">
            <span className="dot working" aria-hidden="true" />
            <span>
              {nameOf(seat.handle)} {seat.status || "is working on a reply"}
            </span>
          </span>
        ))}

        {/* AUTHOR-ONLY, AND NARROWER THAN THE ROOM'S OWN GATE. The engine
            refuses an edit or a delete from anybody but the message's author
            (or an operator, who does it from somewhere else) — a remark
            somebody else can rewrite is a remark attributed to a person who
            did not make it. */}
        {mine && !deleted && !editing && writable && (
          <>
            <button
              type="button"
              className="t-link t-caption"
              onClick={() => setEditing(message.body ?? "")}
            >
              edit
            </button>
            <button
              type="button"
              className="t-link t-caption"
              onClick={() => {
                if (confirming) {
                  onDelete(message.id);
                  setConfirming(false);
                } else {
                  setConfirming(true);
                }
              }}
              onBlur={() => setConfirming(false)}
            >
              {confirming ? "really delete?" : "delete"}
            </button>
          </>
        )}

        {/* A THREAD IS ONE LEVEL DEEP, so a message that is ALREADY a reply
            offers no thread of its own: it says which thread it is in, and
            that thread is the only one it can ever belong to. */}
        {message.thread_root ? (
          <button
            type="button"
            className="t-link t-caption"
            onClick={() => onOpenThread(message.thread_root ?? "")}
          >
            in thread
          </button>
        ) : (
          <button
            type="button"
            className="t-link t-caption row gap-1"
            aria-pressed={threadRoot === message.id}
            onClick={() => onOpenThread(message.id)}
          >
            <ForkRightGlyph size="xs" />
            {threadRoot === message.id ? "thread open" : "reply in thread"}
          </button>
        )}
      </div>
    </div>
  );
}
