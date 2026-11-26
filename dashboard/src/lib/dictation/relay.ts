/**
 * The engine relay as a dictation route: the microphone's PCM to
 * `/ws/dictation`, the recognizer's words back, on the kit's transport
 * contract. Busy or unavailable is answered before `start` settles, so the
 * kit hands over to the browser's own route; nothing mid-session does.
 */
import type {
  DictationErrorCode,
  DictationTransport,
  DictationTransportEvents,
} from "@crewlethq/ui";
import { CLOSE_FORBIDDEN, CLOSE_UNAUTHENTICATED } from "~/contract/closecodes.ts";
import type { DictationDoneReason } from "~/contract/dictation.ts";
import { PCM_RATE } from "./pcm.ts";

/** An open source of PCM frames, from `audio.ts` or a test's double. */
export interface PcmCapture {
  switchStream(stream: MediaStream): void;
  stop(): void;
}

/** The socket as the relay uses it, so a test can stand in for it. */
export interface RelaySocket {
  binaryType: BinaryType;
  readonly readyState: number;
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
  send(data: string | ArrayBuffer): void;
  close(code?: number): void;
}

export interface RelayDeps {
  /** Whether the engine says it can dictate now (the viewer's `dictation`). */
  available(): boolean;
  /** Opens the socket (`protocol/dictation.ts`). */
  socket(): RelaySocket;
  capture(stream: MediaStream, onFrame: (pcm: ArrayBuffer) => void): Promise<PcmCapture>;
}

/** The engine's message, with the fields its type uses. */
interface Message {
  type: "preparing" | "ready" | "partial" | "final" | "done" | "busy" | "error";
  progress?: number;
  text?: string;
  reason?: string;
}

const OPEN = 1;

/**
 * Audio sent before `ready`, at most: the engine holds 10 s of it, so 8 s
 * keeps the first words of a warm start and never overflows a long one.
 */
const BEFORE_READY_BYTES = 8 * 2 * PCM_RATE;

/** A `done` the person did not ask for: why capture stopped. */
const DONE_STOPS: Partial<Record<DictationDoneReason, DictationErrorCode>> = {
  length: "length",
  idle: "silence",
  preparing: "dropped",
};

/** One session's socket, capture and state. */
class Session {
  /** Before the engine's first word or `ready`: a refusal here hands over. */
  private opening = true;
  /** The engine said `ready`; before it, audio is held there up to [BEFORE_READY_BYTES]. */
  private ready = false;
  private early = 0;
  private ended = false;
  private muted = false;
  private capture: PcmCapture | null = null;
  private settle: (() => void) | null = null;
  private flushed: (() => void) | null = null;

  constructor(
    private readonly deps: RelayDeps,
    private readonly events: DictationTransportEvents,
    private readonly stream: MediaStream,
    private readonly socket: RelaySocket,
  ) {}

  /** Settles at the engine's first answer: preparing, ready, busy or a refusal. */
  open(): Promise<void> {
    const settled = new Promise<void>((resolve) => {
      this.settle = resolve;
    });
    const s = this.socket;
    s.binaryType = "arraybuffer";
    s.onopen = () => {
      s.send(JSON.stringify({ type: "start", lang: "en", rate: PCM_RATE, encoding: "pcm16" }));
      void this.startCapture();
    };
    s.onmessage = (event) => {
      if (typeof event.data === "string") this.receive(event.data);
    };
    s.onerror = () => undefined;
    s.onclose = (event) => this.closed(event.code);
    return settled;
  }

  private async startCapture(): Promise<void> {
    try {
      const capture = await this.deps.capture(this.stream, (pcm) => this.frame(pcm));
      if (this.ended || this.muted) capture.stop();
      else this.capture = capture;
    } catch {
      this.fail(this.opening ? "unavailable" : "dropped");
    }
  }

  private frame(pcm: ArrayBuffer): void {
    if (this.ended || this.socket.readyState !== OPEN) return;
    if (!this.ready) {
      if (this.early + pcm.byteLength > BEFORE_READY_BYTES) return;
      this.early += pcm.byteLength;
    }
    this.socket.send(pcm);
  }

  private receive(raw: string): void {
    let m: Message;
    try {
      m = JSON.parse(raw) as Message;
    } catch {
      return;
    }
    if (this.muted) return;
    switch (m.type) {
      case "preparing":
        this.events.preparing?.(typeof m.progress === "number" ? m.progress : undefined);
        this.settled();
        return;
      case "ready":
        this.listening();
        this.events.ready?.();
        return;
      case "partial":
        this.listening();
        this.events.partial(m.text ?? "");
        return;
      case "final":
        this.listening();
        this.events.final(m.text ?? "");
        return;
      case "busy":
        this.fail(this.opening ? "busy" : "dropped");
        return;
      case "error":
        this.fail(this.opening ? "unavailable" : "dropped");
        return;
      case "done": {
        if (this.flushed) return this.finish();
        const stop = DONE_STOPS[m.reason as DictationDoneReason];
        this.fail(stop === "dropped" && this.opening ? "unavailable" : (stop ?? "dropped"));
        return;
      }
    }
  }

  private listening(): void {
    this.ready = true;
    this.opening = false;
    this.settled();
  }

  private settled(): void {
    this.settle?.();
    this.settle = null;
  }

  private closed(code: number): void {
    if (this.flushed) return this.finish();
    if (this.ended || this.muted) return;
    const refused = code === CLOSE_UNAUTHENTICATED || code === CLOSE_FORBIDDEN;
    this.fail(this.opening ? "unavailable" : refused ? "refused" : "dropped");
  }

  /** Ends the session from the engine's side, once, and says why. */
  private fail(code: DictationErrorCode): void {
    if (this.ended || this.muted) return;
    this.teardown();
    this.events.error(code);
    this.settled();
  }

  private teardown(): void {
    this.ended = true;
    this.capture?.stop();
    this.capture = null;
    if (this.socket.readyState <= OPEN) this.socket.close();
  }

  private finish(): void {
    const flushed = this.flushed;
    this.flushed = null;
    this.teardown();
    flushed?.();
  }

  switchStream(stream: MediaStream): void {
    this.capture?.switchStream(stream);
  }

  /** Stops the audio and waits for the engine's last finals. */
  flush(): Promise<void> {
    if (this.ended) return Promise.resolve();
    this.capture?.stop();
    this.capture = null;
    return new Promise<void>((resolve) => {
      this.flushed = resolve;
      if (this.socket.readyState === OPEN) this.socket.send(JSON.stringify({ type: "finish" }));
      else this.finish();
    });
  }

  cancel(): void {
    if (this.muted) return;
    this.muted = true;
    if (!this.ended && this.socket.readyState === OPEN)
      this.socket.send(JSON.stringify({ type: "cancel" }));
    this.teardown();
    this.settled();
  }
}

/** The engine relay route, for `dictation.transport` ahead of the browser's own. */
export function createRelay(deps: RelayDeps): DictationTransport {
  let session: Session | null = null;
  return {
    picksDevice: false,
    available: () => deps.available(),
    start(_deviceId, events, stream) {
      session?.cancel();
      session = null;
      // The relay hears the kit's open microphone; with none there is nothing to send.
      if (!stream) return Promise.reject(new Error("no microphone stream"));
      session = new Session(deps, events, stream, deps.socket());
      return session.open();
    },
    switchStream(stream) {
      if (stream) session?.switchStream(stream);
    },
    flush: () => session?.flush() ?? Promise.resolve(),
    cancel() {
      session?.cancel();
      session = null;
    },
  };
}
