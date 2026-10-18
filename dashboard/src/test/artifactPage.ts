/**
 * An artifact page and everything behind it, end to end in the suite's own
 * window: the COMMITTED runtime build (`static/artifacts/runtime/<contract>/
 * runtime.js`, the bytes the artifact origin serves), the real page host and
 * the real datastore the dashboard runs, over an engine held in memory that
 * answers the routes and queries the store makes.
 *
 * ONE PAGE PER SUITE FILE: the runtime installs `window.crewlet` once and
 * makes it unwritable, exactly as it does in a frame, so a suite opens its
 * page once and walks through what it proves.
 *
 * THE FRAME IS THE SUITE'S WINDOW with its parent stood in for. jsdom's
 * `postMessage` carries neither a source nor transferred ports, so the
 * handshake is driven here as the browser drives it: the runtime's hello
 * reaches the stand-in parent, which hands the host one end of a
 * `MessageChannel` and dispatches the port message to the page from that
 * parent. Everything after it travels on the real port.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { FRAME_BRIDGE, FRAME_MESSAGE } from "@crewlethq/ui/FrameProtocol";
import { RUNTIME_CONTRACT } from "~/contract/artifacts.ts";
import {
  QueryError,
  RestError,
  type ArtifactActor,
  type DataCollection,
  type DataDoc,
  type DataOp,
  type DataSchemaAnswer,
  type LiveSocket,
} from "~/protocol/index.ts";
import { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import type { KeyedAnswer, KeyedRequest } from "~/lib/keyedWrite.ts";
import { PageHost } from "~/routes/artifacts/host.ts";
import type {
  BridgePerson,
  BridgeRoster,
  BridgeViewer,
  RuntimeCapability,
} from "~/runtime/bridge.ts";
import { repoRoot } from "./engineFiles.ts";

const ARTIFACT = "0192f00e-0000-7000-8000-0000000000a1";

/** What one write changed, kept so an undo can put it back. */
interface Step {
  /** Each document the write touched, as it was before, or null where it did not exist. */
  before: Map<string, DataDoc | null>;
  /** The version each touched document reached. */
  after: Map<string, number>;
}

const docKey = (collection: string, id: string) => `${collection}\u0000${id}`;

/**
 * The engine, as much of it as a page reaches: a datastore whose writes are
 * decided here, in order, and pushed to every store that reads it.
 *
 * It refuses as the real one does on both sides of the log: a value a
 * `number` column cannot hold is refused BEFORE the log (`422 invalid`, no
 * verdict), and an undo of a write somebody has changed since is decided
 * `conflict` by the applier.
 */
export class MemoryEngine {
  private seq = 0;
  private readonly docs = new Map<string, DataDoc>();
  private readonly steps = new Map<number, Step>();
  private readonly stores = new Set<ArtifactDataStore>();
  /** Collections the viewer may not read: the read is refused on authority. */
  readonly unreadable = new Set<string>();

  constructor(
    private readonly collections: DataCollection[],
    private readonly author: ArtifactActor,
  ) {}

  /** Puts documents in place as somebody else's earlier writes. */
  seed(collection: string, rows: Record<string, Record<string, unknown>>): void {
    for (const [id, fields] of Object.entries(rows)) {
      this.apply([{ op: "set", collection, doc: id, fields }], { name: "ada", kind: "agent" });
    }
  }

  /** A store reading this engine, as a page's frame holds one. */
  attach(store: ArtifactDataStore): void {
    this.stores.add(store);
  }

  readonly socket = {
    query: async (kind: string, params: Record<string, unknown>): Promise<unknown> => {
      const collection = typeof params["collection"] === "string" ? params["collection"] : "";
      switch (kind) {
        case "artifact_data_schema":
          return {
            collections: this.collections,
            store: {},
            role: "editor",
          } as unknown as DataSchemaAnswer;
        case "artifact_data": {
          if (this.unreadable.has(collection)) throw new QueryError("unauthorized");
          if (!this.collections.some((c) => c.name === collection))
            throw new QueryError("not_found");
          const docs = [...this.docs.values()].filter((d) => d.collection === collection);
          return {
            docs,
            through: this.seq,
            position: { stream: "data", generation: 0, seq: this.seq },
            complete: true,
          };
        }
        case "artifact_data_doc": {
          const doc = this.docs.get(docKey(collection, String(params["doc"])));
          if (!doc) throw new QueryError("not_found");
          return doc;
        }
        default:
          throw new QueryError("not_found");
      }
    },
  } as unknown as LiveSocket;

