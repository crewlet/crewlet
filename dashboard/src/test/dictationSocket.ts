/**
 * `/ws/dictation` for the layout suites, on the test server: the engine's
 * protocol over a real WebSocket, with the engine fake's rule for words (one
 * word of the script per 300 ms of voiced audio), so a suite in Chromium runs
 * the fake microphone, the worklet, the socket and the relay route end to end.
 * Node side only: `vitest.layout.config.ts` mounts it and exposes its commands.
 */
import { createHash } from "node:crypto";
import type { IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";
import type { Plugin } from "vite";

/** What the next dictation does after `start`. */
export type DictationMode = "ready" | "preparing" | "busy";

/** What one dictation sent, for a suite to assert. */
export interface DictationRecord {
  start: Record<string, unknown> | null;
  frames: number;
  bytes: number;
  /** Byte lengths seen, each once. */
  sizes: number[];
  controls: string[];
  closed: boolean;
}

const SCRIPT = ["Ship the fix today.", "Then tell the team."];
/** 300 ms of 16 kHz PCM16 reveals one word, as `speech.FakeWordAudio` does. */
const WORD_BYTES = (3 * 32_000) / 10;
/** The peak a frame needs to count as speech (`speech.FakeVoiced`). */
const VOICED = 1000;
const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

let mode: DictationMode = "ready";
const records: DictationRecord[] = [];

/** The suite's commands: set the next dictation's mode, read what was sent. */
export const dictationCommands = {
  dictationMode: (_ctx: unknown, next: DictationMode) => {
    mode = next;
  },
  dictationRecords: () => records.map((r) => ({ ...r })),
};

function frame(opcode: number, payload: Buffer): Buffer {
  const head =
    payload.length < 126
      ? Buffer.from([0x80 | opcode, payload.length])
      : Buffer.from([0x80 | opcode, 126, payload.length >> 8, payload.length & 0xff]);
  return Buffer.concat([head, payload]);
}

/** One connection: the handshake, then the engine's side of the protocol. */
function serve(req: IncomingMessage, socket: Duplex): void {
  const accept = createHash("sha1")
    .update(`${req.headers["sec-websocket-key"]}${GUID}`)
    .digest("base64");
  socket.write(
    "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
  );
  const record: DictationRecord = {
    start: null,
    frames: 0,
    bytes: 0,
    sizes: [],
    controls: [],
    closed: false,
  };
  records.push(record);
  const session = mode;
  let ready = false;
  let voiced = 0;
  let sentence = 0;
  let heard = 0;
  let ended = false;
  const say = (m: object) => {
    if (!ended) socket.write(frame(1, Buffer.from(JSON.stringify(m))));
  };
  const close = (code: number) => {
    if (ended) return;
    ended = true;
    socket.end(frame(8, Buffer.from([code >> 8, code & 0xff])));
  };
  const words = () => SCRIPT[sentence % SCRIPT.length]!.replace(/[.,]/g, "").split(" ");
  const finals = () => {
    if (heard > 0) say({ type: "final", seq: sentence, text: SCRIPT[sentence % SCRIPT.length] });
  };
  const text = (raw: string) => {
    const m = JSON.parse(raw) as Record<string, unknown>;
    if (record.start === null) {
      record.start = m;
      if (session === "busy") {
        say({ type: "busy", reason: "capacity" });
        return close(1013);
      }
      if (session === "preparing") {
        say({ type: "preparing", progress: 0.25 });
        setTimeout(() => say({ type: "preparing", progress: 0.75 }), 300);
        setTimeout(() => {
          ready = true;
          say({ type: "ready" });
        }, 900);
        return;
      }
      ready = true;
      say({ type: "ready" });
      return;
    }
    record.controls.push(String(m.type));
    if (m.type === "finish") finals();
    say({ type: "done", reason: m.type === "finish" ? "finish" : "cancel" });
    close(1000);
  };
  const audio = (pcm: Buffer) => {
    record.frames++;
    record.bytes += pcm.length;
    if (!record.sizes.includes(pcm.length)) record.sizes.push(pcm.length);
    if (!ready) return;
    let peak = 0;
    for (let i = 0; i + 1 < pcm.length; i += 2) peak = Math.max(peak, Math.abs(pcm.readInt16LE(i)));
    if (peak < VOICED) return;
    voiced += pcm.length;
    while (voiced >= WORD_BYTES) {
      voiced -= WORD_BYTES;
      heard++;
      const all = words();
      if (heard < all.length)
        say({ type: "partial", seq: sentence, text: all.slice(0, heard).join(" ") });
      else {
        finals();
        sentence++;
        heard = 0;
      }
    }
  };

  let buffered = Buffer.alloc(0);
  socket.on("data", (chunk: Buffer) => {
    buffered = Buffer.concat([buffered, chunk]);
    for (;;) {
      if (buffered.length < 2) return;
      const opcode = buffered[0]! & 0x0f;
      let length = buffered[1]! & 0x7f;
      let at = 2;
      if (length === 126) {
        if (buffered.length < 4) return;
        length = buffered.readUInt16BE(2);
        at = 4;
      } else if (length === 127) {
        if (buffered.length < 10) return;
        length = Number(buffered.readBigUInt64BE(2));
        at = 10;
      }
      if (buffered.length < at + 4 + length) return;
      const mask = buffered.subarray(at, at + 4);
      const payload = Buffer.from(buffered.subarray(at + 4, at + 4 + length));
      for (let i = 0; i < payload.length; i++) payload[i]! ^= mask[i % 4]!;
      buffered = buffered.subarray(at + 4 + length);
      if (opcode === 1) text(payload.toString("utf8"));
      else if (opcode === 2) audio(payload);
      else if (opcode === 8) close(1000);
    }
  });
  socket.on("close", () => {
    record.closed = true;
    ended = true;
  });
  socket.on("error", () => undefined);
}

/** Answers `/ws/dictation` upgrades on the test server. */
export function dictationSocket(): Plugin {
  return {
    name: "crewlet:test-dictation-socket",
    configureServer(server) {
      server.httpServer?.on("upgrade", (req: IncomingMessage, socket: Duplex) => {
        if ((req.url ?? "").split("?")[0] === "/ws/dictation") serve(req, socket);
      });
    },
  };
}
