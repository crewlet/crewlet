/**
 * The read cursor's policy, against a clock.
 *
 * Every case here is about a write nobody asked for: one per message, one that
 * moves backwards, one that was lost because the tab closed, and one that
 * silently marks a room read further than the reader got.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { GENERATION_STRIDE, packPosition, ReadCursors, READ_FLUSH_MS } from "./cursor.ts";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

const at = (generation: number, seq: number) => ({
  stream: "CREWLET_CHAT_LOG",
  generation,
  seq,
});

describe("packing a position", () => {
  test("a cursor is the packed position, generation first", () => {
    // The coordination record holds one integer per room and only ever
    // compares it, so the ordering has to survive the packing: a sequence
    // from an older generation is below every sequence in a newer one however
    // large it is.
    expect(packPosition(at(0, 7))).toBe(7);
    expect(packPosition(at(1, 0))).toBe(GENERATION_STRIDE);
    expect(packPosition(at(1, 2))).toBeGreaterThan(packPosition(at(0, 2 ** 39))!);
  });

  test("a position this browser cannot hold exactly is refused, not rounded", () => {
    // A rounded cursor can land ABOVE the message it was taken from, which
    // marks unread messages read for ever on a record that only moves
    // forward. Refusing leaves the badge up — a failure somebody can see.
    expect(packPosition(at(1 << 20, 5))).toBeNull();
    expect(packPosition(null)).toBeNull();
  });
});

describe("flushing", () => {
  test("a burst of reading is one write, carrying the furthest point", () => {
    // The whole reason this class exists: a reader passing a message every
    // few hundred milliseconds would otherwise put one fleet-wide
    // compare-and-set behind a scroll wheel.
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    for (let seq = 1; seq <= 40; seq++) cursors.see("room-1", packPosition(at(1, seq)));
    expect(flush).not.toHaveBeenCalled();

    vi.advanceTimersByTime(READ_FLUSH_MS);
    expect(flush).toHaveBeenCalledTimes(1);
    expect(flush).toHaveBeenCalledWith({ "room-1": packPosition(at(1, 40)) });
  });

  test("a reader who never stops is still written down, once per interval", () => {
    // THE WINDOW IS A THROTTLE, NOT A QUIET PERIOD, and the difference is the
    // whole behaviour: re-armed on every message, somebody reading steadily
    // for ten minutes would have their cursor written when they STOPPED and
    // not before — so closing the lid mid-room loses the lot, which is the
    // one thing the immediate flushes cannot cover because nothing fires.
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    // One message every half second for three intervals' worth of reading.
    for (let tick = 1; tick <= (READ_FLUSH_MS / 500) * 3; tick++) {
      cursors.see("room-1", packPosition(at(1, tick)));
      vi.advanceTimersByTime(500);
    }
    expect(flush).toHaveBeenCalledTimes(3);
    // And each write carried where the reader had actually reached.
    expect(flush).toHaveBeenLastCalledWith({ "room-1": packPosition(at(1, 90)) });
  });

  test("reading back through history never moves the cursor down", () => {
    // Scrolling back is the common case, and every message passed on the way
    // is one the person has already read. A client that sent them would spend
    // a fleet-wide write to say nothing.
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 90)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    flush.mockClear();

    for (let seq = 89; seq > 0; seq--) cursors.see("room-1", packPosition(at(1, seq)));
    vi.advanceTimersByTime(READ_FLUSH_MS * 3);
    expect(flush).not.toHaveBeenCalled();
  });

  test("leaving a room writes at once rather than waiting out the interval", () => {
    // A room switch, a hidden tab and a closing page are the three moments
    // where the next flush may never happen.
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 12)));
    cursors.flushNow();
    expect(flush).toHaveBeenCalledWith({ "room-1": packPosition(at(1, 12)) });

    // And the interval that was already running does not then write again.
    flush.mockClear();
    vi.advanceTimersByTime(READ_FLUSH_MS * 2);
    expect(flush).not.toHaveBeenCalled();
  });

  test("two rooms read in one interval are one write", () => {
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 3)));
    cursors.see("room-2", packPosition(at(1, 9)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    expect(flush).toHaveBeenCalledTimes(1);
    expect(flush).toHaveBeenCalledWith({
      "room-1": packPosition(at(1, 3)),
      "room-2": packPosition(at(1, 9)),
    });
  });

  test("a flush that failed is retried rather than lost", async () => {
    // An unreachable engine is not the reader having un-read the room.
    let fail = true;
    const flush = vi.fn(async () => {
      if (fail) throw new Error("unreachable");
      return undefined;
    });
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 5)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    await vi.waitFor(() => expect(flush).toHaveBeenCalledTimes(1));

    fail = false;
    await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    expect(flush).toHaveBeenCalledTimes(2);
    expect(flush).toHaveBeenLastCalledWith({ "room-1": packPosition(at(1, 5)) });
  });

  test("a room already written is not written again", () => {
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 5)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    flush.mockClear();

    cursors.see("room-1", packPosition(at(1, 5)));
    cursors.flushNow();
    expect(flush).not.toHaveBeenCalled();
  });
});
