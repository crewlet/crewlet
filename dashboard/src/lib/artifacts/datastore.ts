/**
 * One artifact's datastore as this tab holds it: the collections it has read,
 * kept current from the room's pushes, and the writes it has made and not yet
 * seen settle.
 *
 * TWO READERS, ONE STORE. The native sheet draws a collection from it and
 * writes the reader's edits through it as the dashboard (`via: dashboard`);
 * the page bridge serves a page's `crewlet.data` from it and writes as the
 * page's version (`via: bridge`, which the engine holds to the capabilities
 * that version declares). Both see the same documents, so a sheet and a page
 * open side by side never disagree about what the store holds.
 *
 * # Current, by version, not by arrival
 *
 * Every document carries its version (the packed position of the record that
 * last wrote it), and a delta is applied only when it is NEWER than what this
 * store holds for that document, tombstones included. So a push that arrives
 * while a collection is being read, a catch-up that overlaps a push, and a
 * re-read after a reconnect all converge on the same state whatever order
 * they land in, and a deleted row is never brought back by a stale delta.
 *
 * # Honest instant edits (D23)
 *
 * A write the reader makes is drawn at once as an OVERLAY on the cells it
 * touches, marked pending in the sheet's [SheetPendingEdits] until the engine
 * confirms it. Confirmed, the mark goes and the overlay stays only until the
 * document's own version reaches the write's position, when the canonical body
 * replaces it. Refused, the overlay goes, the cell reverts, and the reason is
 * said on the cell. A write whose outcome nobody could establish is kept, with
 * its operation key, for a Retry that sends exactly it again, which the engine
 * applies once.
 */

import { SheetPendingEdits, type SheetEdit, type SheetSettle } from "@crewlethq/ui";
import type {
  ArtifactDataPush,
  ArtifactJoined,
  DataDoc,
  DataHistoryEntry,
  DataOp,
  DataSchemaAnswer,
  DataWriteAnswer,
  LiveSocket,
  LogPosition,
} from "~/protocol/index.ts";
import { QueryError, RestError } from "~/protocol/index.ts";
import { DATA_BATCH_MAX_OPS } from "~/contract/artifacts.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "../keyedWrite.ts";
import { undoData, writeData, type DataVia } from "./api.ts";
import { dataRefusalText } from "./words.ts";

/** The packed form of a state-log position (`statelog.Position.Packed`): `(generation << 40) | seq`. */
export function packedPosition(p: LogPosition | undefined): number {
  if (!p) return 0;
  return p.generation * 2 ** 40 + p.seq;
}

/** What one collection holds on this tab. */
export interface CollectionView {
  name: string;
  /** Live documents, by id, in no particular order. */
  docs: ReadonlyMap<string, DataDoc>;
  status: "loading" | "loaded" | "failed";
  /** Why the last read failed, when it did. */
  error: QueryError | null;
}

/** How a write ended, as a page or a sheet acts on it. */
export type WriteResult =
  /** The engine applied every op. `seq` is the write's place in the store's history. */
  | { verdict: "applied"; seq: number; position: number }
  /** Durable, and not yet applied on the node that answered: the verdict follows the push. */
  | { verdict: "pending"; position: number }
  /**
   * The engine decided against it, in its words: whether the applier
   * decided it on the log or the node refused it before appending (a value
   * the grammar refuses, a share that writes nothing, a purged store) is
   * the engine's business, and the verdict is the same either way.
   */
  | {
      verdict: "conflict" | "invalid" | "forbidden" | "quota" | "gone";
      reason: string;
      conflict?: DataDoc[];
    }
  /** Nothing was decided: this node's write budget is spent, and the same write may be sent again after the wait. */
  | { verdict: "rate_limited"; reason: string; retryAfterMs?: number }
  /** Nobody could establish whether it landed; Retry sends exactly it again. */
  | { verdict: "unknown"; reason: string };

/** One cell a write sets, with the value drawn there until the engine answers. */
interface Cell {
  collection: string;
  doc: string;
  field: string;
  value: unknown;
}

/** A write this tab has made whose outcome is not known, kept for its Retry. */
interface Unsettled {
  request: KeyedRequest;
  cells: Cell[];
  /** Rows the write adds, drawn until the engine hands them back. */
  rows: { collection: string; doc: string }[];
}

