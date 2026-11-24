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
 * it, and the row at the top of the view stays where it stood while rows
 * above it are drawn at their real height or an older page lands
 * (`ChatThread`'s own rules, whose word on whether the reader is at the foot
 * this screen takes rather than measuring a second way). A short room rests
 * on the composer rather than under its intro at the top of the view.
 *
 * # A row renders when its message does
 *
 * The rows are memoized, and so is the transcript, against props the room
 * keeps stable: typing in the composer, a presence change, a scroll that
 * re-windows the list or a page landing below redraws no row whose message,
 * place or answering seats did not change.
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
 * # Moving the window moves nothing on screen
 *
 * What is not drawn is stood for at exactly the height it is drawn at, or the
 * page fights the reader: the thread holds the row at the top of the view in
 * place (`ChatThread`), so every pixel a spacer is off becomes a scroll the
 * page makes, that scroll moves the window, and the window moves the spacer
 * back. So a row's height is recorded only while the row is drawn (a row the
 * window removed is reported once more at no height at all), the observer
 * lets go of a row in the commit that removes it, heights are kept to the
 * fraction of a pixel, and the window is laid out in the coordinates the page
 * draws in: each day's label and rule counted above its first row
 * (`placedHeights`), and stood for with its rows when the day is not drawn
 * (`daysInWindow`).
 *
 * # History is read ahead of the reader
 *
 * A reader scrolling up meets older messages before the top of what is held:
 * once fewer than [READ_AHEAD_VIEWS] views of rows stand above the view, the
 * next older page is asked for, once per cursor, so a page that could not be
 * read is asked again only from the head's button. The thread holds the
 * reader's place while it lands.
 *
 * # What arrives while the reader reads back is counted, not pulled in
 *
 * Scrolled up, a new message is COUNTED and said over the foot of the list
 * ("3 new messages"), and the reader comes back to it when they choose. The
 * count is KEYED ON IDENTITY, by the room's own sequence: an edit, a reaction
 * or a delete arriving for a message already on screen is not an arrival,
 * and counted by length a fixed typo would read as news.
 *
 * # A landing is placed once its row is drawn
 *
 * A jump to a message, to a date or to the New line names a row the window
 * may not draw yet. The row is drawn whatever the window says, and brought
 * into view in the commit that draws it, through the thread, never in a
 * frame that guessed it would be drawn by then: a link opened on a fresh
 * load used to scroll to an estimate and look for a row that was not there.
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
  memo,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type MutableRefObject,
  type ReactNode,
} from "react";

import {
  Button,
  CHAT_MESSAGE_CLASS,
  ChatDay,
  ChatDivider,
  ChatThread,
  NewItemsNotice,
  type ChatThreadHandle,
} from "@crewlethq/ui";
import { ArrowUpGlyph } from "@crewlethq/icons/glyphs";
import { useClock } from "~/lib/clock.ts";
import { readerDay } from "~/lib/format.ts";
import type { ChatChannel, ChatMessageView, ChatWorking } from "~/protocol/index.ts";

import { packPosition } from "./cursor.ts";
import {
  dayOf,
  daysInWindow,
  isUnread,
  layoutDays,
  placedHeights,
  timeOf,
  type Day,
} from "./days.ts";
import { DayJump, type Jumps } from "./DayJump.tsx";
import { MessageRow } from "./MessageRow.tsx";
import { windowFor, type Tail } from "./messages.ts";
import type { Outgoing } from "./outbox.ts";
import { PendingRow } from "./Pending.tsx";
import { RoomIntro, isNewRoom } from "./RoomIntro.tsx";
import { useRoomScope } from "./scope.tsx";
import { saidBy } from "./speaker.ts";

/**
 * How tall a message is assumed to be before it has been drawn: an author
 * line and one line of body at this density. Only ever a placeholder for rows
 * outside the window; every row that has been on screen is remembered at its
 * measured height.
 */
const ROW_ESTIMATE = 64;

/** How many views of held rows above the view are left when the next older page is asked for. */
export const READ_AHEAD_VIEWS = 1.5;

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

