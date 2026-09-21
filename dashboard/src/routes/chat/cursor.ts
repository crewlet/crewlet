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
 */

import { useEffect, useRef } from "react";

import type { ChatMessageView, LogPosition } from "~/protocol/index.ts";

import { flushRead } from "./writes.ts";

/**
 * How long a burst of reading is allowed to accumulate before it is written.
 *
 * FIFTEEN SECONDS, which is `coord.ReconcileInterval` — the cadence this
 * engine already refreshes a liveness fact in coordination at, and therefore
 * the rate the fleet's own store is sized for. It is not a latency budget: the
 * badge on the screen that did the reading clears the moment it is read, and
 * this is what the person's OTHER tabs and their next page load see. At the
 * design's 20,000 messages a day it turns a scroll through a hundred messages
 * into one compare-and-set instead of a hundred.
 *
 * Shorter buys nothing anybody can see and multiplies the write rate; longer
 * starts to lose real reading to a closed lid, which is what the immediate
 * flushes below are for.
 */
export const READ_FLUSH_MS = 15_000;

/**
 * The packed form's generation stride — `statelog.GenerationStride`.
 *
 * A position packs as `(generation << 40) | seq`, and that is what a cursor
 * stores: the coordination record holds one integer per room and only ever
 * COMPARES it, so the client has to pack rather than send a triple.
 */
export const GENERATION_STRIDE = 2 ** 40;

/**
 * A position as the one integer a cursor is, or null when this browser cannot
 * hold it exactly.
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
  flush: (cursors: Record<string, number>) => Promise<unknown>;
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
  private timer: ReturnType<typeof setTimeout> | 0 = 0;
  private stopped = false;

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

  /** What is written but not yet acknowledged — for the screen's own badge
   *  arithmetic, which must not wait for a round trip. */
  reached(channelID: string): number {
    return this.highest[channelID] ?? 0;
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
    if (Object.keys(cursors).length === 0) return;
    this.pending = {};
    try {
      await this.opts.flush(cursors);
    } catch {
      // PUT BACK, NEVER DROPPED. The engine being unreachable is not the
      // reader having un-read the room, and the next flush — or the one the
      // tab makes on its way out — carries it. Only where nothing newer has
      // since been read, which is what keeps this from moving a cursor
      // backwards on the way in.
      for (const [id, packed] of Object.entries(cursors)) {
        if (packed > (this.pending[id] ?? 0)) this.pending[id] = packed;
      }
      if (!this.stopped) this.schedule();
    }
  }
}

/**
 * One read cursor for the tab, with the two moments a browser gives us to
 * flush before it is too late.
 *
 * `visibilitychange` rather than window blur: blur fires for a click into
 * another window that never hid this one, which is several writes a minute for
 * somebody switching between chat and their editor, while `hidden` fires only
 * when the tab actually went away — the case where the next interval may never
 * arrive. `pagehide` is the other end of the same thought and is the one event
 * a closing tab reliably gets.
 */
export function useReadCursors(): ReadCursors {
  const cursors = useRef<ReadCursors | null>(null);
  if (cursors.current === null) {
    cursors.current = new ReadCursors({ flush: (values) => flushRead({ cursors: values }) });
  }
  const held = cursors.current;

  useEffect(() => {
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
      // chat, which is exactly the case the interval cannot cover.
      held.flushNow();
      held.stop();
    };
  }, [held]);

  return held;
}
