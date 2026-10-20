/**
 * The dashboard's client, with a demo engine where the network would be.
 *
 * [makeDemoClient] returns what `src/main.tsx` builds, a `Store` and a socket,
 * so `ClientContext` takes it unchanged. The socket is a [DemoSocket]: the
 * dashboard's own `LiveSocket` with its transport replaced and its DISPATCH
 * kept. Every frame the demo engine pushes, the handshake snapshot included,
 * reaches the store through `LiveSocket.onMessage` as the JSON text a real
 * socket would deliver, so the protocol code the e2e replay certifies is the
 * code a prototype runs. Only the wire is gone: it never opens a WebSocket,
 * and a query is answered by the engine rather than sent.
 */

import { takeSession } from "~/lib/session.ts";
import {
  LiveSocket,
  QueryError,
  Store,
  type PresenceActivity,
  type PresenceFocus,
  type QueryMap,
  type QueryName,
  type RoomListener,
} from "~/protocol/index.ts";
import { joinRoom } from "./engine/artifacts.ts";
import type { DemoCompany } from "./engine/company.ts";
import { DemoEngine, DemoRefusal, LOG } from "./engine/engine.ts";

/**
 * How long the demo engine takes to answer a question, in milliseconds: a
 * round trip to an engine on the same network. Long enough that a screen's
 * loading state is drawn for a frame, as it is in use, and short enough that
 * nobody waits on it.
 */
export const ANSWER_MS = 20;

/** The dashboard's socket, answered by a demo engine instead of a server. */
export class DemoSocket extends LiveSocket {
  readonly engine: DemoEngine;
  /** The base class keeps its store private; the demo's handshake needs it. */
  private readonly demoStore: Store;
  private open = false;
  private holding = false;
  /** The seat this socket watches for `inbox_changed`, "" for none. */
  private watching = "";
  private unsubscribe: (() => void) | null = null;
  /** Every artifact room a screen is in, by artifact: the base class's rooms need a wire. */
  private readonly demoRooms = new Map<string, Set<RoomListener>>();

  constructor(store: Store, engine: DemoEngine) {
    // THE SHIPPED BOOT'S OPTIONS (`src/main.tsx`): a re-dial after a session
    // ended is for this tab's own reader only.
    super(store, { takeSession });
    this.demoStore = store;
    this.engine = engine;
  }

  /**
   * Dial: connected at once, then the handshake snapshot, as a real socket's
   * `onopen` and first frame. The frame calls it once it has read the
   * session (`app/Shell.tsx`), and again for every session it reads, which
   * an open socket ignores.
   */
  override start(): void {
    if (this.open || this.holding) return;
    this.open = true;
    this.unsubscribe = this.engine.onPush((push) => {
      if (!this.open) return;
      // AN INBOX FRAME GOES ONLY TO A SOCKET WATCHING THAT SEAT, as the hub sends it.
      if (push.kind === "inbox_changed") {
        const handle = (push.data as { handle?: string } | null)?.handle ?? "";
        if (this.watching === "" || handle !== this.watching) return;
      }
      // A ROOM'S PUSH goes to that room's listeners, read off its own
      // `artifact`, as the base class routes one off the wire.
      const room = ROOM_PUSHES[push.kind as keyof typeof ROOM_PUSHES];
      if (room) {
        const artifact = (push.data as { artifact?: string } | null)?.artifact ?? "";
        const data = JSON.parse(JSON.stringify(push.data)) as never;
        for (const listener of this.demoRooms.get(artifact) ?? []) listener[room]?.(data);
        return;
      }
      this.onMessage(JSON.stringify(push));
    });
    this.demoStore.setAuthRejected(false);
    this.demoStore.setAccessRefused(null);
    this.demoStore.setConnected(true);
    this.onMessage(JSON.stringify({ kind: "snapshot", data: this.engine.snapshot() }));
  }

  /** Watch one seat's inbox, or none with "": one per socket, a second replacing the first. */
  override watch(seat: string): void {
    this.watching = seat.trim();
  }

  /** Dial nothing while this tab signs out (`lib/session.ts`). */
  override hold(): void {
    this.holding = true;
  }

  /** A sign-out nothing answered stays on the page, and the socket dials again. */
  override release(): void {
    this.holding = false;
    this.start();
  }

  /** A fresh handshake, which re-hydrates every slice exactly as a reconnect does. */
  override reconnect(): void {
    this.stop();
    this.start();
  }

  override stop(): void {
    this.open = false;
    this.unsubscribe?.();
    this.unsubscribe = null;
    this.demoStore.setConnected(false);
  }

  override get connected(): boolean {
    return this.open;
  }

  /**
   * Be in an artifact's room: the engine answers the join after a round trip,
   * and its pushes for the artifact reach `listener` until the way out is
   * called. A refusal reaches `refused` in the words the engine's join uses.
   */
  override room(artifact: string, listener: RoomListener): () => void {
    const id = artifact.trim();
    const listeners = this.demoRooms.get(id) ?? new Set<RoomListener>();
    this.demoRooms.set(id, listeners);
    listeners.add(listener);
    const timer = setTimeout(() => {
      if (!listeners.has(listener)) return;
      try {
        listener.joined?.(JSON.parse(JSON.stringify(joinRoom(this.engine, id))) as never);
      } catch (err) {
        listener.refused?.(
          err instanceof DemoRefusal ? new QueryError(err.code) : new QueryError("query_failed"),
        );
      }
    }, ANSWER_MS);
    return () => {
      clearTimeout(timer);
      listeners.delete(listener);
      if (listeners.size === 0) this.demoRooms.delete(id);
    };
  }

  /** What the reader is doing in a room: nobody else is listening in a demo. */
  override presence(_artifact: string, _activity: PresenceActivity, _focus: PresenceFocus): void {}

  /**
   * Ask the demo engine. The answer crosses JSON on its way back, as an
   * answer off the wire does, so a screen can never hold a reference into the
   * engine's own company and a value JSON cannot carry never reaches it.
   */
  override query<K extends QueryName>(
    what: K,
    params?: Record<string, unknown>,
  ): Promise<QueryMap[K]> {
    return new Promise<QueryMap[K]>((resolve, reject) => {
      setTimeout(() => {
        if (!this.open) {
          reject(new QueryError("closed"));
          return;
        }
        try {
          const answer = this.engine.answer(what, params ?? {});
          resolve(JSON.parse(JSON.stringify(answer ?? null)) as QueryMap[K]);
        } catch (err) {
          if (err instanceof DemoRefusal) {
            reject(new QueryError(err.code, null, err.detail));
            return;
          }
          console.error(LOG, `Answering "${what}" failed inside the demo engine.`, err);
          reject(new QueryError("query_failed"));
        }
      }, ANSWER_MS);
    });
  }
}

/** The pushes an artifact's room hears, and the listener each reaches. */
const ROOM_PUSHES = {
  artifact_changed: "changed",
  artifact_data: "data",
  artifact_presence: "presence",
} as const;

/** What `src/main.tsx` builds, over a demo company: the store, the socket and the engine behind it. */
export interface DemoClient {
  store: Store;
  socket: DemoSocket;
  engine: DemoEngine;
}

/**
 * A store and a socket over `company`, ready for `ClientContext`. Call
 * `socket.start()` to connect, as the shell does.
 */
export function makeDemoClient(company: DemoCompany): DemoClient {
  const engine = new DemoEngine(company);
  const store = new Store();
  return { store, socket: new DemoSocket(store, engine), engine };
}