/** What the store is built over. */
export interface DataStoreOptions {
  artifact: string;
  socket: LiveSocket;
  via: DataVia;
  /** Names this tab's session in its writes, so its own writes are recognised in the store's history. */
  clientID: string;
  /** Sends one keyed write; `keyedWrite` outside a suite. */
  write?: (req: KeyedRequest) => Promise<KeyedAnswer>;
  /** Mints an operation key for a new write. */
  newKey: () => string;
}

const cellKey = (collection: string, doc: string, field: string) =>
  `${collection}\u0000${doc}\u0000${field}`;

/**
 * What one push moved: the collections it named, or every collection (a
 * resync, a schema change, a store gone). What a reader that keeps no copy of
 * the rows (a dashboard's store datasets, read through the datasets route)
 * reads again on.
 */
export interface DataTouch {
  collections: ReadonlySet<string>;
  all: boolean;
}

export class ArtifactDataStore {
  readonly artifact: string;
  readonly pending = new SheetPendingEdits();
  private readonly socket: LiveSocket;
  private readonly via: DataVia;
  private readonly clientID: string;
  private readonly send: (req: KeyedRequest) => Promise<KeyedAnswer>;
  private readonly newKey: () => string;

  private schemaAnswer: DataSchemaAnswer | null = null;
  private schemaError: QueryError | null = null;
  private readonly collections = new Map<
    string,
    {
      docs: Map<string, DataDoc>;
      versions: Map<string, number>;
      status: CollectionView["status"];
      error: QueryError | null;
      through: number;
      reads: number;
    }
  >();
  /** The reader's own values, drawn over the documents until the engine's catch up. */
  private readonly overlay = new Map<string, { value: unknown; until: number }>();
  /**
   * Rows the reader added, by `collection\u0000doc`, drawn until the engine's
   * own document arrives: zero while the write is out, the write's position
   * once it is confirmed.
   */
  private readonly adding = new Map<string, number>();
  private readonly unsettled: Unsettled[] = [];
  /** The reader's own applied writes, newest last: what Mod+Z undoes. */
  private readonly undoable: number[] = [];
  /** The undos of those, newest last: what Mod+Shift+Z undoes again. */
  private readonly redoable: number[] = [];
  private readonly listeners = new Set<() => void>();
  private readonly touchListeners = new Set<(touch: DataTouch) => void>();
  private gone = false;
  private stopped = false;

  constructor(options: DataStoreOptions) {
    this.artifact = options.artifact;
    this.socket = options.socket;
    this.via = options.via;
    this.clientID = options.clientID;
    this.send = options.write ?? keyedWrite;
    this.newKey = options.newKey;
    this.pending.subscribe(() => this.emit());
  }

  // ---- reading -----------------------------------------------------------

  /** The store's collections the reader may see, its catalogue row and the reader's role. */
  get schema(): DataSchemaAnswer | null {
    return this.schemaAnswer;
  }

  /** Why the schema could not be read, when it could not. */
  get schemaFailure(): QueryError | null {
    return this.schemaError;
  }

  /** Whether the store was purged with its artifact. */
  get isGone(): boolean {
    return this.gone;
  }

  /** Read the schema, again where it was read before. */
  async loadSchema(): Promise<void> {
    try {
      const answer = await this.socket.query("artifact_data_schema", { id: this.artifact });
      if (this.stopped) return;
      this.schemaAnswer = answer;
      this.schemaError = null;
      // A COLLECTION THE SCHEMA NO LONGER HOLDS is dropped with its rows.
      const names = new Set(answer.collections.map((c) => c.name));
      for (const name of this.collections.keys())
        if (!names.has(name)) this.collections.delete(name);
    } catch (err) {
      if (this.stopped) return;
      this.schemaError = err instanceof QueryError ? err : new QueryError("query_failed");
    }
    this.emit();
  }

  /** One collection as this tab holds it, or null before it was asked for. */
  view(collection: string): CollectionView | null {
    const c = this.collections.get(collection);
    if (!c) return null;
    return { name: collection, docs: c.docs, status: c.status, error: c.error };
  }

