/**
 * A room's transcript, on the kit's `ChatThread`: each day under its rule
 * with its date pill pinned near the top while the day is in view (a Jump to
 * menu), the New line where the reader left off, a banner floating over the
 * list while that line is out of view, and the room's intro where the
 * history reaches its very beginning.
 *
 * # The page moves only when the reader is not reading
 *
 * At the foot of the list a new message follows, because that is somebody
 * watching a conversation; scrolled up, the list stays where the reader put
 * it (`ChatThread`'s own rule). A short room rests on the composer rather
 * than under its intro at the top of the view.
 *
 * # Only what is on screen is in the DOM
 *
 * A reader who pages back through an afternoon holds hundreds of rows, each
 * of which can be a pasted stack trace. The window is computed from measured
 * heights (`messages.ts`'s `windowFor`), and the rows that are not drawn are
 * replaced by spacers of their height, inside the day they belong to, so the
 * scrollbar describes the whole conversation and a day's pill still pins
 * over the part of the day that is drawn.
 *
 * # What arrives while the reader reads back is counted, not pulled in
 *
 * Scrolled up, a new message is COUNTED and said over the foot of the list
 * ("3 new messages"), and the reader comes back to it when they choose. The
 * count is KEYED ON IDENTITY, by the room's own sequence: an edit, a reaction
 * or a delete arriving for a message already on screen is not an arrival,
 * and counted by length a fixed typo would read as news.
 *
 * # The New line is where the reader left off when they came in
 *
 * It stays there for the visit, after the room is marked read underneath it:
 * a line that moved as the reader read would chase them down the list. A
 * jump to it, to a date or to the beginning reads back as far as it needs
 * first (`readBack`), since a room is held from its newest page down.
 */

