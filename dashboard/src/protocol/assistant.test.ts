/**
 * The assistant's transport: its routes, and a turn's stream as the engine
 * serves it.
 *
 * THE STREAM IS THE ENGINE'S OWN GOLDEN (`internal/assistant/wire/testdata/
 * frames.sse`), served through a stubbed `fetch` in chunks cut anywhere, so a
 * resume, a stall and a refusal are each exercised over bytes the engine's
 * encoder wrote rather than a fixture typed to agree with this client.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { eventually } from "~/test/settle.ts";

import { repoRoot } from "~/test/engineFiles.ts";
import {
  call,
  exportFile,
  isAssistantPath,
  openTurnStream,
  versioned,
  type StreamStop,
} from "./assistant.ts";
import type { AssistantNarrowFrame } from "./assistantWire.ts";
import { RestError } from "./rest.ts";

const GOLDEN = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);
/** The golden's first turn, the events of one stream. */
const FIRST = GOLDEN.split("\n\n")
  .filter((block) => block.includes("3db96f52-a891-5953-9735-0ae522cf6347."))
  .map((block) => `${block}\n\n`);

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** A body that hands over `chunks` one read at a time, then ends or hangs. */
function body(chunks: string[], end: "close" | "hang" = "close"): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let at = 0;
  return new ReadableStream({
    pull(controller) {
      if (at < chunks.length) {
        controller.enqueue(encoder.encode(chunks[at++]!));
        return;
      }
      if (end === "close") controller.close();
      return new Promise(() => {});
    },
  });
}

const sse = (stream: ReadableStream<Uint8Array>) =>
  new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } });

/** Every request sent, and a fetch answering each in turn from `answers`. */
function serve(...answers: ((init: RequestInit) => Response | Promise<Response>)[]) {
  const sent: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      const answer = answers[Math.min(sent.length - 1, answers.length - 1)]!;
      return answer(init);
    }),
  );
  return sent;
}

/** Open a stream and collect what it reports until it closes. */
function watch(url: string, lastEventId?: string, stallMs = 60_000) {
  const frames: AssistantNarrowFrame[] = [];
  const states: string[] = [];
  const stops: StreamStop[] = [];
  let closed!: () => void;
  const ended = new Promise<void>((resolve) => {
    closed = resolve;
  });
  const stream = openTurnStream(
    url,
    {
      frame: (f) => frames.push(f),
      state: (s) => {
        states.push(s);
        if (s === "closed") closed();
      },
      stop: (r) => stops.push(r),
    },
    { lastEventId, stallMs, backoffMs: 5, maxBackoffMs: 10 },
  );
  return { frames, states, stops, ended, stream };
}

const URL_ = "/assistant/conversations/c1/stream?turn=3db96f52-a891-5953-9735-0ae522cf6347";