  /**
   * A collection's documents with the reader's own values drawn over them:
   * what a sheet draws and what a page's subscription hands it.
   */
  docsOf(
    collection: string,
  ): { doc: DataDoc; fields: Record<string, unknown>; pending: boolean }[] {
    const c = this.collections.get(collection);
    if (!c) return [];
    const out: { doc: DataDoc; fields: Record<string, unknown>; pending: boolean }[] = [];
    for (const doc of c.docs.values()) {
      const fields: Record<string, unknown> = { ...(doc.fields ?? {}) };
      let pending = false;
      for (const field of Object.keys(fields)) {
        if (this.pending.isPending(doc.id, field)) pending = true;
      }
      for (const [key, entry] of this.overlay) {
        const [coll, id, field] = key.split("\u0000") as [string, string, string];
        if (coll !== collection || id !== doc.id) continue;
        if (entry.value === null) delete fields[field];
        else fields[field] = entry.value;
        if (this.pending.isPending(id, field)) pending = true;
      }
      out.push({ doc, fields, pending });
    }
    // A ROW THE READER ADDED that the engine has not handed back yet is
    // drawn too, with whatever the reader has typed into it, so Add row is
    // instant like an edit.
    const placed = new Set(out.map((d) => d.doc.id));
    for (const key of this.adding.keys()) {
      const [coll, id] = key.split("\u0000") as [string, string];
      if (coll !== collection || placed.has(id)) continue;
      const fields: Record<string, unknown> = {};
      for (const [cell, entry] of this.overlay) {
        const [c2, id2, field] = cell.split("\u0000") as [string, string, string];
        if (c2 === collection && id2 === id && entry.value !== null) fields[field] = entry.value;
      }
      out.push({
        doc: {
          collection,
          id,
          fields,
          version: 0,
          created_by: { name: "", kind: "" },
          created_at: "",
          updated_by: { name: "", kind: "" },
          updated_at: "",
        },
        fields,
        pending: true,
      });
    }
    return out;
  }

  /**
   * Read a collection whole, a page at a time: the first time it is asked for,
   * and again after a resync. Concurrent reads of one collection are one.
   */
  async load(collection: string): Promise<void> {
    let c = this.collections.get(collection);
    if (!c) {
      c = {
        docs: new Map(),
        versions: new Map(),
        status: "loading",
        error: null,
        through: 0,
        reads: 0,
      };
      this.collections.set(collection, c);
    }
    const read = ++c.reads;
    c.status = c.docs.size > 0 ? c.status : "loading";
    this.emit();
    const docs = new Map<string, DataDoc>();
    const versions = new Map<string, number>();
    let cursor = "";
    // THE LOWEST `through` ANY PAGE ANSWERED: a page read later can carry a
    // newer version than the documents an earlier page already passed, and
    // the catch-up from it would skip what changed under them.
    let through = Number.POSITIVE_INFINITY;
    try {
      for (;;) {
        const page = await this.socket.query("artifact_data", {
          id: this.artifact,
          collection,
          limit: 500,
          ...(cursor ? { cursor } : {}),
        });
        if (this.stopped || c.reads !== read) return;
        for (const doc of page.docs) {
          versions.set(doc.id, doc.version);
          if (!doc.deleted) docs.set(doc.id, doc);
        }
        through = Math.min(through, page.through);
        if (!page.after) break;
        cursor = page.after;
      }
    } catch (err) {
      if (this.stopped || c.reads !== read) return;
      c.status = "failed";
      c.error = err instanceof QueryError ? err : new QueryError("query_failed");
      this.emit();
      // A FAULT OF THE DASHBOARD'S OWN is not the engine's: the view says the
      // read failed, and the error still reaches the console rather than
      // passing for a refusal (a page whose `docs` came back null did).
      if (!(err instanceof QueryError)) throw err;
      return;
    }
    // WHAT A PUSH ALREADY BROUGHT NEWER is kept over what the read saw: the
    // read and the pushes converge by version, never by arrival.
    for (const [id, version] of c.versions) {
      if ((versions.get(id) ?? -1) >= version) continue;
      versions.set(id, version);
      const held = c.docs.get(id);
      if (held) docs.set(id, held);
      else docs.delete(id);
    }
    c.docs = docs;
    c.versions = versions;
    // A WHOLE READ REPLACES WHAT WAS HELD, so its `through` replaces the
    // held one too, never ratchets it: a higher one kept from before would
    // skip what this read did not see.
    c.through = through;
    c.status = "loaded";
    c.error = null;
    this.settleOverlay();
    this.emit();
  }

