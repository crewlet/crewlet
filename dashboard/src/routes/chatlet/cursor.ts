/**
 * Where this person has read to, and the one rule that decides how often that
 * is written down.
 *
 * A read cursor is the highest-frequency write anybody makes in this product:
 * a reader scrolling a busy room passes a message every few hundred
 * milliseconds, and each flush is a compare-and-set against the coordination
 * store — the engine's own embedded broker on the default topology, shared
 * with every lease, every counter and every duty in the fleet. Writing one per
 * message would put a fleet-wide round trip behind a scroll wheel.
 *
 * So the cursor is COALESCED: a burst becomes one write, and the write carries
 * the furthest point reached rather than each point passed. Three things flush
 * at once instead of waiting — leaving a room, hiding the tab, and closing it
 * — because each is a moment where the next flush might never happen.
 *
 * AND A WRITER IS ONE SEAT'S. What it holds is how far somebody read as one
 * seat, and the flush that writes it is made as whoever the session is when it
 * goes out, under whatever seat the engine resolves them to THEN — so a reader
 * or a seat that changes under it turns the last one's reading into the next
 * one's record. A cursor only moves forward, so that would mark the next
 * seat's shared rooms read for good, silently, which is the failure this file
 * exists to prevent. A flush therefore writes only while the tab is still the
 * reader and the seat it was made for ([writerFor]), and anything else lets
 * the reading go unwritten — a person signing in hands the tab over by a
 * reload (`lib/session.ts`), and the reload fires the `pagehide` this writer
 * flushes on; a seat that moves remounts the screen (`Chat.tsx`), whose
 * cleanup flushes too.
 */

import { useEffect, useRef, useState } from "react";

import { GENERATION_STRIDE } from "~/contract/positions.ts";
import { currentReader } from "~/lib/reader.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useAct, type Act, type PressLabel } from "~/lib/useAct.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import type { ActionArgs, ActResult } from "~/protocol/act.ts";
import {
  type ChatMessageView,
  type ChatReadReceipt,
  type ChatReadState,
  type LogPosition,
  type Store,
} from "~/protocol/index.ts";

/**
 * How long a burst of reading is allowed to accumulate before it is written.
 *
 * FIFTEEN SECONDS, which is `coord.ReconcileInterval` — the cadence this
 * engine already refreshes a liveness fact in coordination at, and therefore
 * the rate the fleet's own store is sized for. It IS how long a badge waits:
 * the rail's unread counts are the engine's, counted against the record this
 * writes, so they move when a flush lands — the flush's own answer is the
 * seat's new read state ([markRead]), and the rail asks again once its
 * revision moves — and leaving the room, which is when a reader looks back at
 * the rail, flushes at once. At the design's 20,000
 * messages a day it turns a scroll through a hundred messages into one
 * compare-and-set instead of a hundred.
 *
 * Shorter buys nothing anybody can see and multiplies the write rate; longer
 * starts to lose real reading to a closed lid, which is what the immediate
 * flushes below are for.
 */
export const READ_FLUSH_MS = 15_000;

/**
 * A position as the one integer a cursor is — `(generation << 40) | seq`, by
 * the engine's own stride ([GENERATION_STRIDE]) — or null when this browser
 * cannot hold it exactly.
 *
 * REFUSED RATHER THAN ROUNDED, and the direction of the error is why. A
 * JavaScript number is exact to 2^53, and the packed form runs to 2^63 — so a
 * generation past 8,191 (a fleet that has reanchored its chat log that many
 * times) would round, and a rounded cursor can land ABOVE the message it was
 * taken from. That marks unread messages read for ever, silently, on a record
 * that only ever moves forward. A cursor that is not written is a badge that
 * stays up, which is the failure a person can see and fix.
 */
export function packPosition(at: LogPosition | null | undefined): number | null {
  if (!at) return null;
  const packed = at.generation * GENERATION_STRIDE + at.seq;
  if (!Number.isSafeInteger(packed) || packed < 0) return null;
  return packed;
}