  /** One keyed write, decided as the engine decides it. */
  readonly write = async (req: KeyedRequest): Promise<KeyedAnswer> => {
    const body = req.body as { ops?: DataOp[]; seq?: number };
    if (req.path.endsWith("/data/undo")) return this.undo(Number(body.seq));
    const ops = body.ops ?? [];
    const wrong = this.wrongValue(ops);
    if (wrong) {
      // REFUSED BEFORE THE LOG: the grammar's own check, with no verdict.
      return refused(422, { error: "invalid", detail: wrong });
    }
    return this.answer(this.apply(ops, this.author));
  };

  private wrongValue(ops: readonly DataOp[]): string | null {
    for (const op of ops) {
      const columns = this.collections.find((c) => c.name === op.collection)?.columns ?? [];
      for (const [field, value] of Object.entries(op.fields ?? {})) {
        const column = columns.find((c) => c.key === field);
        if (column?.type === "number" && value !== null && typeof value !== "number") {
          return `${field} is a number`;
        }
      }
    }
    return null;
  }

  private undo(seq: number): KeyedAnswer {
    const step = this.steps.get(seq);
    if (!step) return refused(409, { error: "not_undoable", detail: `no write ${seq}` });
    for (const [key, version] of step.after) {
      if ((this.docs.get(key)?.version ?? 0) !== version) {
        return refused(409, {
          error: "stale_version",
          verdict: "conflict",
          detail: "Somebody changed this since.",
          seq: ++this.seq,
        });
      }
    }
    const ops: DataOp[] = [...step.before].map(([key, doc]) => {
      const [collection, id] = key.split("\u0000") as [string, string];
      return doc
        ? { op: "set", collection, doc: id, fields: { ...(doc.fields ?? {}) } }
        : { op: "delete", collection, doc: id };
    });
    return this.answer(this.apply(ops, this.author));
  }

  private answer(seq: number): KeyedAnswer {
    return {
      kind: "done",
      pending: false,
      body: { verdict: "applied", seq, position: { stream: "data", generation: 0, seq } },
    };
  }

  /** Applies ops as one write, records how to undo it, and pushes what changed. */
  private apply(ops: readonly DataOp[], by: ArtifactActor): number {
    const seq = ++this.seq;
    const step: Step = { before: new Map(), after: new Map() };
    const now = new Date(Date.UTC(2026, 9, 18, 10, 0, seq)).toISOString();
    for (const op of ops) {
      const key = docKey(op.collection, op.doc);
      const held = this.docs.get(key);
      if (!step.before.has(key)) step.before.set(key, held && !held.deleted ? held : null);
      const fields: Record<string, unknown> =
        op.op === "set" ? {} : { ...(held && !held.deleted ? held.fields : {}) };
      for (const [field, value] of Object.entries(op.fields ?? {})) {
        if (value === null) delete fields[field];
        else fields[field] = value;
      }
      this.docs.set(key, {
        collection: op.collection,
        id: op.doc,
        ...(op.op === "delete" ? { deleted: true } : { fields }),
        version: seq,
        created_by: held?.created_by ?? by,
        created_at: held?.created_at ?? now,
        updated_by: by,
        updated_at: now,
      });
      step.after.set(key, seq);
    }
    this.steps.set(seq, step);
    const deltas = [...step.after.keys()].map((key) => {
      const doc = this.docs.get(key) as DataDoc;
      return {
        collection: doc.collection,
        doc: doc.id,
        version: doc.version,
        ...(doc.deleted ? { deleted: true } : { body: doc.fields ?? {} }),
        read: "all" as const,
        created_by: doc.created_by.name,
        updated_by: doc.updated_by,
        updated_at: doc.updated_at,
      };
    });
    for (const store of this.stores) store.applyPush({ artifact: ARTIFACT, deltas, through: seq });
    return seq;
  }
}

function refused(status: number, body: Record<string, unknown>): KeyedAnswer {
  return {
    kind: "refused",
    text: typeof body["detail"] === "string" ? body["detail"] : "Refused.",
    stale: false,
    error: new RestError(status, body),
  };
}