  /** Read what changed in a loaded collection since what it holds, or whole where the engine says a catch-up cannot. */
  async catchUp(collection: string): Promise<void> {
    const c = this.collections.get(collection);
    if (!c || c.status !== "loaded" || c.through <= 0) return this.load(collection);
    const since = c.through;
    let cursor = "";
    try {
      let through = since;
      for (;;) {
        const page = await this.socket.query("artifact_data", {
          id: this.artifact,
          collection,
          since,
          limit: 500,
          ...(cursor ? { cursor } : {}),
        });
        if (this.stopped) return;
        if (page.resync) return this.load(collection);
        for (const doc of page.docs) this.applyDoc(collection, doc);
        through = page.through;
        if (!page.after) break;
        cursor = page.after;
      }
      // ADOPTED ONLY ONCE THE LAST PAGE IS IN: a catch-up that fails part
      // way asks again from where it started, never past rows it never read.
      c.through = Math.max(c.through, through);
    } catch (err) {
      if (this.stopped) return;
      c.error = err instanceof QueryError ? err : new QueryError("query_failed");
    }
    this.settleOverlay();
    this.emit();
  }

  /** One document read again, for a write whose push did not arrive. */
  async refreshDoc(collection: string, id: string): Promise<void> {
    try {
      const doc = await this.socket.query("artifact_data_doc", {
        id: this.artifact,
        collection,
        doc: id,
      });
      if (this.stopped) return;
      this.applyDoc(collection, doc);
    } catch (err) {
      if (this.stopped) return;
      // A DOCUMENT NOTHING HOLDS any more was deleted: forget it.
      if (err instanceof QueryError && err.message === "not_found") {
        const c = this.collections.get(collection);
        c?.docs.delete(id);
      }
    }
    this.settleOverlay();
    this.emit();
  }

  /**
   * The room's join answer: on the first, nothing is held and every asked
   * collection loads; on a reconnect, a catch-up from what each holds, or a
   * whole read where the engine says the pushes this tab missed cannot be
   * caught up.
   */
  joined(answer: ArtifactJoined): void {
    void this.loadSchema();
    for (const name of this.collections.keys()) {
      void (answer.resync ? this.load(name) : this.catchUp(name));
    }
  }

  /** One `artifact_data` push, as this reader may see it. */
  applyPush(push: ArtifactDataPush): void {
    this.touched(push);
    if (push.gone) {
      this.gone = true;
      this.emit();
      return;
    }
    if (push.schema) void this.loadSchema();
    if (push.resync) {
      for (const name of this.collections.keys()) void this.load(name);
      return;
    }
    if (push.reread) {
      // TOO LARGE TO CARRY: the keys name what moved, and each collection
      // they touch catches up from what it holds.
      const touched = new Set((push.keys ?? []).map((k) => k.collection));
      for (const name of touched) if (this.collections.has(name)) void this.catchUp(name);
      return;
    }
    for (const delta of push.deltas ?? []) {
      const c = this.collections.get(delta.collection);
      if (!c) continue;
      this.applyDoc(delta.collection, {
        collection: delta.collection,
        id: delta.doc,
        ...(delta.body ? { fields: delta.body } : {}),
        ...(delta.deleted ? { deleted: true } : {}),
        version: delta.version,
        created_by: c.docs.get(delta.doc)?.created_by ?? { name: delta.created_by, kind: "" },
        created_at: c.docs.get(delta.doc)?.created_at ?? delta.updated_at,
        updated_by: delta.updated_by,
        updated_at: delta.updated_at,
      });
    }
    if (typeof push.through === "number") {
      for (const c of this.collections.values()) {
        if (c.status === "loaded") c.through = Math.max(c.through, push.through);
      }
    }
    this.settleOverlay();
    this.emit();
  }

  private applyDoc(collection: string, doc: DataDoc): void {
    const c = this.collections.get(collection);
    if (!c) return;
    if ((c.versions.get(doc.id) ?? -1) >= doc.version) return;
    c.versions.set(doc.id, doc.version);
    if (doc.deleted) c.docs.delete(doc.id);
    else c.docs.set(doc.id, doc);
  }

