/**
 * The read cursor's policy, against a clock.
 *
 * Every case here is about a write nobody asked for: one per message, one that
 * moves backwards, one that was lost because the tab closed, and one that
 * silently marks a room read further than the reader got.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import type { ActResult } from "~/protocol/act.ts";
import { Store } from "~/protocol/index.ts";
import { GENERATION_STRIDE } from "~/contract/positions.ts";
import { noteReader } from "~/lib/reader.ts";
import { markRead, packPosition, ReadCursors, READ_FLUSH_MS, stillOwed } from "./cursor.ts";

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

// A STOP IS NOT AN END. React tears an effect down and sets it up again over
// one component's state in development, so the writer the teardown stopped is
// the one the next setup uses — and a stop that could not be undone left the
// read cursor still for the life of the screen. A forgotten person is the
// other thing: resuming them would hand their reading on.
describe("resuming a stopped writer", () => {
  test("takes what is read again, and writes what was pending", () => {
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.stop();
    cursors.see("room-1", packPosition(at(1, 20)));
    cursors.resume();
    cursors.see("room-1", packPosition(at(1, 21)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    expect(flush).toHaveBeenCalledWith({ "room-1": packPosition(at(1, 21)) });
  });

  test("a failed flush put back while stopped is scheduled again on resuming", async () => {
    const flush = vi.fn(async (_cursors: Record<string, number>): Promise<void> => {
      throw new Error("unreachable");
    });
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 22)));
    cursors.stop();
    cursors.flushNow();
    await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    expect(flush).toHaveBeenCalledTimes(1);
    flush.mockImplementation(async () => undefined);
    cursors.resume();
    await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    expect(flush).toHaveBeenCalledTimes(2);
    expect(flush).toHaveBeenLastCalledWith({ "room-1": packPosition(at(1, 22)) });
  });

  test("never brings a forgotten person back", () => {
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.forget();
    cursors.resume();
    cursors.see("room-1", packPosition(at(1, 23)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    cursors.flushNow();
    expect(flush).not.toHaveBeenCalled();
  });
});

// A WRITER IS ONE PERSON'S. The flush goes out as whoever the tab's credential
// names when it is sent, so once the credential names somebody else, what the
// last person read can only be written as the next one — on a record that only
// moves forward, marking the next person's shared rooms read for good.
describe("forgetting a person", () => {
  test("what they read is never written, whenever the next flush would come", () => {
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 30)));
    cursors.forget();
    vi.advanceTimersByTime(READ_FLUSH_MS * 2);
    // The moments that flush at once — leaving, hiding, closing — included.
    cursors.flushNow();
    expect(flush).not.toHaveBeenCalled();
  });

  test("it takes nothing more: the last person's screen is still drawn", () => {
    // Between the credential moving and the screen redrawing, the transcript
    // on screen is still the last person's, reporting what scrolls into view.
    const flush = vi.fn(async () => undefined);
    const cursors = new ReadCursors({ flush });
    cursors.forget();
    cursors.see("room-1", packPosition(at(1, 31)));
    vi.advanceTimersByTime(READ_FLUSH_MS);
    cursors.flushNow();
    expect(flush).not.toHaveBeenCalled();
  });

  test("a flush of theirs that fails is not put back for the next person", async () => {
    // Sent with their credential and lost: retried, it would go out with the
    // next one's.
    let reject: (err: Error) => void = () => undefined;
    const flush = vi.fn(
      () =>
        new Promise<undefined>((_, fail) => {
          reject = fail;
        }),
    );
    const cursors = new ReadCursors({ flush });
    cursors.see("room-1", packPosition(at(1, 32)));
    cursors.flushNow();
    expect(flush).toHaveBeenCalledTimes(1);
    cursors.forget();
    reject(new Error("unreachable"));
    await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    cursors.flushNow();
    expect(flush).toHaveBeenCalledTimes(1);
  });
});

describe("whether a flush is still owed", () => {
  // THE FLUSH IS A PRESS LIKE ANY OTHER (`mark_chat_read`), so its outcome is
  // the act transport's, and only some of them leave the cursors to send
  // again. Owed where another try can land; let go where the same flush is
  // refused the same way for as long as the tab is open — or the timer would
  // re-send a doomed write every interval.
  const can: WriteAccess = { can: true, as: "ada", acts: ["mark_chat_read"] };
  const blocked = (block: "offline" | "loading" | "anonymous" | "not_served"): WriteAccess => ({
    can: false,
    block,
    reason: block,
  });
  const result = (kind: "applied" | "pending"): ActResult => ({
    kind,
    tool: "mark_chat_read",
    opId: "r",
    position: null,
    domain: null,
    receipt: {},
    caveats: [],
  });
  const refusal = (retryable: boolean): ActResult => ({
    kind: "refused",
    tool: "mark_chat_read",
    opId: "r",
    code: retryable ? "unavailable" : "invalid",
    sentence: "",
    detail: "",
    hint: "",
    grants: [],
    reason: "",
    retryable,
    remedy: null,
  });

  test("a flush that landed is not owed", () => {
    expect(stillOwed(result("applied"), can)).toBe(false);
    expect(stillOwed(result("pending"), can)).toBe(false);
  });

  test("one nobody can vouch for is owed: the merge only moves a cursor forward", () => {
    expect(stillOwed({ kind: "unknown", tool: "mark_chat_read", opId: "r", reason: "" }, can)).toBe(
      true,
    );
  });

  test("a refusal the node made is owed, and a refusal of the flush itself is not", () => {
    expect(stillOwed(refusal(true), can)).toBe(true);
    expect(stillOwed(refusal(false), can)).toBe(false);
  });

  test("a flush this browser could not send is owed only while that can clear", () => {
    // Offline, or still learning who it is: the next flush may go. A reader
    // the engine will never record a cursor for: it never will.
    expect(stillOwed(null, blocked("offline"))).toBe(true);
    expect(stillOwed(null, blocked("loading"))).toBe(true);
    expect(stillOwed(null, blocked("anonymous"))).toBe(false);
    expect(stillOwed(null, blocked("not_served"))).toBe(false);
  });
});

// A READ-STATE WRITE'S ANSWER IS THE SEAT'S NEW STATE, and the tab that wrote
// it applies it as it would the push — but only while the tab is still its
// reader, and only a state that says whose it is.
describe("the answer to a read-state write", () => {
  afterEach(() => {
    sessionStorage.clear();
  });

  const state = { handle: "ada", revision: 7, cursors: { "room-1": 30 }, muted: ["room-1"] };

  /** A press that answers `receipt`, after `meanwhile` has run. */
  function press(receipt: unknown, meanwhile: () => void = () => undefined) {
    return {
      access: { can: true, as: "ada", acts: ["mark_chat_read"] },
      run: vi.fn(async (): Promise<ActResult> => {
        meanwhile();
        return {
          kind: "applied",
          tool: "mark_chat_read",
          opId: "r",
          position: null,
          domain: null,
          receipt,
        } as ActResult;
      }),
    } as never;
  }

  test("is this tab's read state", async () => {
    noteReader("person-ada");
    const store = new Store();
    await markRead(press({ read: state }), store, { mute: ["room-1"] }, { done: "Muted" });
    expect(store.state.chatRead).toEqual(state);
  });

  test("is not applied once the tab's reader is somebody else", async () => {
    // Sent for the last reader and answered after the tab was handed over
    // (`lib/session.ts`, which notes the next reader before it reloads):
    // applied, it would replace the next reader's state with the last one's
    // and forget the next reader's live chat along with it.
    noteReader("person-ada");
    const store = new Store();
    store.applyChatRead({ handle: "bo", revision: 3, cursors: {} });
    await markRead(
      press({ read: state }, () => {
        sessionStorage.clear();
        noteReader("person-bo");
      }),
      store,
      { mute: ["room-1"] },
      { done: "Muted" },
    );
    expect(store.state.chatRead?.handle).toBe("bo");
  });

  test("is applied to a tab that learned its reader while it was out", async () => {
    // A tab that had recorded nobody when it sent the write, and adopted the
    // session's principal before the answer came back: the write was made as
    // them, and the tab is still theirs.
    const store = new Store();
    await markRead(
      press({ read: state }, () => noteReader("person-ada")),
      store,
      { mute: ["room-1"] },
      { done: "Muted" },
    );
    expect(store.state.chatRead).toEqual(state);
  });

  test("names its person, or is nothing", async () => {
    // A state that does not say whose it is could not be ordered against the
    // one held — the revision orders one seat's states and nothing else.
    noteReader("person-ada");
    const store = new Store();
    const { handle: _unnamed, ...nameless } = state;
    await markRead(press({ read: nameless }), store, { mute: ["room-1"] }, { done: "Muted" });
    await markRead(press({ cursors: {} }), store, { mute: ["room-1"] }, { done: "Muted" });
    expect(store.state.chatRead).toBeNull();
  });
});