/** The furthest point a page of messages reaches, or null for an empty one. */
export function furthest(messages: readonly ChatMessageView[]): number | null {
  let best: number | null = null;
  for (const view of messages) {
    const packed = packPosition(view.position);
    if (packed !== null && (best === null || packed > best)) best = packed;
  }
  return best;
}

export interface ReadCursorOptions {
  /**
   * Write these cursors. Rejecting is expected — the engine may be
   * unreachable — and a rejection puts them back, so nothing is lost by a
   * flush that failed.
   */
  flush: (cursors: Record<string, number>, threads: Record<string, number>) => Promise<unknown>;
  /** Overridable for the suite only; every caller takes [READ_FLUSH_MS]. */
  delay?: number;
}

/**
 * The read cursor for however many rooms a tab passes through.
 *
 * A CLASS RATHER THAN A HOOK, so the policy can be exercised against a clock
 * instead of against a rendered screen: what has to be right here is a
 * question about timers and arithmetic, and a test that had to mount a
 * transcript to ask it would be testing the transcript.
 */
export class ReadCursors {
  private readonly opts: ReadCursorOptions;
  /** Read and not yet written. */
  private pending: Record<string, number> = {};
  /**
   * The furthest point this tab has accepted for each room, written or not.
   *
   * SEPARATE FROM `pending`, and not derived from whether a write succeeded:
   * the advance-only rule is about what the person has READ, which is true the
   * moment they read it. A map that recorded only what the engine had
   * acknowledged would re-offer every message again for as long as one flush
   * was in flight.
   */
  private highest: Record<string, number> = {};
  /**
   * The same two maps for a THREAD'S own place, keyed by its root: a thread
   * pane reads replies the room's cursor does not reach, and the badge on a
   * thread line is counted against the furthest of the two. One writer for
   * both, so a reader moving between a room and its thread makes one flush.
   */
  private pendingThreads: Record<string, number> = {};
  private highestThreads: Record<string, number> = {};
  private timer: ReturnType<typeof setTimeout> | 0 = 0;
  private stopped = false;
  /** The seat this writer read for is gone ([forget]). */
  private forgotten = false;

  constructor(opts: ReadCursorOptions) {
    this.opts = opts;
  }

  /**
   * This person has now read as far as `packed` in this room.
   *
   * ADVANCE-ONLY, here as well as in the engine. The server ignores a value at
   * or below what it holds — two tabs racing produce the same record whichever
   * lands first — and a client that sent one anyway would be spending a
   * fleet-wide write to say nothing. Scrolling BACK through history is exactly
   * that case, and it is the common one.
   */
  see(channelID: string, packed: number | null): void {
    if (this.stopped || !channelID || packed === null) return;
    if (packed <= (this.highest[channelID] ?? 0)) return;
    this.highest[channelID] = packed;
    this.pending[channelID] = packed;
    this.schedule();
  }

  /**
   * This person has now read a thread as far as `packed`: the newest reply a
   * thread pane has had on screen. Advance-only, as [see] is, and written in
   * the same flush.
   */
  seeThread(rootID: string, packed: number | null): void {
    if (this.stopped || !rootID || packed === null) return;
    if (packed <= (this.highestThreads[rootID] ?? 0)) return;
    this.highestThreads[rootID] = packed;
    this.pendingThreads[rootID] = packed;
    this.schedule();
  }

  /**
   * Write what is pending now.
   *
   * For the three moments where waiting out the interval risks losing the
   * reading entirely: the reader left the room, hid the tab, or closed it.
   */
  flushNow(): void {
    this.clearTimer();
    void this.write();
  }

  /** Stop scheduling. A final write is the caller's own `flushNow`, because
   *  unmounting and going away are not the same event. */
  stop(): void {
    this.stopped = true;
    this.clearTimer();
  }

