/**
 * The relay route against a scripted socket: what it sends, how each engine
 * message lands on the kit's transport contract, and that a refusal before
 * the first word hands over while one after it ends the session.
 */
import { describe, expect, test, vi } from "vitest";
import { afterMicrotasks, eventually } from "~/test/settle.ts";
import type { DictationErrorCode, DictationTransportEvents } from "@crewlethq/ui";
import { createRelay, type PcmCapture, type RelaySocket } from "./relay.ts";

class FakeSocket implements RelaySocket {
  binaryType: BinaryType = "blob";
  readyState = 0;
  sent: (string | ArrayBuffer)[] = [];
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closes = 0;

  constructor(readonly url: string) {}

  open(): void {
    this.readyState = 1;
    this.onopen?.(new Event("open"));
  }
  say(message: object): void {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(message) }));
  }
  drop(code: number): void {
    this.readyState = 3;
    this.onclose?.({ code } as CloseEvent);
  }
  send(data: string | ArrayBuffer): void {
    if (this.readyState !== 1) throw new Error("sent on a socket that is not open");
    this.sent.push(data);
  }
  close(): void {
    this.closes++;
    if (this.readyState === 3) return;
    this.readyState = 2;
    queueMicrotask(() => this.drop(1000));
  }
  /** The control messages sent, by type. */
  controls(): string[] {
    return this.sent
      .filter((m): m is string => typeof m === "string")
      .map((m) => JSON.parse(m).type);
  }
  frames(): number {
    return this.sent.filter((m) => typeof m !== "string").length;
  }
}

interface Harness {
  socket: () => FakeSocket;
  events: DictationTransportEvents & { errors: DictationErrorCode[]; log: string[] };
  frame: (bytes?: number) => void;
  capture: PcmCapture & { stopped: number; switched: MediaStream[] };
  /** The relay has asked for the capture. */
  opened: () => boolean;
  relay: ReturnType<typeof createRelay>;
}

const STREAM = {} as MediaStream;

function harness(available = true): Harness {
  let socket: FakeSocket | null = null;
  let onFrame: ((pcm: ArrayBuffer) => void) | null = null;
  const capture = {
    stopped: 0,
    switched: [] as MediaStream[],
    switchStream(stream: MediaStream) {
      capture.switched.push(stream);
    },
    stop() {
      capture.stopped++;
    },
  };
  const errors: DictationErrorCode[] = [];
  const log: string[] = [];
  const events = {
    errors,
    log,
    partial: (text: string) => log.push(`partial:${text}`),
    final: (text: string) => log.push(`final:${text}`),
    error: (code: DictationErrorCode) => errors.push(code),
    preparing: (progress?: number) => log.push(`preparing:${progress}`),
    ready: () => log.push("ready"),
  };
  const relay = createRelay({
    available: () => available,
    socket: () => (socket = new FakeSocket("wss://crewlet.example.com/ws/dictation?reader=r1")),
    capture: async (_stream, frame) => {
      onFrame = frame;
      return capture;
    },
  });
  return {
    socket: () => socket!,
    events,
    frame: (bytes = 1280) => onFrame?.(new ArrayBuffer(bytes)),
    capture,
    opened: () => onFrame !== null,
    relay,
  };
}

/** Starts a session and opens its socket; answers the start's promise, unsettled. */
async function started(h: Harness): Promise<{ settled: Promise<void> }> {
  const settled = Promise.resolve(h.relay.start(undefined, h.events, STREAM));
  h.socket().open();
  await eventually(() => expect(h.opened()).toBe(true));
  return { settled };
}

describe("a session", () => {
  test("says start in the engine's format, then streams the microphone's frames", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    h.frame();
    h.frame();
    expect(h.socket().binaryType).toBe("arraybuffer");
    expect(JSON.parse(h.socket().sent[0] as string)).toEqual({
      type: "start",
      lang: "en",
      rate: 16000,
      encoding: "pcm16",
    });
    expect(h.socket().frames()).toBe(2);
    expect(h.events.log).toEqual(["ready"]);
  });

  test("lands partials and finals as the kit's words", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    h.socket().say({ type: "partial", seq: 0, text: "ship the" });
    h.socket().say({ type: "final", seq: 0, text: "Ship the fix." });
    h.socket().say({ type: "partial", seq: 1, text: "then" });
    expect(h.events.log).toEqual([
      "ready",
      "partial:ship the",
      "final:Ship the fix.",
      "partial:then",
    ]);
    expect(h.events.errors).toEqual([]);
  });

  test("preparing settles the start; audio before ready stops at 8 s, inside the engine's hold", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.frame();
    h.socket().say({ type: "preparing", progress: 0.25 });
    await start;
    // 40 ms frames: 200 of them are 8 s.
    for (let i = 0; i < 260; i++) h.frame();
    expect(h.socket().frames()).toBe(200);
    h.socket().say({ type: "preparing", progress: 0.75 });
    h.socket().say({ type: "ready" });
    h.frame();
    expect(h.socket().frames()).toBe(201);
    expect(h.events.log).toEqual(["preparing:0.25", "preparing:0.75", "ready"]);
  });

  test("switches the microphone without a new socket", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    const next = {} as MediaStream;
    h.relay.switchStream?.(next, "usb");
    expect(h.capture.switched).toEqual([next]);
  });

  test("has nothing to send without the kit's open microphone", async () => {
    const h = harness();
    await expect(Promise.resolve(h.relay.start(undefined, h.events, null))).rejects.toThrow();
  });

  test("is available as the engine says", () => {
    expect(harness(true).relay.available?.()).toBe(true);
    expect(harness(false).relay.available?.()).toBe(false);
  });
});