import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import {
  Button,
  CHAT_THREAD_FOLLOW_WITHIN,
  ChatDay,
  ChatDivider,
  ChatThread,
  NewItemsNotice,
  type ChatThreadHandle,
} from "@crewlethq/ui";
import { ArrowUpGlyph } from "@crewlethq/icons/glyphs";
import { useNow } from "~/lib/clock.ts";
import { readerDay } from "~/lib/format.ts";
import type { ChatChannel, ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { packPosition } from "./cursor.ts";
import { dayOf, isUnread, layoutDays, timeOf, type Day } from "./days.ts";
import { DayJump, type Jumps } from "./DayJump.tsx";
import { MessageRow } from "./MessageRow.tsx";
import { windowFor, type Tail } from "./messages.ts";
import type { Outgoing } from "./outbox.ts";
import { PendingRow } from "./Pending.tsx";
import { RoomIntro, isNewRoom } from "./RoomIntro.tsx";
import { useRoomScope } from "./scope.tsx";

/**
 * How tall a message is assumed to be before it has been drawn: an author
 * line and one line of body at this density. Only ever a placeholder for rows
 * outside the window; every row that has been on screen is remembered at its
 * measured height.
 */
const ROW_ESTIMATE = 64;

/** What the room can ask of its transcript. */
export interface TranscriptHandle {
  /** The newest messages, followed again: after a send. */
  scrollToLatest: () => void;
}

/** How many messages the reader had not read when they came in, as the rail counted them. */
export interface EntryUnread {
  count: number;
  /** The count stopped at the engine's limit: "99+" rather than 99. */
  capped: boolean;
}

export interface TranscriptProps {
  channel: ChatChannel | undefined;
  /**
   * The room's bare name, or who a conversation is with: drawn beside the
   * room's glyph in the intro, so never with a `#` of its own.
   */
  title: string;
  /** What a sentence calls the room (`rooms.ts`' `roomWhere`): `#name`. */
  where: string;
  /** Who is in a direct conversation. */
  participants: readonly string[];
  tail: Tail;
  /** This room's own messages still on their way (not a thread's). */
  pending: readonly Outgoing[];
  /** The seats answering in this room, each under the message it answers. */
  working: readonly ChatWorking[];
  /** The packed place the reader had read through when they came in, or null. */
  since: number | null;
  sinceUnread: EntryUnread | null;
  paging: boolean;
  /** Ask for the page below the oldest row held. */
  onOlder: () => void;
  /**
   * Read older pages until `enough` says the held tail reaches what a jump
   * needs, or the room's start; resolves with the tail as it then stands.
   */
  readBack: (enough: (tail: Tail) => boolean) => Promise<Tail>;
  /** The newest row drawn, reported as rows scroll past. Whether it counts as
   *  read is the room's (`Room.tsx`). */
  onSeen: (view: ChatMessageView) => void;
  onMarkRead: () => void;
  /** A message a link, a mention or a search landed on, or "". */
  flash: string;
  /** The landing is done: the address can drop the message. */
  onLanded: () => void;
}

export const Transcript = forwardRef<TranscriptHandle, TranscriptProps>(
  function Transcript(props, ref) {
    const { tail, since, onSeen, readBack } = props;
    const scope = useRoomScope();
    const thread = useRef<ChatThreadHandle | null>(null);
    const head = useRef<HTMLDivElement | null>(null);
    const heights = useRef(new Map<string, number>());
    const [metrics, setMetrics] = useState({ scrollTop: 0, viewport: 0 });
    const [following, setFollowing] = useState(true);
    // THE NEWEST MESSAGE THE READER HAD IN VIEW AT THE FOOT, by the room's
    // own sequence, which an edit does not move.
    const [seenSeq, setSeenSeq] = useState(0);
    const today = readerDay(useNow());

    // OLDEST AT THE TOP, the order a conversation is read in and the reverse of
    // the order the engine answers in.
    const rows = useMemo(() => [...tail.messages].reverse(), [tail.messages]);
    const firstUnread = useMemo(
      () => rows.find((view) => isUnread(view, since, scope.viewer))?.message.id ?? "",
      [rows, since, scope.viewer],
    );
    const days = useMemo(() => layoutDays(rows, today, firstUnread), [rows, today, firstUnread]);

    const newestSeq = rows[rows.length - 1]?.channel_seq ?? 0;
    useEffect(() => {
      if (following && newestSeq > seenSeq) setSeenSeq(newestSeq);
    }, [following, newestSeq, seenSeq]);
    const arrived = following
      ? 0
      : rows.filter(
          (r) => r.channel_seq > seenSeq && r.message.author !== scope.viewer && !r.message.event,
        ).length;

    // --- the window ----------------------------------------------------------

    const scroller = useCallback(() => thread.current?.scroller() ?? null, []);
    const measure = useCallback(() => {
      const el = scroller();
      if (!el) return;
      const top = el.scrollTop - (head.current?.offsetHeight ?? 0);
      setMetrics((held) =>
        held.scrollTop === top && held.viewport === el.clientHeight
          ? held
          : { scrollTop: top, viewport: el.clientHeight },
      );
      setFollowing(el.scrollHeight - el.scrollTop - el.clientHeight <= CHAT_THREAD_FOLLOW_WITHIN);
    }, [scroller]);
    useEffect(() => {
      const el = scroller();
      if (!el) return;
      measure();
      el.addEventListener("scroll", measure, { passive: true });
      const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
      observer?.observe(el);
      return () => {
        el.removeEventListener("scroll", measure);
        observer?.disconnect();
      };
    }, [scroller, measure]);

    const heightOf = (index: number) =>
      heights.current.get(rows[index]?.message.id ?? "") ?? ROW_ESTIMATE;
    const view = windowFor({
      count: rows.length,
      heightOf,
      scrollTop: metrics.scrollTop,
      viewport: metrics.viewport,
    });
    const keep = useCallback((id: string, height: number) => heights.current.set(id, height), []);

    // AN OLDER PAGE KEEPS THE READER WHERE THEY WERE. Rows land above what they
    // are reading, and with the browser's own scroll anchoring off (the thread
    // anchors its foot itself) the list would jump down by the page's height.
    const before = useRef<{ first: string; height: number }>({ first: "", height: 0 });
    useLayoutEffect(() => {
      const el = scroller();
      const first = rows[0]?.message.id ?? "";
      const was = before.current;
      if (el && was.first && first !== was.first && rows.some((r) => r.message.id === was.first)) {
        el.scrollTop += el.scrollHeight - was.height;
      }
    }, [rows, scroller]);
    useLayoutEffect(() => {
      before.current = { first: rows[0]?.message.id ?? "", height: scroller()?.scrollHeight ?? 0 };
    });

    // WHAT THE READER HAS SEEN is the newest row drawn, reported rather than
    // decided here: how far that moves a durable cursor is the room's business.
    const newestDrawn = rows[Math.min(view.end, rows.length) - 1];
    useEffect(() => {
      if (newestDrawn) onSeen(newestDrawn);
    }, [newestDrawn?.message.id, onSeen]); // eslint-disable-line react-hooks/exhaustive-deps

    // --- jumps ---------------------------------------------------------------

    /** Bring a row to the view: its day's rule to the top, or the row to the middle. */
    const land = useCallback(
      (id: string, how: "day" | "middle") => {
        const el = scroller();
        if (!el) return;
        const index = rows.findIndex((r) => r.message.id === id);
        if (index >= 0) {
          // FIRST BY ARITHMETIC, so the row is inside the window and drawn,
          // then by the element itself once it is.
          let offset = head.current?.offsetHeight ?? 0;
          for (let i = 0; i < index; i++) offset += heightOf(i);
          el.scrollTop = Math.max(0, offset - (how === "middle" ? el.clientHeight / 2 : 0));
        }
        requestAnimationFrame(() => {
          const row = el.querySelector<HTMLElement>(`[data-message-id="${CSS.escape(id)}"]`);
          if (!row) return;
          if (how === "middle") thread.current?.reveal(row);
          else
            (row.closest<HTMLElement>(".crewlet-chat-day") ?? row).scrollIntoView({
              block: "start",
            });
        });
      },
      // `heightOf` reads the refs and `rows`, both named here.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      [rows, scroller],
    );

    const jumps: Jumps = {
      latest: () => thread.current?.scrollToLatest(),
      beginning: () => {
        void readBack((held) => held.older === "").then(() =>
          requestAnimationFrame(() => {
            const el = scroller();
            if (el) el.scrollTop = 0;
          }),
        );
      },
      toDay: (key) => {
        // READ BACK PAST THE START OF THAT DAY, so its first message is held.
        void readBack((held) => {
          const oldestHeld = held.messages[held.messages.length - 1];
          return !!oldestHeld && dayOf(oldestHeld.message.created_at) < key;
        }).then((held) => {
          const target = [...held.messages]
            .reverse()
            .find((r) => dayOf(r.message.created_at) >= key);
          if (target) requestAnimationFrame(() => land(target.message.id, "day"));
          else thread.current?.scrollToLatest();
        });
      },
    };

    useImperativeHandle(
      ref,
      () => ({ scrollToLatest: () => thread.current?.scrollToLatest() }),
      [],
    );

    // A MESSAGE OPENED FROM A LINK, A MENTION OR A SEARCH LANDS IN THE MIDDLE
    // OF THE VIEW, with the messages before and after it in sight, read back to
    // first where it is older than what is held.
    const [flashed, setFlashed] = useState("");
    const { flash, onLanded } = props;
    useEffect(() => {
      if (!flash || !tail.loaded) return;
      let live = true;
      void readBack((held) => held.messages.some((r) => r.message.id === flash)).then((held) => {
        if (!live) return;
        if (held.messages.some((r) => r.message.id === flash)) {
          setFlashed(flash);
          requestAnimationFrame(() => land(flash, "middle"));
        }
        onLanded();
      });
      return () => {
        live = false;
      };
      // Once per landing: the tail it reads back through is the hook's own.
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [flash, tail.loaded]);

    // --- the banner ----------------------------------------------------------

    const unreadHeld = useMemo(
      () => rows.filter((r) => isUnread(r, since, scope.viewer)).length,
      [rows, since, scope.viewer],
    );
    const oldest = packPosition(rows[0]?.position);
    const reachesSince =
      tail.older === "" || (since !== null && oldest !== null && oldest <= since);
    const entry = props.sinceUnread;
    const count = reachesSince ? unreadHeld : Math.max(unreadHeld, entry?.count ?? 0);
    const capped = !reachesSince && !!entry?.capped && count >= entry.count;
    const sinceAt = rows.find((r) => r.message.id === firstUnread)?.message.created_at;
    const [bannerGone, setBannerGone] = useState(false);
    const banner = count > 0 && !bannerGone && (firstUnread !== "" || !reachesSince);

    // THE BANNER IS FOR A NEW LINE OUT OF VIEW: once the reader can see the
    // line, it has said what it had to say.
    useEffect(() => {
      if (!banner || typeof IntersectionObserver === "undefined") return;
      const line = scroller()?.querySelector('[data-divider="unread"]');
      if (!line) return;
      const seen = new IntersectionObserver((entries) => {
        if (entries.some((e) => e.isIntersecting)) setBannerGone(true);
      });
      seen.observe(line);
      return () => seen.disconnect();
    }, [banner, days, view.start, view.end, scroller]);

    const jumpToNew = () => {
      setBannerGone(true);
      void readBack((held) => {
        const end = packPosition(held.messages[held.messages.length - 1]?.position);
        return since !== null && end !== null && end <= since;
      }).then((held) => {
        const first = [...held.messages].reverse().find((r) => isUnread(r, since, scope.viewer))
          ?.message.id;
        if (first) requestAnimationFrame(() => land(first, "day"));
      });
    };

    // --- drawing -------------------------------------------------------------

    const first = props.channel ? dayOf(props.channel.created_at) : "";
    const newest = dayOf(rows[rows.length - 1]?.message.created_at);
    const drawnDays: ReactNode[] = [];
    for (const day of days) {
      const inWindow = day.rows.filter((r) => r.index >= view.start && r.index < view.end);
      if (inWindow.length === 0) continue;
      const above = day.rows
        .filter((r) => r.index < view.start)
        .reduce((sum, r) => sum + heightOf(r.index), 0);
      const below = day.rows
        .filter((r) => r.index >= view.end)
        .reduce((sum, r) => sum + heightOf(r.index), 0);
      drawnDays.push(
        <DayBlock
          key={day.key}
          day={day}
          rows={inWindow}
          above={above}
          below={below}
          today={today}
          newest={newest}
          first={first}
          jumps={jumps}
          keep={keep}
          working={props.working}
          flashed={flashed}
        />,
      );
    }
    const insideTop = sumAbove(days, view.start, heightOf);
    const insideBottom = sumBelow(days, view.end, heightOf);

    const fresh = isNewRoom(rows);
    return (
      <div className={`chat-transcript${banner ? " has-banner" : ""}`} data-transcript>
        {banner && (
          <div className="chat-unread-banner" role="status">
            <button type="button" onClick={jumpToNew}>
              <ArrowUpGlyph size="sm" />
              <span className="chat-unread-banner-text">
                {capped ? `${count}+` : count} new {count === 1 ? "message" : "messages"}
                {sinceAt && (
                  <span className="chat-unread-banner-since"> since {timeOf(sinceAt)}</span>
                )}
              </span>
            </button>
            <button
              type="button"
              className="chat-unread-banner-mark"
              onClick={() => {
                setBannerGone(true);
                props.onMarkRead();
              }}
            >
              Mark as read
            </button>
          </div>
        )}
        <ChatThread
          ref={thread}
          jump={false}
          rest="end"
          label={`Messages in ${props.where}`}
          busy={props.paging}
        >
          <div ref={head} className="chat-transcript-head">
            {tail.older ? (
              <div className="chat-older">
                <Button
                  variant="ghost"
                  size="small"
                  onClick={props.onOlder}
                  disabled={props.paging}
                >
                  {props.paging ? "Reading…" : "Older messages"}
                </Button>
              </div>
            ) : (
              tail.loaded &&
              props.channel && (
                <RoomIntro
                  channel={props.channel}
                  participants={props.participants}
                  title={props.title}
                  fresh={fresh}
                />
              )
            )}
          </div>
          {view.padTop - insideTop > 0 && (
            <div style={{ height: view.padTop - insideTop }} aria-hidden="true" />
          )}
          {drawnDays}
          {view.padBottom - insideBottom > 0 && (
            <div style={{ height: view.padBottom - insideBottom }} aria-hidden="true" />
          )}
          {props.pending.map((row) => (
            <PendingRow key={row.opId} row={row} />
          ))}
        </ChatThread>
        {arrived > 0 && (
          <div className="chat-new-below">
            <NewItemsNotice
              count={arrived}
              noun="new message"
              onShow={() => thread.current?.scrollToLatest()}
            />
          </div>
        )}
      </div>
    );
  },
);

/** The height of the rows above the window that sit inside the first drawn day. */
function sumAbove(days: readonly Day[], start: number, heightOf: (i: number) => number): number {
  const day = days.find((d) => d.rows.some((r) => r.index >= start));
  if (!day) return 0;
  return day.rows.filter((r) => r.index < start).reduce((sum, r) => sum + heightOf(r.index), 0);
}

/** The height of the rows below the window that sit inside the last drawn day. */
function sumBelow(days: readonly Day[], end: number, heightOf: (i: number) => number): number {
  const day = [...days].reverse().find((d) => d.rows.some((r) => r.index < end));
  if (!day) return 0;
  return day.rows.filter((r) => r.index >= end).reduce((sum, r) => sum + heightOf(r.index), 0);
}

/** One day: its rule, its pill as a Jump to menu, and its drawn rows between spacers. */
function DayBlock({
  day,
  rows,
  above,
  below,
  today,
  newest,
  first,
  jumps,
  keep,
  working,
  flashed,
}: {
  day: Day;
  rows: Day["rows"];
  above: number;
  below: number;
  today: string;
  newest: string;
  first: string;
  jumps: Jumps;
  keep: (id: string, height: number) => void;
  working: readonly ChatWorking[];
  flashed: string;
}) {
  return (
    <ChatDay
      label={day.label}
      unread={day.newOnRule ? "New" : undefined}
      pill={
        <DayJump
          label={day.label}
          day={day.key}
          today={today}
          newest={newest}
          first={first}
          jumps={jumps}
        />
      }
    >
      {above > 0 && <div style={{ height: above }} aria-hidden="true" />}
      {rows.map((row) => (
        <Measured key={row.view.message.id} id={row.view.message.id} keep={keep}>
          {row.newAbove && <ChatDivider variant="unread" label="New" />}
          <MessageRow
            view={row.view}
            continuation={row.continuation}
            working={working}
            flash={flashed === row.view.message.id}
          />
        </Measured>
      ))}
      {below > 0 && <div style={{ height: below }} aria-hidden="true" />}
    </ChatDay>
  );
}

/**
 * A row, measured after every render: a row's height changes with its own
 * content (an edit, a reaction, a preview), and a height recorded once would
 * make the spacers wrong for the rest of the visit.
 */
function Measured({
  id,
  keep,
  children,
}: {
  id: string;
  keep: (id: string, height: number) => void;
  children: ReactNode;
}) {
  const node = useRef<HTMLDivElement | null>(null);
  useLayoutEffect(() => {
    if (node.current) keep(id, node.current.offsetHeight);
  });
  return (
    <div ref={node} className="chat-row">
      {children}
    </div>
  );
}