  /**
   * Take positions again after a [stop] — never after [forget], whose seat
   * has gone for good.
   *
   * FOR A WRITER IN USE AGAIN: React runs an effect's setup, cleanup and setup
   * again over one component's state in development (`StrictMode`, which
   * `main.tsx` renders under), so the cleanup that stops a writer is followed
   * by a setup that keeps using it. Stopped for good there, every later [see]
   * returned early and the read cursor never moved under `npm run dev`.
   */
  resume(): void {
    if (this.forgotten) return;
    this.stopped = false;
    if (Object.keys(this.pending).length + Object.keys(this.pendingThreads).length > 0) {
      this.schedule();
    }
  }

  /**
   * The seat this writer read for is gone: the tab now reads as another, or
   * is somebody else's. Drop what was read WITHOUT WRITING IT, and take
   * nothing more.
   *
   * NOT A FLUSH, because there is nobody left to write it as. The engine
   * resolves the seat when a press arrives, so every write from here on is
   * the NEXT seat's — and its record only moves forward, so the last one's
   * reading would mark the next one's shared rooms read for good. Unwritten,
   * it is a badge shown again, which is the failure a person can see and fix.
   *
   * FINAL, rather than a reset the next seat reads on through: a writer that
   * went on taking positions from the last seat's transcript, still on
   * screen until the remount, would file those under the next one too. A
   * flush already sent is the last seat's, sent while it was the tab's — and
   * one that FAILS is not put back, since putting it back is exactly the
   * write this refuses. [writerFor] calls it from a flush that finds the tab
   * moved; nothing else needs to.
   */
  forget(): void {
    this.forgotten = true;
    this.stop();
    this.pending = {};
    this.highest = {};
    this.pendingThreads = {};
    this.highestThreads = {};
  }

  private schedule(): void {
    // A TRAILING EDGE, AND ONLY ONE TIMER. Re-arming on every message would
    // make a person who reads continuously never flush at all, which is the
    // opposite failure and the harder one to notice: the cursor would move
    // only when they stopped.
    if (this.timer) return;
    this.timer = setTimeout(() => {
      this.timer = 0;
      void this.write();
    }, this.opts.delay ?? READ_FLUSH_MS);
  }

  private clearTimer(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = 0;
  }

  private async write(): Promise<void> {
    const cursors = this.pending;
    const threads = this.pendingThreads;
    if (Object.keys(cursors).length === 0 && Object.keys(threads).length === 0) return;
    this.pending = {};
    this.pendingThreads = {};
    try {
      await this.opts.flush(cursors, threads);
    } catch {
      // PUT BACK, NEVER DROPPED. The engine being unreachable is not the
      // reader having un-read the room, and the next flush — or the one the
      // tab makes on its way out — carries it. Only where nothing newer has
      // since been read, which is what keeps this from moving a cursor
      // backwards on the way in. And never for a seat that has since gone
      // ([forget]): the next flush would be somebody else's.
      if (this.forgotten) return;
      for (const [id, packed] of Object.entries(cursors)) {
        if (packed > (this.pending[id] ?? 0)) this.pending[id] = packed;
      }
      for (const [id, packed] of Object.entries(threads)) {
        if (packed > (this.pendingThreads[id] ?? 0)) this.pendingThreads[id] = packed;
      }
      if (!this.stopped) this.schedule();
    }
  }
}

/**
 * Whether a flush that came back with `result` is still owed: true puts its
 * cursors back for the next flush, false lets them go.
 *
 * OWED WHERE ANOTHER TRY CAN LAND: an outcome nobody can vouch for (the merge
 * is idempotent and a cursor only moves forward, so sending it again is
 * safe), a refusal the NODE made (draining, or unable to answer just now), and
 * a press this browser never sent because it could not act for a moment —
 * offline, or still learning who it is. NOT OWED where the same flush is
 * refused the same way however often it is sent: a refusal of the request
 * itself, or a reader the engine will never record a cursor for (nobody
 * signed in, a credential acting as no seat, an engine that does not serve
 * the tool). Putting those
 * back would re-send a doomed write every interval for as long as the tab is
 * open.
 */