  /** Drops each overlay value, and each added row, the engine's own document has caught up with. */
  private settleOverlay(): void {
    const versionOf = (collection: string, id: string) =>
      this.collections.get(collection)?.versions.get(id) ?? 0;
    for (const [key, entry] of this.overlay) {
      if (entry.until <= 0) continue;
      const [collection, id] = key.split("\u0000") as [string, string];
      if (versionOf(collection, id) >= entry.until) this.overlay.delete(key);
    }
    for (const [key, until] of this.adding) {
      if (until <= 0) continue;
      const [collection, id] = key.split("\u0000") as [string, string];
      if (versionOf(collection, id) >= until) this.adding.delete(key);
    }
  }

  // ---- writing -----------------------------------------------------------

  /**
   * A sheet's edits as one write: each cell a `merge` of its one field, a
   * cleared cell a null (which removes the field). Past the engine's ops in
   * one write (a large paste), the edits go as several writes, each all or
   * nothing, in order.
   */
  async edit(collection: string, edits: readonly SheetEdit[]): Promise<void> {
    const byDoc = new Map<string, Record<string, unknown>>();
    for (const edit of edits) {
      const fields = byDoc.get(edit.rowId) ?? {};
      fields[edit.field] = edit.value;
      byDoc.set(edit.rowId, fields);
    }
    // LONG TEXT IS WRITTEN GUARDED (D18): replacing a long-text value names
    // the version of the row the reader edited, so a concurrent edit is a
    // conflict they are told about rather than a paragraph silently lost.
    // Without it the engine refuses every such edit.
    const longText = new Set(
      (this.schemaAnswer?.collections.find((c) => c.name === collection)?.columns ?? [])
        .filter((c) => c.type === "longtext")
        .map((c) => c.key),
    );
    const versions = this.collections.get(collection)?.versions;
    const ops: DataOp[] = [...byDoc].map(([doc, fields]) => {
      const version = versions?.get(doc);
      const guarded =
        version !== undefined && Object.keys(fields).some((field) => longText.has(field));
      return {
        op: "merge",
        collection,
        doc,
        fields,
        ...(guarded ? { if_version: version } : {}),
      };
    });
    for (let i = 0; i < ops.length; i += DATA_BATCH_MAX_OPS) {
      await this.write(ops.slice(i, i + DATA_BATCH_MAX_OPS));
    }
  }

  /** A new row with these fields, under an id this tab mints. */
  async addRow(collection: string, fields: Record<string, unknown> = {}): Promise<string> {
    const id = rowID();
    await this.write([{ op: "set", collection, doc: id, fields }]);
    return id;
  }

  /** Remove rows: each a tombstone, so a client catching up sees the delete. */
  async deleteRows(collection: string, ids: readonly string[]): Promise<WriteResult | null> {
    let last: WriteResult | null = null;
    const ops: DataOp[] = ids.map((doc) => ({ op: "delete", collection, doc }));
    for (let i = 0; i < ops.length; i += DATA_BATCH_MAX_OPS) {
      last = await this.write(ops.slice(i, i + DATA_BATCH_MAX_OPS));
    }
    return last;
  }

  /**
   * One batch of ops, drawn at once and confirmed by the engine. Resolves with
   * how it ended; it rejects only for a write nobody sent.
   */
  async write(ops: DataOp[]): Promise<WriteResult> {
    const cells = cellsOf(ops);
    const rows = ops
      .filter((op) => op.op === "set" && !this.collections.get(op.collection)?.versions.has(op.doc))
      .map((op) => ({ collection: op.collection, doc: op.doc }));
    const deleted = ops.filter((op) => op.op === "delete");
    for (const cell of cells) {
      this.overlay.set(cellKey(cell.collection, cell.doc, cell.field), {
        value: cell.value,
        until: 0,
      });
    }
    for (const row of rows) this.adding.set(`${row.collection}\u0000${row.doc}`, 0);
    const request: KeyedRequest = {
      ...writeData(this.artifact, ops, this.clientID, this.via),
      key: this.newKey(),
    };
    const result = await this.send_(request, cells, rows, true);
    // A DELETE IS DRAWN when the engine has it: a row that vanished on the
    // press and came back on a refusal reads as a glitch, and the confirmed
    // delete is the push that follows.
    if (result.verdict === "applied" && deleted.length > 0) {
      for (const op of deleted) void this.refreshDoc(op.collection, op.doc);
    }
    return result;
  }

  /** Every write whose outcome nobody could establish, sent again exactly, under its own key. */
  async retry(): Promise<void> {
    const again = this.unsettled.splice(0);
    this.emit();
    for (const u of again) await this.send_(u.request, u.cells, u.rows, false);
  }