/** A refusal as a page catches it. */
export interface PageError extends Error {
  code: string;
}

/** A write's outcome as a page reads it. */
export interface PageOutcome {
  verdict: string;
  seq?: number;
  reason?: string;
}

/** The part of `window.crewlet` the suites call, typed as the contract documents it. */
export interface PageRuntime {
  readonly version: string;
  readonly ready: Promise<void>;
  capabilities: { has(name: string): boolean; list(): string[] };
  identity: { me(): Promise<BridgeViewer>; people(): Promise<BridgePerson[]> };
  data: {
    collection(name: string): {
      subscribe(
        cb: (docs: { id: string; data: Record<string, unknown> }[]) => void,
        onError?: (err: PageError) => void,
      ): () => void;
      merge(id: string, fields: Record<string, unknown>): Promise<PageOutcome>;
      set(id: string, fields: Record<string, unknown>): Promise<PageOutcome>;
    };
    schema(): Promise<{ name: string; title?: string }[]>;
    undo(): Promise<PageOutcome | null>;
    redo(): Promise<PageOutcome | null>;
  };
  presence: { roster(): BridgeRoster };
}

/** The page's runtime, as installed on the window. */
export function pageRuntime(): PageRuntime {
  return (window as unknown as { crewlet: PageRuntime }).crewlet;
}

/** What a suite opens a page with. */
export interface PageOptions {
  capabilities: RuntimeCapability[];
  engine: MemoryEngine;
  viewer: BridgeViewer;
  people?: BridgePerson[];
  roster?: BridgeRoster;
}

/** An open page: its runtime, and the host and store behind it. */
export interface OpenPage {
  host: PageHost;
  store: ArtifactDataStore;
  /** Closes the port, so nothing keeps the suite's process alive. */
  close(): void;
}

/** The runtime build the artifact origin serves for this contract, as committed. */
function runtimeScript(): string {
  return readFileSync(
    join(repoRoot(), "static", "artifacts", "runtime", RUNTIME_CONTRACT, "runtime.js"),
    "utf-8",
  );
}

/**
 * Opens a page in the suite's window: installs the committed runtime, answers
 * its hello with a port to a real host, and resolves once `crewlet.ready` has.
 */
export async function openPage(options: PageOptions): Promise<OpenPage> {
  let n = 0;
  const store = new ArtifactDataStore({
    artifact: ARTIFACT,
    socket: options.engine.socket,
    via: { via: "bridge", version: 1 },
    clientID: "page-harness",
    write: options.engine.write,
    newKey: () => `key-${++n}`,
  });
  options.engine.attach(store);
  const host = new PageHost({
    artifact: ARTIFACT,
    version: 1,
    capabilities: options.capabilities,
    socket: options.engine.socket,
    store,
    clientID: "page-harness",
    newKey: () => `key-${++n}`,
    openLink: async () => false,
    setPresence: () => {},
    write: options.engine.write,
  });
  host.setViewer(options.viewer);
  host.setPeople(options.people ?? []);
  if (options.roster) host.setRoster(options.roster);

  const channel = new MessageChannel();
  const parent = {
    postMessage(message: unknown) {
      const hello = message as { type?: string; nonce?: string };
      if (hello.type !== FRAME_MESSAGE.hello || hello.nonce !== NONCE) return;
      channel.port1.addEventListener("message", (event: MessageEvent) => {
        void host.receive(event.data, channel.port1);
      });
      channel.port1.start();
      host.attach(channel.port1);
      const delivered = new Event("message");
      Object.defineProperties(delivered, {
        data: { value: { type: FRAME_MESSAGE.port, bridge: FRAME_BRIDGE.max } },
        source: { value: parent },
        ports: { value: [channel.port2] },
      });
      window.dispatchEvent(delivered);
    },
  };
  window.location.hash = `nonce=${NONCE}&theme=light`;
  Object.defineProperty(window, "parent", { value: parent, configurable: true });
  // THE RUNTIME IS A CLASSIC SCRIPT the origin places first in the page: run
  // as one, against this window's globals.
  new Function(runtimeScript())();
  await pageRuntime().ready;
  return {
    host,
    store,
    close() {
      host.stop();
      store.stop();
      channel.port1.close();
      channel.port2.close();
    },
  };
}

const NONCE = "harness-nonce-0123456789";