export function stillOwed(result: ActResult | null, access: WriteAccess): boolean {
  if (result === null)
    return !access.can && (access.block === "offline" || access.block === "loading");
  switch (result.kind) {
    case "applied":
    case "pending":
      return false;
    case "unknown":
      return true;
    case "refused":
      return result.retryable;
  }
}

/**
 * One read cursor for the seat the tab reads chat as, with the two moments a
 * browser gives us to flush before it is too late.
 *
 * THE FLUSH IS A CHANGE LIKE ANY OTHER, made through the act transport as the
 * session's principal (`mark_chat_read`, ADR-0024) — one write path, audited
 * as every other is. QUIET, because a cursor written in the background is
 * nothing a person pressed: no toast, and its outcome only decides whether
 * the cursors go back.
 *
 * `visibilitychange` rather than window blur: blur fires for a click into
 * another window that never hid this one, which is several writes a minute for
 * somebody switching between chat and their editor, while `hidden` fires only
 * when the tab actually went away — the case where the next interval may never
 * arrive. `pagehide` is the other end of the same thought and is the one event
 * a closing tab reliably gets.
 *
 * `seat` is the seat the tab reads as NOW (`Chat.tsx` writes it in the render
 * that moves it); the writer is made for the one it holds at the mount, and
 * the screen is remounted under each, so one writer is only ever one seat's.
 */
export function useReadCursors(seat: { readonly current: string }): ReadCursors {
  const mark = useAct("mark_chat_read");
  const { store } = useClient();
  // THE LATEST PRESS AND ACCESS, read when a flush fires rather than when the
  // writer was made: the writer outlives every render, and a flush that read
  // the access of the first one would think a tab that has since reconnected
  // was still offline.
  const latest = useRef(mark);
  latest.current = mark;
  const [held] = useState(() => writerFor(latest, store, currentReader(), seat));

  useEffect(() => {
    // TAKING POSITIONS FROM HERE ON, which the cleanup below stopped if this
    // is the same writer set up again ([ReadCursors.resume]).
    held.resume();
    const away = () => {
      if (document.visibilityState === "hidden") held.flushNow();
    };
    const leaving = () => held.flushNow();
    document.addEventListener("visibilitychange", away);
    window.addEventListener("pagehide", leaving);
    return () => {
      document.removeEventListener("visibilitychange", away);
      window.removeEventListener("pagehide", leaving);
      // THE LAST FLUSH IS EXPLICIT. Unmounting the screen is a reader leaving
      // chat, which is exactly the case the interval cannot cover — and a
      // remount for another seat, which the flush's own guard lets go.
      held.flushNow();
      held.stop();
    };
  }, [held]);

  return held;
}

/**
 * A writer whose flush is a quiet `mark_chat_read` press, through `latest`,
 * for the tab's `reader` reading as the seat `seat` holds now.
 *
 * A FLUSH WHOSE READER OR SEAT HAS GONE WRITES NOTHING, and the writer is done
 * ([ReadCursors.forget]). The press goes out as whoever the session is when
 * it is sent, under the seat the engine resolves them to then — what this
 * writer holds is the last one's reading, and there is nobody left to write
 * it as: dropped rather than put back for the next.
 *
 * THE TAB'S READER IS `lib/reader.ts`'s, which a hand-over writes before its
 * reload: `sessionStorage` is emptied and the new reader recorded, and only
 * then does the page go — firing the `pagehide` this flushes on, a flush that
 * would otherwise post the last reader's reading under the new cookie. A
 * sign-out empties the storage before its reload too. With storage refused
 * the reader is always null and this guard holds nothing, which is the limit
 * a hand-over has there as well.
 *
 * NOBODY YET IS NOT SOMEBODY ELSE. A tab opened with a session already in the
 * browser records its reader only when the frame's session read lands
 * (`lib/session.ts`'s `takeSession`), and the frame can draw this screen
 * before that, on the viewer's answer over the socket — so the writer is made
 * for nobody. The first reader the tab learns is then the one it ADOPTS, by
 * the frame's own rule (a tab nobody recorded takes the session it finds),
 * and every press it sent before was made as them; that reader is this
 * writer's from then on. Compared with nobody instead, the first flush found
 * the tab "moved" and the writer forgot itself, so nothing that tab read was
 * ever written.
 */