  /** Writes waiting on a Retry. */
  get unsettledCount(): number {
    return this.unsettled.length;
  }

  /** Sends one write and settles everything it drew. `record` puts an applied write on the undo stack. */
  private async send_(
    request: KeyedRequest,
    cells: Cell[],
    rows: { collection: string; doc: string }[],
    record: boolean,
  ): Promise<WriteResult> {
    const settles = cells.map((c) => this.pending.begin(c.doc, c.field));
    this.emit();
    const answer = await this.send(request);
    if (this.stopped) return resultOf(answer);
    const result = resultOf(answer);
    switch (result.verdict) {
      case "applied":
        this.confirmed(cells, rows, result.position);
        for (const settle of settles) settle({ ok: true });
        if (record) {
          this.undoable.push(result.seq);
          this.redoable.length = 0;
        }
        this.settleOverlay();
        this.confirmLater(cells, rows, result.position);
        break;
      case "pending":
        // DURABLE, AND THE VERDICT IS ON ITS POSITION: the cells stay pending
        // until the store's history says what the applier decided.
        void this.verdictAt(request, cells, rows, settles, result.position, record);
        break;
      case "unknown":
        // KEPT, VALUES AND ALL, for a Retry under the same key, which the
        // engine applies once whether or not the first attempt landed.
        this.unsettled.push({ request, cells, rows });
        for (const settle of settles) settle({ ok: false, message: result.reason });
        break;
      default:
        this.reverted(cells, rows);
        for (const settle of settles) settle({ ok: false, message: result.reason });
    }
    this.emit();
    return result;
  }

  /** A write the engine applied: its overlay stays until the documents reach its position. */
  private confirmed(
    cells: Cell[],
    rows: { collection: string; doc: string }[],
    position: number,
  ): void {
    for (const cell of cells) {
      const entry = this.overlay.get(cellKey(cell.collection, cell.doc, cell.field));
      if (entry) entry.until = position;
    }
    for (const row of rows) {
      const key = `${row.collection}\u0000${row.doc}`;
      if (this.adding.has(key)) this.adding.set(key, position);
    }
  }

  /** A write the engine refused: everything it drew goes, and the cells show what the engine holds. */
  private reverted(cells: Cell[], rows: { collection: string; doc: string }[]): void {
    for (const cell of cells) this.overlay.delete(cellKey(cell.collection, cell.doc, cell.field));
    for (const row of rows) this.adding.delete(`${row.collection}\u0000${row.doc}`);
  }

  /**
   * A confirmed write whose push has not come in a few seconds (a socket that
   * dropped as it was answered) reads the documents it wrote, so its overlay
   * gives way to the engine's body rather than standing in for it.
   */
  private confirmLater(
    cells: { collection: string; doc: string }[],
    rows: { collection: string; doc: string }[],
    position: number,
  ): void {
    setTimeout(() => {
      if (this.stopped) return;
      const docs = new Set([...cells, ...rows].map((c) => `${c.collection}\u0000${c.doc}`));
      for (const key of docs) {
        const [collection, doc] = key.split("\u0000") as [string, string];
        const version = this.collections.get(collection)?.versions.get(doc) ?? 0;
        if (version < position) void this.refreshDoc(collection, doc);
      }
    }, CONFIRM_WAIT_MS);
  }