describe("before the first word, a refusal hands over", () => {
  test.each([
    ["busy", { type: "busy", reason: "capacity" }, "busy"],
    ["an error", { type: "error", code: "dictation_unavailable" }, "unavailable"],
    ["a preparation that ran out", { type: "done", reason: "preparing" }, "unavailable"],
  ])("%s", async (_name, message, code) => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say(message);
    await start;
    expect(h.events.errors).toEqual([code]);
    expect(h.capture.stopped).toBe(1);
    expect(h.socket().closes).toBe(1);
  });

  test("a refused handshake is unavailable", async () => {
    const h = harness();
    const start = Promise.resolve(h.relay.start(undefined, h.events, STREAM));
    h.socket().onerror?.(new Event("error"));
    h.socket().drop(1006);
    await start;
    expect(h.events.errors).toEqual(["unavailable"]);
  });

  test("busy while preparing is still before the first word", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "preparing" });
    await start;
    h.socket().say({ type: "busy", reason: "capacity" });
    expect(h.events.errors).toEqual(["busy"]);
  });
});

describe("mid-session, the engine's end says why capture stopped", () => {
  test.each([
    ["the length limit", { type: "done", reason: "length" }, "length"],
    ["the idle cut", { type: "done", reason: "idle" }, "silence"],
    ["an error", { type: "error", code: "dictation_unavailable" }, "dropped"],
    ["busy", { type: "busy", reason: "capacity" }, "dropped"],
  ])("%s", async (_name, message, code) => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    h.socket().say(message);
    expect(h.events.errors).toEqual([code]);
    expect(h.capture.stopped).toBe(1);
  });

  test.each([
    [4401, "refused"],
    [4403, "refused"],
    [1006, "dropped"],
  ])("a close %i is %s, said once", async (close, code) => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "partial", seq: 0, text: "hello" });
    await start;
    h.socket().drop(close);
    h.socket().drop(close);
    expect(h.events.errors).toEqual([code]);
  });
});

describe("the end the person asks for", () => {
  test("flush stops the audio, asks for the last words and settles on done", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    let flushed = false;
    const flush = h.relay.flush().then(() => {
      flushed = true;
    });
    expect(h.capture.stopped).toBe(1);
    expect(h.socket().controls()).toEqual(["start", "finish"]);
    h.socket().say({ type: "final", seq: 0, text: "Done." });
    await afterMicrotasks();
    expect(flushed).toBe(false);
    h.socket().say({ type: "done", reason: "finish" });
    await flush;
    expect(h.events.log).toEqual(["ready", "final:Done."]);
    expect(h.events.errors).toEqual([]);
    expect(h.socket().closes).toBe(1);
  });

  test("flush settles on a close too, and says nothing of it", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    const flush = h.relay.flush();
    h.socket().drop(1006);
    await flush;
    expect(h.events.errors).toEqual([]);
  });

  test("cancel tells the engine, closes, and nothing follows", async () => {
    const h = harness();
    const { settled: start } = await started(h);
    h.socket().say({ type: "ready" });
    await start;
    h.relay.cancel();
    expect(h.socket().controls()).toEqual(["start", "cancel"]);
    expect(h.capture.stopped).toBe(1);
    h.socket().say({ type: "partial", seq: 0, text: "late" });
    h.socket().drop(1006);
    expect(h.events.log).toEqual(["ready"]);
    expect(h.events.errors).toEqual([]);
  });

  test("a cancel before the capture opened releases it when it does", async () => {
    let release: (capture: PcmCapture) => void = () => {};
    const stop = vi.fn();
    let socket: FakeSocket | null = null;
    const relay = createRelay({
      available: () => true,
      socket: () => (socket = new FakeSocket("wss://crewlet.example.com/ws/dictation")),
      capture: () =>
        new Promise<PcmCapture>((resolve) => {
          release = resolve;
        }),
    });
    const h = harness();
    void relay.start(undefined, h.events, STREAM);
    socket!.open();
    relay.cancel();
    release({ switchStream: () => {}, stop });
    await eventually(() => expect(stop).toHaveBeenCalledTimes(1));
  });
});