export const Transcript = memo(
  forwardRef<TranscriptHandle, TranscriptProps>(function Transcript(props, ref) {
    const { tail, since, onSeen, readBack } = props;
    const scope = useRoomScope();
    const thread = useRef<ChatThreadHandle | null>(null);
    const head = useRef<HTMLDivElement | null>(null);
    const heights = useRef(new Map<string, number>());
    // THE NEW LINE THAT HAS GLOWED, by its first unread message (`useGlowOnce`).
    const glowed = useRef("");
    // WHERE THE VIEW STANDS, and the row that was first when it was read: an
    // older page landing above that row moves the window with it (`shift`).
    const [metrics, setMetrics] = useState({
      scrollTop: 0,
      viewport: 0,
      first: "",
    });
    // WHETHER THE READER IS AT THE FOOT: the thread's own answer.
    const [following, setFollowing] = useState(true);
    // THE NEWEST MESSAGE THE READER HAD IN VIEW AT THE FOOT, by the room's
    // own sequence, which an edit does not move.
    const [seenSeq, setSeenSeq] = useState(0);
    // A ROW BEING LANDED ON, drawn whatever the window says until it is placed.
    const [landing, setLanding] = useState<Landing | null>(null);
    // TODAY, which a day's rule reads: the transcript renders when the date
    // changes, not on every tick of the clock.
    const today = useClock(readerDay);
    // THE HEIGHT OF A DAY'S LABEL AND RULE, as the kit draws them: measured off
    // a drawn day, since the window places every day's first row under them.
    const [chrome, setChrome] = useState(0);

    // OLDEST AT THE TOP, the order a conversation is read in and the reverse of
    // the order the engine answers in.
    const rows = useMemo(() => [...tail.messages].reverse(), [tail.messages]);
    const firstRow = useRef("");
    firstRow.current = rows[0]?.message.id ?? "";
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
          (r) => r.channel_seq > seenSeq && !saidBy(r.message, scope.viewer) && !r.message.event,
        ).length;

    // --- the window ----------------------------------------------------------

    const scroller = useCallback(() => thread.current?.scroller() ?? null, []);
    const measure = useCallback(() => {
      const el = scroller();
      if (!el) return;
      const top = el.scrollTop - (head.current?.offsetHeight ?? 0);
      const first = firstRow.current;
      setMetrics((held) =>
        held.scrollTop === top && held.viewport === el.clientHeight && held.first === first
          ? held
          : { scrollTop: top, viewport: el.clientHeight, first },
      );
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

    // READ AHEAD OF THE READER: the next older page is asked for while there
    // is still room above the view to scroll into, once per cursor.
    const { onOlder, paging } = props;
    const askedFor = useRef("");
    useEffect(() => {
      if (!tail.older || paging || askedFor.current === tail.older || metrics.viewport <= 0) return;
      if (metrics.scrollTop > metrics.viewport * READ_AHEAD_VIEWS) return;
      askedFor.current = tail.older;
      onOlder();
    }, [tail.older, paging, metrics.scrollTop, metrics.viewport, onOlder]);

    const heightOf = (index: number) =>
      heights.current.get(rows[index]?.message.id ?? "") ?? ROW_ESTIMATE;
    // WHERE THE PAGE DRAWS EACH ROW: its height, and a day's label and rule
    // above the day's first row.
    const placed = placedHeights(days, heightOf, chrome);
    // AN OLDER PAGE LANDS ABOVE THE ROW THAT WAS FIRST when the view was last
    // read, and the thread holds the reader's place by that page's height
    // before the paint. The window is drawn where the view will then stand,
    // so the rows the reader is looking at stay drawn through the landing.
    const landedAt = metrics.first ? rows.findIndex((r) => r.message.id === metrics.first) : -1;
    let shift = 0;
    for (let i = 0; i < landedAt; i++) shift += placed(i);
    // A ROW BEING LANDED ON IS DRAWN: the window is the one around it.
    const view = windowFor({
      count: rows.length,
      heightOf: placed,
      scrollTop: metrics.scrollTop + shift,
      viewport: metrics.viewport,
      include: landing ? rows.findIndex((r) => r.message.id === landing.id) : undefined,
    });
    // EVERY DRAWN ROW'S HEIGHT, kept as it renders and as it reflows (a
    // narrower window, a preview that loads), since a row renders only when
    // its message changes. ONLY WHILE IT IS DRAWN: the observer reports a row
    // the window has just removed once more, at no height at all, and that
    // is not its height; recorded, the spacer standing for it shrank by the
    // whole row and the page jumped.
    const rowSizes = useMemo<RowMeasure>(() => {
      const ids = new Map<Element, string>();
      const sizes =
        typeof ResizeObserver === "undefined"
          ? null
          : new ResizeObserver((entries) => {
              for (const entry of entries) {
                const id = ids.get(entry.target);
                if (id && entry.target.isConnected)
                  heights.current.set(id, drawnHeight(entry.target));
              }
            });
      return {
        keep: (id, height) => heights.current.set(id, height),
        watch: (id, node) => {
          ids.set(node, id);
          sizes?.observe(node);
          return () => {
            ids.delete(node);
            sizes?.unobserve(node);
          };
        },
      };
    }, []);

    // WHAT THE READER HAS SEEN is the newest row drawn, reported rather than
    // decided here: how far that moves a durable cursor is the room's business.
    const newestDrawn = rows[Math.min(view.end, rows.length) - 1];
    useEffect(() => {
      if (newestDrawn) onSeen(newestDrawn);
    }, [newestDrawn?.message.id, onSeen]); // eslint-disable-line react-hooks/exhaustive-deps

    // --- jumps ---------------------------------------------------------------

    // THE LANDING, PLACED IN THE COMMIT THAT DREW ITS ROW, always through the
    // thread, which then knows the reader left the foot: the row to the middle,
    // or its day, which lands with its rule at the top when it outruns the
    // view. Read again at once, so the next window is the one around the view.
    useLayoutEffect(() => {
      if (!landing) return;
      const row = scroller()?.querySelector<HTMLElement>(
        `[data-message-id="${CSS.escape(landing.id)}"]`,
      );
      if (!row) return;
      thread.current?.reveal(
        landing.how === "day" ? (row.closest<HTMLElement>(".crewlet-chat-day") ?? row) : row,
      );
      if (landing.focus) row.focus({ preventScroll: true });
      measure();
      setLanding(null);
    }, [landing, rows, scroller, measure]);

    // HOME AND END REACH THE HELD EDGES. The kit walks the rows in the page and
    // this list draws a window of them, so from a focused row the transcript
    // lands on its first held or newest message itself, and focuses it.
    const edgeKeys = (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
      if (!(event.target as Element).matches(`.${CHAT_MESSAGE_CLASS}`)) return;
      const id = edgeOf(rows, event.key);
      if (!id) return;
      event.preventDefault();
      setLanding({ id, how: "middle", focus: true });
    };

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
          if (target) setLanding({ id: target.message.id, how: "day" });
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
          setLanding({ id: flash, how: "middle" });
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
        if (first) setLanding({ id: first, how: "middle" });
      });
    };

    // --- drawing -------------------------------------------------------------

    // THE SEATS ANSWERING UNDER EACH MESSAGE, so a row is handed its own and
    // a seat starting work under one message redraws that row alone.
    const workingOn = useMemo(() => {
      const out = new Map<string, ChatWorking[]>();
      for (const seat of props.working) {
        if (seat.thread) out.set(seat.thread, [...(out.get(seat.thread) ?? []), seat]);
      }
      return out;
    }, [props.working]);

    const first = props.channel ? dayOf(props.channel.created_at) : "";
    const newest = dayOf(rows[rows.length - 1]?.message.created_at);
    const laid = daysInWindow(days, view.start, view.end, heightOf, chrome);
    const drawnDays: ReactNode[] = laid.days.map(({ day, rows: inWindow, above, below }) => (
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
        measure={rowSizes}
        working={workingOn}
        flashed={flashed}
        line={firstUnread}
        glowed={glowed}
      />
    ));

    // THE DAY'S LABEL AND RULE, measured off the first drawn day: its height
    // less the rows and spacers this list put in it. Constant while the
    // stylesheets are, so this settles on the first commit that draws a day.
    useLayoutEffect(() => {
      const day = scroller()?.querySelector<HTMLElement>(".crewlet-chat-day");
      if (!day) return;
      let inside = 0;
      for (const child of day.querySelectorAll<HTMLElement>(
        ":scope > .chat-row, :scope > .chat-spacer",
      )) {
        inside += child.getBoundingClientRect().height;
      }
      const measured = day.getBoundingClientRect().height - inside;
      if (Math.abs(measured - chrome) >= 0.5) setChrome(measured);
    });

    const fresh = isNewRoom(rows);
    return (
      <div className="chat-transcript" data-transcript onKeyDownCapture={edgeKeys}>
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
          flush
          label={`Messages in ${props.where}`}
          busy={props.paging}
          onFollowChange={setFollowing}
        >
          <div ref={head} className="chat-transcript-head">
            {tail.older ? (
              <div className="chat-older">
                <Button variant="ghost" size="small" onClick={props.onOlder} loading={props.paging}>
                  {props.paging ? "Loading older messages" : "Older messages"}
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
          {laid.padTop > 0 && (
            <div className="chat-spacer" style={{ height: laid.padTop }} aria-hidden="true" />
          )}
          {drawnDays}
          {laid.padBottom > 0 && (
            <div className="chat-spacer" style={{ height: laid.padBottom }} aria-hidden="true" />
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
              label={arrivalsLabel}
              announcement={arrivalsAnnouncement}
              onShow={() => thread.current?.scrollToLatest()}
            />
          </div>
        )}
      </div>
    );
  }),
);