function writerFor(
  latest: { readonly current: Act<"mark_chat_read"> },
  store: Store,
  reader: string | null,
  seat: { readonly current: string },
): ReadCursors {
  const madeFor = seat.current;
  let readBy = reader;
  const writer: ReadCursors = new ReadCursors({
    flush: async (values, threads) => {
      readBy ??= currentReader();
      if (currentReader() !== readBy || seat.current !== madeFor) {
        writer.forget();
        return;
      }
      const result = await markRead(
        latest.current,
        store,
        {
          ...(Object.keys(values).length > 0 ? { cursors: values } : {}),
          ...(Object.keys(threads).length > 0 ? { thread_cursors: threads } : {}),
        },
        { done: "Read", quiet: true },
      );
      if (stillOwed(result, latest.current.access)) {
        throw new Error("the read cursor is still owed");
      }
    },
  });
  return writer;
}

/**
 * Make a read-state write as the session's principal, and hand the tab the
 * state it answered with — the cursor writer's flush, and a room's Mute.
 *
 * THE ANSWER IS THE SEAT'S NEW STATE, NAMED BY ITS SEAT
 * ([ChatReadReceipt]) — the form the `chat_cursor_moved` push and the rail's
 * `read` take — so it goes through the store's one ordering
 * (`Store.applyChatRead`). The push the same write raises reaches this tab as
 * well, and the record's revision makes the two one state: whichever lands
 * first moves it, the other moves nothing, and the rail asks again once. The
 * answer used to be thrown away and the press asked the rail again by itself,
 * so a flush whose push won the race re-read the rail twice — the second ask
 * discarding the first after the engine had counted every room's unread.
 *
 * ONLY WHILE THE TAB IS STILL ITS READER. The write goes out as whoever the
 * session is, and its answer can arrive after the tab was handed to somebody
 * else: applied then, it would replace the next reader's state with the last
 * one's, and forget the next reader's live chat along with it
 * (`Store.forgetSeat`). A tab that had recorded nobody when the write went
 * out and has learned its reader since is still that reader's, by
 * [writerFor]'s adoption rule. A seat that moved needs no check here — the
 * answer names its seat, and the store replaces a state naming another rather
 * than ordering the two.
 */
export async function markRead(
  mark: Act<"mark_chat_read">,
  store: Store,
  args: ActionArgs<"mark_chat_read">,
  label: PressLabel,
): Promise<ActResult | null> {
  // READ IN THE SAME TURN THE PRESS IS SENT, so it is the reader the write
  // went out for.
  const sentAs = currentReader();
  const result = await mark.run(args, label);
  if (result && (result.kind === "applied" || result.kind === "pending")) {
    const read = readOf(result.receipt);
    if (read && (sentAs === null || currentReader() === sentAs)) store.applyChatRead(read);
  }
  return result;
}

/**
 * The read state a `mark_chat_read` receipt carries, or null for one that
 * carries none this build can tell is somebody's — which is applied as
 * nothing rather than as a nameless state the store could not order.
 */
function readOf(receipt: unknown): ChatReadState | null {
  const read = (receipt as Partial<ChatReadReceipt> | null)?.read;
  return read && typeof read.handle === "string" && read.handle !== "" ? read : null;
}