describe("a turn's stream", () => {
  test("reads every frame once, cut anywhere, and closes when the turn is done", async () => {
    const text = FIRST.join("");
    const pieces: string[] = [];
    for (let i = 0; i < text.length; i += 37) pieces.push(text.slice(i, i + 37));
    const sent = serve(() => sse(body(pieces)));
    const w = watch(URL_);
    await w.ended;
    expect(w.frames.map((f) => f.seq)).toEqual(FIRST.map((_, i) => i + 1));
    expect(w.states).toEqual(["connecting", "live", "closed"]);
    expect(w.stops).toEqual([]);
    // THE SESSION COOKIE AND NOTHING ELSE, and no cache in front of it.
    expect(sent[0]!.init.credentials).toBe("same-origin");
    expect(sent[0]!.init.cache).toBe("no-store");
    expect(new Headers(sent[0]!.init.headers).get("Last-Event-ID")).toBeNull();
  });

  // A RESUME ASKS FOR WHAT IT MISSED: the connection dropped after the fifth
  // frame, and the next dial names it, so the engine replays only the rest.
  test("a dropped connection is dialled again from the last frame it read", async () => {
    const sent = serve(
      () => sse(body(FIRST.slice(0, 5))),
      () => sse(body(FIRST.slice(5))),
    );
    const w = watch(URL_);
    await w.ended;
    expect(w.frames.map((f) => f.seq)).toEqual(FIRST.map((_, i) => i + 1));
    expect(w.states).toContain("reconnecting");
    expect(new Headers(sent[1]!.init.headers).get("Last-Event-ID")).toBe(
      "3db96f52-a891-5953-9735-0ae522cf6347.5",
    );
  });

  test("a stream the caller already holds part of resumes from the frame it names", async () => {
    const sent = serve(() => sse(body(FIRST.slice(3))));
    const w = watch(URL_, "3db96f52-a891-5953-9735-0ae522cf6347.3");
    await w.ended;
    expect(new Headers(sent[0]!.init.headers).get("Last-Event-ID")).toBe(
      "3db96f52-a891-5953-9735-0ae522cf6347.3",
    );
  });

  // A QUIET STREAM IS STALLED after two missed keepalives: a connection a
  // proxy silently dropped never errors, it just stops, and only a timer can
  // tell it from a long tool call.
  test("a stream that goes quiet past its keepalives is dialled again", async () => {
    const sent = serve(
      () => sse(body(FIRST.slice(0, 2), "hang")),
      () => sse(body(FIRST.slice(2))),
    );
    const w = watch(URL_, undefined, 30);
    await w.ended;
    expect(sent).toHaveLength(2);
    expect(new Headers(sent[1]!.init.headers).get("Last-Event-ID")).toBe(
      "3db96f52-a891-5953-9735-0ae522cf6347.2",
    );
    expect(w.frames).toHaveLength(FIRST.length);
  });

  test("a frame of another protocol version stops the stream for good", async () => {
    const other = FIRST[0]!.replace('"v":1', '"v":2');
    const sent = serve(() => sse(body([other, ...FIRST.slice(1)])));
    const w = watch(URL_);
    await w.ended;
    expect(w.stops).toEqual([{ code: "protocol_mismatch", version: 2 }]);
    expect(w.frames).toEqual([]);
    expect(sent).toHaveLength(1);
  });

  // A FAULT ENDS THE CONNECTION TOO, not only the reading: a body left open
  // and unread holds the engine's handler on a write until the tab closes.
  test("a stream stopped by a fault aborts its connection", async () => {
    const other = FIRST[0]!.replace('"v":1', '"v":2');
    const sent = serve(() => sse(body([other], "hang")));
    const w = watch(URL_);
    await w.ended;
    expect(w.stops[0]?.code).toBe("protocol_mismatch");
    expect(sent[0]!.init.signal?.aborted).toBe(true);
  });

  // A LONG OUTAGE HOLDS NOTHING: each reconnect's wait and each attempt let go
  // of the stream's own signal, the one that outlives every dial, so
  // listeners do not pile up on it per reconnect.
  test("reconnecting leaves no listener behind on the stream's signal", async () => {
    const added = vi.spyOn(AbortSignal.prototype, "addEventListener");
    const removed = vi.spyOn(AbortSignal.prototype, "removeEventListener");
    serve(
      () => new Response("{}", { status: 503 }),
      () => new Response("{}", { status: 503 }),
      () => new Response("{}", { status: 503 }),
      () => sse(body(FIRST)),
    );
    const w = watch(URL_);
    await w.ended;
    const adds = new Map<AbortSignal, number>();
    const held = new Map<AbortSignal, number>();
    const count = (spy: typeof added, by: number) =>
      spy.mock.calls.forEach(([type], i) => {
        if (type !== "abort") return;
        const signal = spy.mock.contexts[i] as AbortSignal;
        if (by > 0) adds.set(signal, (adds.get(signal) ?? 0) + 1);
        held.set(signal, (held.get(signal) ?? 0) + by);
      });
    count(added, 1);
    count(removed, -1);
    added.mockRestore();
    removed.mockRestore();
    // The stream's own signal is the one every dial and every wait listens on.
    const [outer, dials] = [...adds].sort((a, b) => b[1] - a[1])[0]!;
    expect(dials).toBeGreaterThanOrEqual(7);
    expect(held.get(outer)).toBe(0);
  });

  // A BUSY ENGINE IS WAITED OUT, NOT A FAULT: a `429 assistant_busy` on the
  // stream is dialled again after the Retry-After it names, not after the
  // backoff, and the stream is not stopped. Mutation: dial on the backoff
  // and the second dial comes milliseconds after the first.
  test("a busy stream is dialled again after the wait the engine named", async () => {
    const at: number[] = [];
    serve(
      () => {
        at.push(Date.now());
        return new Response(JSON.stringify({ error: "assistant_busy" }), {
          status: 429,
          headers: { "Content-Type": "application/json", "Retry-After": "1" },
        });
      },
      () => {
        at.push(Date.now());
        return sse(body(FIRST));
      },
    );
    const w = watch(URL_);
    await w.ended;
    expect(w.stops).toEqual([]);
    expect(w.frames).toHaveLength(FIRST.length);
    expect(at[1]! - at[0]!).toBeGreaterThanOrEqual(950);
  });

  test("a refusal stops it with the engine's own answer; a 5xx is dialled again", async () => {
    const refused = serve(
      () =>
        new Response(JSON.stringify({ error: "not_found", detail: "no such turn" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        }),
    );
    const w = watch(URL_);
    await w.ended;
    expect(refused).toHaveLength(1);
    const stop = w.stops[0];
    expect(stop?.code).toBe("refused");
    expect(stop?.code === "refused" && stop.error).toBeInstanceOf(RestError);

    const retried = serve(
      () => new Response("{}", { status: 503, headers: { "Retry-After": "1" } }),
      () => sse(body(FIRST)),
    );
    const again = watch(URL_);
    await again.ended;
    expect(retried).toHaveLength(2);
    expect(again.stops).toEqual([]);
  });

  // A PARKED TURN REOPENED AT ITS LAST FRAME: the engine answers 204, the
  // event-stream answer that says nothing follows. Mutation: read the 204 as
  // an unreadable answer (or an empty stream) and it is dialled again.
  test("a 204 ends the stream, and nothing is dialled again", async () => {
    const sent = serve(() => new Response(null, { status: 204 }));
    const w = watch(URL_, "3db96f52-a891-5953-9735-0ae522cf6347.21");
    await w.ended;
    expect(sent).toHaveLength(1);
    expect(w.stops).toEqual([]);
    expect(w.states).toEqual(["connecting", "closed"]);
  });

  // ANSWERED IS NOT PROGRESS: a connection the engine answers and closes with
  // nothing new is the same outage, so the backoff grows and the outage keeps
  // the instant it began (which is what the drawer's "Try again" measures).
  // Mutation: reset the backoff on the status line, and every dial comes at
  // the first step, as the drawer's loop of 22 requests a minute did.
  test("connections answered with nothing back off as one outage", async () => {
    const at: number[] = [];
    const empty = () => {
      at.push(Date.now());
      return sse(body([]));
    };
    serve(empty, empty, empty, empty, () => sse(body(FIRST)));
    const since: (number | null)[] = [];
    let closed!: () => void;
    const ended = new Promise<void>((resolve) => {
      closed = resolve;
    });
    openTurnStream(
      URL_,
      {
        frame: () => {},
        state: (state, outage) => {
          if (state === "reconnecting") since.push(outage);
          if (state === "closed") closed();
        },
        stop: () => {},
      },
      { backoffMs: 20, maxBackoffMs: 1_000 },
    );
    await ended;
    const gaps = at.slice(1).map((t, i) => t - at[i]!);
    expect(gaps[1]!).toBeGreaterThan(gaps[0]!);
    expect(gaps[2]!).toBeGreaterThan(gaps[1]!);
    expect(new Set(since).size).toBe(1);
    expect(since[0]).not.toBeNull();
  });

  test("closing it ends the read and dials nothing more", async () => {
    const sent = serve(() => sse(body(FIRST.slice(0, 1), "hang")));
    const w = watch(URL_);
    await eventually(() => expect(w.frames).toHaveLength(1));
    w.stream.close();
    await w.ended;
    expect(sent).toHaveLength(1);
  });

  test("a stream address that is not the assistant's is never dialled", async () => {
    const sent = serve(() => sse(body(FIRST)));
    const w = watch("https://elsewhere.example/assistant/x");
    await w.ended;
    expect(sent).toHaveLength(0);
    expect(w.stops[0]?.code).toBe("invalid_path");
  });
});

describe("the assistant's own paths", () => {
  test("only a path under /assistant/ on this origin is followed", () => {
    expect(isAssistantPath("/assistant/cards/c1/export.csv")).toBe(true);
    expect(isAssistantPath("/assistant/conversations/c1/stream?turn=t")).toBe(true);
    for (const refused of [
      "//evil.example/assistant/x",
      "https://evil.example/assistant/x",
      "/assistant/../operator/act/x",
      "/assistant/./x",
      "/assistant/x\\y",
      "/assistant/x\ny",
      "/worklet/items",
      "assistant/x",
    ]) {
      expect(isAssistantPath(refused), refused).toBe(false);
    }
  });

  test("an export is read from its card's own path and refused anywhere else", async () => {
    const sent = serve(() => new Response("a,b\n", { status: 200 }));
    await exportFile("/assistant/cards/c1/export.csv");
    expect(sent[0]!.url.endsWith("/assistant/cards/c1/export.csv")).toBe(true);
    await expect(exportFile("/operator/act/x")).rejects.toBeInstanceOf(RestError);
    expect(sent).toHaveLength(1);
  });
});

describe("a route", () => {
  test("is sent at its table's path with the id encoded, its version and its operation key", async () => {
    const sent = serve(
      () =>
        new Response(JSON.stringify({ turn_id: "t", stream_url: "/assistant/x", after_seq: 0 }), {
          status: 202,
          headers: { "Content-Type": "application/json" },
        }),
    );
    const accepted = await call(
      "POST /assistant/conversations/{id}/turns",
      { ...versioned, input: { text: "hi" }, page_context: null },
      { id: "c/1", opKey: "op-1" },
    );
    expect(accepted.turn_id).toBe("t");
    expect(sent[0]!.url.endsWith("/assistant/conversations/c%2F1/turns")).toBe(true);
    expect(sent[0]!.init.method).toBe("POST");
    const headers = new Headers(sent[0]!.init.headers);
    expect(headers.get("Idempotency-Key")).toBe("op-1");
    expect(headers.get("Content-Type")).toBe("application/json");
    expect(JSON.parse(String(sent[0]!.init.body))).toEqual({
      protocol_version: 1,
      input: { text: "hi" },
      page_context: null,
    });
  });

  test("an attachment is sent as its own bytes, typed by the file, named in the query", async () => {
    const stored = {
      file: {
        file_id: "f",
        name: "shot.png",
        media_type: "image/png",
        size: 3,
        download: "/assistant/x",
      },
    };
    const sent = serve(
      () =>
        new Response(JSON.stringify(stored), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
    );
    const image = new File([new Uint8Array([1, 2, 3])], "shot.png", { type: "image/png" });
    const answer = await call("POST /assistant/conversations/{id}/files", image, {
      id: "c1",
      query: { name: "shot.png" },
      opKey: "op-2",
    });
    expect(answer).toEqual(stored);
    expect(sent[0]!.url).toMatch(/\/assistant\/conversations\/c1\/files\?name=shot\.png$/);
    const headers = new Headers(sent[0]!.init.headers);
    expect(headers.get("Content-Type")).toBe("image/png");
    expect(headers.get("Idempotency-Key")).toBe("op-2");
    expect(sent[0]!.init.body).toBe(image);
  });

  test("a route that names an id refuses to be sent without one", async () => {
    await expect(call("GET /assistant/conversations/{id}", null, {})).rejects.toBeInstanceOf(
      TypeError,
    );
  });
});