  /** The applier's verdict on a write accepted and not yet applied where it was answered. */
  private async verdictAt(
    request: KeyedRequest,
    cells: Cell[],
    rows: { collection: string; doc: string }[],
    settles: ((outcome: SheetSettle) => void)[],
    position: number,
    record: boolean,
  ): Promise<void> {
    const opID = request.key ?? "";
    const first = cells[0] ?? rows[0];
    for (let attempt = 0; attempt < VERDICT_ATTEMPTS && !this.stopped; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, CONFIRM_WAIT_MS));
      if (this.stopped) return;
      let entry: DataHistoryEntry | undefined;
      try {
        const page = await this.socket.query("artifact_data_history", {
          id: this.artifact,
          ...(first ? { collection: first.collection, doc: first.doc } : {}),
          limit: 50,
        });
        entry = page.entries.find((e) => e.op_id === opID);
      } catch {
        // THE HISTORY COULD NOT BE READ THIS TIME: the next attempt asks
        // again, and after the last the write is offered for a Retry.
        continue;
      }
      if (!entry) continue;
      if (entry.verdict === "applied") {
        this.confirmed(cells, rows, position);
        for (const settle of settles) settle({ ok: true });
        if (record) {
          this.undoable.push(entry.seq);
          this.redoable.length = 0;
        }
        for (const doc of new Set(
          [...cells, ...rows].map((c) => `${c.collection}\u0000${c.doc}`),
        )) {
          const [collection, id] = doc.split("\u0000") as [string, string];
          void this.refreshDoc(collection, id);
        }
      } else {
        this.reverted(cells, rows);
        for (const settle of settles)
          settle({ ok: false, message: dataRefusalText(entry.verdict) });
      }
      this.emit();
      return;
    }
    if (this.stopped) return;
    // NO VERDICT CAME BACK in the time this tab waits: the write is kept for
    // a Retry, which the engine collapses onto the first if it landed.
    this.unsettled.push({ request, cells, rows });
    for (const settle of settles)
      settle({ ok: false, message: "This change could not be confirmed. Retry it." });
    this.emit();
  }

  /** Undo the reader's own last write on this store, by its place in the history. */
  async undo(): Promise<WriteResult | null> {
    const seq = this.undoable.pop();
    if (seq === undefined) return null;
    const result = resultOf(
      await this.send({
        ...undoData(this.artifact, seq, this.clientID, this.via),
        key: this.newKey(),
      }),
    );
    if (result.verdict === "applied") this.redoable.push(result.seq);
    // NOTHING WAS DECIDED, so the step is the reader's to take again.
    else if (undecided(result)) this.undoable.push(seq);
    this.refreshAll();
    return result;
  }

  /** Undo the reader's last undo. */
  async redo(): Promise<WriteResult | null> {
    const seq = this.redoable.pop();
    if (seq === undefined) return null;
    const result = resultOf(
      await this.send({
        ...undoData(this.artifact, seq, this.clientID, this.via),
        key: this.newKey(),
      }),
    );
    if (result.verdict === "applied") this.undoable.push(result.seq);
    else if (undecided(result)) this.redoable.push(seq);
    this.refreshAll();
    return result;
  }

  private refreshAll(): void {
    for (const name of this.collections.keys()) void this.catchUp(name);
  }

  // ---- listening ---------------------------------------------------------

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => void this.listeners.delete(listener);
  }

  /**
   * Every push's [DataTouch], whether or not this store holds the
   * collections it names: a dashboard reads its store datasets through the
   * engine, as its viewer, and only needs to know when to read them again.
   */
  onTouch(listener: (touch: DataTouch) => void): () => void {
    this.touchListeners.add(listener);
    return () => void this.touchListeners.delete(listener);
  }

  private touched(push: ArtifactDataPush): void {
    if (this.touchListeners.size === 0) return;
    const collections = new Set<string>();
    for (const d of push.deltas ?? []) collections.add(d.collection);
    for (const k of push.keys ?? []) collections.add(k.collection);
    const touch: DataTouch = {
      collections,
      all: push.gone === true || push.resync === true || push.schema === true,
    };
    if (!touch.all && collections.size === 0) return;
    for (const listener of [...this.touchListeners]) listener(touch);
  }

  /** Stop: nothing read or written after this changes anything. */
  stop(): void {
    this.stopped = true;
    this.listeners.clear();
    this.touchListeners.clear();
  }

  private emit(): void {
    for (const listener of [...this.listeners]) listener();
  }
}

/** A write the engine decided nothing about: a spent budget, or an answer nobody could establish. */
function undecided(result: WriteResult): boolean {
  return result.verdict === "rate_limited" || result.verdict === "unknown";
}

/** How long a confirmed write waits for its push before reading what it wrote. */
const CONFIRM_WAIT_MS = 3_000;

/** How many times a pending write's verdict is looked for in the history before Retry is offered. */
const VERDICT_ATTEMPTS = 5;

/** The cells a batch of ops writes, with the value each draws until the engine answers. */
function cellsOf(ops: readonly DataOp[]): Cell[] {
  const out: Cell[] = [];
  for (const op of ops) {
    if ((op.op === "merge" || op.op === "set") && op.fields) {
      for (const [field, value] of Object.entries(op.fields)) {
        out.push({ collection: op.collection, doc: op.doc, field, value });
      }
    }
  }
  return out;
}