/** A row a jump is taking the reader to, and what is placed: the row mid view, or its day. */
interface Landing {
  id: string;
  how: "day" | "middle";
  /** Focus the row once placed: a key moved the reader there. */
  focus?: boolean;
}

/** The message Home or End lands on: the first held or the newest, never a room event. */
export function edgeOf(rows: readonly ChatMessageView[], key: string): string {
  const speech = rows.filter((row) => !row.message.event);
  const to = key === "Home" ? speech[0] : key === "End" ? speech.at(-1) : undefined;
  return to?.message.id ?? "";
}

/** How a drawn row reports its height: as it renders, and as it reflows. */
interface RowMeasure {
  keep: (id: string, height: number) => void;
  /** Watches a row's element for a change of size; returns the unwatch. */
  watch: (id: string, node: Element) => () => void;
}

const NO_WORKING: readonly ChatWorking[] = [];

/** "Show 3 new messages": what arrived while the reader was reading back. */
export function arrivalsLabel(count: number): string {
  return `Show ${newMessages(count)}`;
}

/** What a screen reader is told once as messages arrive below the reader. */
export function arrivalsAnnouncement(count: number): string {
  return `${newMessages(count)} arrived while you were reading.`;
}

function newMessages(count: number): string {
  return `${count.toLocaleString()} new ${count === 1 ? "message" : "messages"}`;
}