/** A new row's id: random, and within the document grammar. */
export function rowID(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** A keyed answer as the write it was. */
export function resultOf(answer: KeyedAnswer): WriteResult {
  if (answer.kind === "done") {
    const body = answer.body as Partial<DataWriteAnswer>;
    const position = packedPosition(body.position);
    if (answer.pending || body.verdict === "pending") return { verdict: "pending", position };
    return { verdict: "applied", seq: body.seq ?? 0, position };
  }
  if (answer.kind === "unknown") return { verdict: "unknown", reason: answer.text };
  const err = answer.error;
  if (!err) return { verdict: "invalid", reason: answer.text };
  if (err.status === 429) {
    return {
      verdict: "rate_limited",
      reason: refusedReason(err, answer.text),
      ...(err.retryAfterSeconds !== null ? { retryAfterMs: err.retryAfterSeconds * 1000 } : {}),
    };
  }
  // THE APPLIER'S VERDICT IN THE READER'S WORDS. Its own detail names the
  // rule for the agent tools (op indices, `if_version`, field keys), which a
  // person can act on none of; a refusal before the log keeps the sheet's
  // own sentence, or the dashboard's reading of the engine's code.
  const verdict = decidedVerdict(err);
  const reason =
    err.body.verdict !== undefined ? dataRefusalText(verdict) : refusedReason(err, answer.text);
  const conflict = Array.isArray(err.body.conflict) ? (err.body.conflict as DataDoc[]) : undefined;
  return { verdict, reason, ...(conflict ? { conflict } : {}) };
}

/** The decided verdicts, as the applier writes them. */
const DECIDED = ["conflict", "invalid", "forbidden", "quota", "gone"] as const;
type Decided = (typeof DECIDED)[number];

/**
 * The verdict a refusal is: the applier's own where the answer carries it,
 * else what the node's refusal before the log means. ONE ANSWER PER MEANING,
 * whichever side of the log decided it: a value the grammar refuses at the
 * edge and one the applier refuses are both `invalid`, so neither a page nor
 * the sheet handles the same refusal two ways. A refusal this build cannot
 * place is `invalid` with the engine's own words, which is how the contract
 * asks an unknown code to be taken.
 */
function decidedVerdict(err: RestError): Decided {
  const said = err.body.verdict;
  if (typeof said === "string" && (DECIDED as readonly string[]).includes(said))
    return said as Decided;
  switch (err.status) {
    case 401:
    case 403:
      return "forbidden";
    case 409:
      // ONE STATUS, FOUR MEANINGS, told apart by the code the engine gives
      // each: the company at its limit, an artifact in the trash (whose data
      // is not written until it is restored), an undo the history cannot
      // make, and somebody else's change.
      switch (err.code) {
        case "quota_exceeded":
          return "quota";
        case "artifact_trashed":
          return "gone";
        case "not_undoable":
          return "invalid";
        default:
          return "conflict";
      }
    case 410:
      return "gone";
    default:
      return "invalid";
  }
}

/**
 * What an undo or a redo the engine did not take says, or null for one it
 * took. Only a CONFLICT is somebody else's change overtaking it; every other
 * refusal (a spent budget, a write the history no longer holds, an answer
 * nobody could establish) says its own reason rather than blaming a
 * collaborator.
 */
export function stepRefusal(result: WriteResult, done: "undone" | "redone"): string | null {
  switch (result.verdict) {
    case "applied":
    case "pending":
      return null;
    case "conflict":
      return `That change was overtaken by another and cannot be ${done}.`;
    default:
      return result.reason;
  }
}

/** What a write refused before the log says on the cell. */
function refusedReason(err: RestError, fallback: string): string {
  if (err.status === 429) return "Too many changes at once. Wait a moment, then try again.";
  if (err.status === 403) return "You can view this but not change it.";
  // THE TRASH IS A STATE AND A PURGE IS FOR GOOD, and the engine answers them
  // apart (409 `artifact_trashed`, 410 `data_gone`): read as one, a person was
  // told a deleted sheet was in a trash nobody could restore it from.
  if (err.status === 409 && err.code === "artifact_trashed")
    return "This artifact is in the trash, so nothing can be changed.";
  if (err.status === 410) return "This artifact was deleted, so nothing can be changed.";
  return fallback;
}