/**
 * Whether the New line for `line` (its first unread message, "" for none)
 * glows here: only the first mount that draws it, so the window drawing it
 * back on a scroll does not glow it again. Decided once per line per mount.
 */
export function useGlowOnce(glowed: MutableRefObject<string>, line: string): boolean {
  const decided = useRef<{ line: string; pulse: boolean } | null>(null);
  if (decided.current?.line !== line) {
    decided.current = { line, pulse: line !== "" && glowed.current !== line };
  }
  useEffect(() => {
    if (line) glowed.current = line;
  }, [glowed, line]);
  return decided.current.pulse;
}

/**
 * A drawn row's height to the fraction of a pixel, as the page lays it out:
 * a whole-pixel `offsetHeight` summed over a few hundred rows put the spacers
 * pixels off the rows they stand for.
 */
function drawnHeight(node: Element): number {
  return node.getBoundingClientRect().height;
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
  measure,
  working,
  flashed,
  line,
  glowed,
}: {
  day: Day;
  rows: Day["rows"];
  above: number;
  below: number;
  today: string;
  newest: string;
  first: string;
  jumps: Jumps;
  measure: RowMeasure;
  working: ReadonlyMap<string, readonly ChatWorking[]>;
  flashed: string;
  /** The first unread message, which the New line stands above, or "". */
  line: string;
  glowed: MutableRefObject<string>;
}) {
  const pulse = useGlowOnce(glowed, day.newOnRule ? line : "");
  return (
    <ChatDay
      label={day.label}
      unread={day.newOnRule ? "New" : undefined}
      pulse={pulse}
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
      {above > 0 && <div className="chat-spacer" style={{ height: above }} aria-hidden="true" />}
      {rows.map((row) => (
        <TranscriptRow
          key={row.view.message.id}
          view={row.view}
          continuation={row.continuation}
          newAbove={row.newAbove}
          working={working.get(row.view.message.id) ?? NO_WORKING}
          flash={flashed === row.view.message.id}
          measure={measure}
          glowed={glowed}
        />
      ))}
      {below > 0 && <div className="chat-spacer" style={{ height: below }} aria-hidden="true" />}
    </ChatDay>
  );
}

/**
 * One drawn row, measured as it renders and watched as it reflows: a row's
 * height changes with its own content (an edit, a reaction, a preview) and
 * with the transcript's width, and a height recorded once would make the
 * spacers wrong for the rest of the visit. MEMOIZED with the message row in
 * it, on props that change only when the row's own message or place does.
 */
const TranscriptRow = memo(function TranscriptRow({
  view,
  continuation,
  newAbove,
  working,
  flash,
  measure,
  glowed,
}: {
  view: ChatMessageView;
  continuation: boolean;
  newAbove: boolean;
  working: readonly ChatWorking[];
  flash: boolean;
  measure: RowMeasure;
  glowed: MutableRefObject<string>;
}) {
  const node = useRef<HTMLDivElement | null>(null);
  const id = view.message.id;
  const pulse = useGlowOnce(glowed, newAbove ? id : "");
  useLayoutEffect(() => {
    if (node.current) measure.keep(id, drawnHeight(node.current));
  });
  // WATCHED FROM THE COMMIT THAT DRAWS IT TO THE ONE THAT REMOVES IT, never a
  // frame longer: a passive effect let go after the row had left the page,
  // and the observer reported it in between.
  useLayoutEffect(
    () => (node.current ? measure.watch(id, node.current) : undefined),
    [id, measure],
  );
  return (
    <div ref={node} className="chat-row">
      {newAbove && <ChatDivider variant="unread" label="New" pulse={pulse} />}
      <MessageRow view={view} continuation={continuation} working={working} flash={flash} />
    </div>
  );
});
