/**
 * The host end of the page bridge: what the viewer does with every call an
 * artifact page's runtime makes over its port.
 *
 * ONE PAGE, ONE ARTIFACT, ONE VERSION. A host is built for the frame it
 * embedded and binds every call to that artifact and version: a page never
 * names its own, so it cannot reach another artifact's data or room. Each
 * call is held to the capabilities the version declares (a fast, readable
 * refusal), then made to the engine as the signed-in viewer, which decides it
 * again against the viewer's share and the collection's rules: the real
 * boundary, so a page gains nothing the person could not do in the dashboard.
 *
 * NOTHING ARRIVING ON THE PORT IS TRUSTED. A call is read by the bridge's own
 * reader (`runtime/bridge.ts`), its parameters checked here, and anything
 * that is not exactly a call is dropped.
 */

import { EXTERNAL_URL_MAX, externalUrl } from "@crewlethq/ui";
import { QueryError, type DataDoc, type DataOp, type LiveSocket } from "~/protocol/index.ts";
import { RUNTIME_CONTRACT, STATE_COLLECTION } from "~/contract/artifacts.ts";
import {
  BRIDGE_MESSAGE,
  capabilityOf,
  readCall,
  type BridgeCall,
  type BridgeConnection,
  type BridgeDoc,
  type BridgeErrorCode,
  type BridgeRoster,
  type BridgeViewer,
  type BridgeWriteOutcome,
  type RuntimeCapability,
} from "~/runtime/bridge.ts";
import type { ArtifactDataStore, WriteResult } from "~/lib/artifacts/datastore.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "~/lib/keyedWrite.ts";
import { writeState } from "~/lib/artifacts/api.ts";

/** What a host is built over. */
export interface PageHostOptions {
  artifact: string;
  version: number;
  /** What the version declares: the capabilities the page may use. */
  capabilities: readonly RuntimeCapability[];
  socket: LiveSocket;
  /**
   * The artifact's datastore as this page writes it (`via: bridge`, this
   * version), or null for a version that declared neither `db` nor `state`,
   * whose every data call is refused on the capability first.
   */
  store: ArtifactDataStore | null;
  clientID: string;
  newKey: () => string;
  /** Asks the reader whether to open a link, through the interstitial; resolves with their answer. */
  openLink: (url: URL) => Promise<boolean>;
  /** Says where the reader is in the room. */
  setPresence: (
    activity: "viewing" | "editing" | "idle",
    focus: BridgeRoster["members"][number]["focus"],
  ) => void;
  /** Sends one keyed write; `keyedWrite` outside a suite. */
  write?: (req: KeyedRequest) => Promise<KeyedAnswer>;
}

/** A refusal a call is answered with. */
class Refusal extends Error {
  constructor(
    readonly code: BridgeErrorCode,
    message: string,
  ) {
    super(message);
  }
}

/** The engine's query grammar's operators, and the page contract's names for the two it spells longer. */
const QUERY_OPS: Readonly<Record<string, string>> = {
  eq: "eq",
  ne: "ne",
  lt: "lt",
  lte: "le",
  gt: "gt",
  gte: "ge",
  in: "in",
  contains: "contains",
  prefix: "prefix",
};

/** A page's query (`{where: [[field, op, value]], order: [[field, dir]], limit, cursor}`) in the engine's grammar. */
export function engineQuery(q: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const where = q["where"];
  if (where !== undefined) {
    if (!Array.isArray(where))
      throw new Refusal("invalid", "where is a list of [field, op, value]");
    out["where"] = where.map((clause) => {
      if (!Array.isArray(clause) || clause.length !== 3 || typeof clause[0] !== "string") {
        throw new Refusal("invalid", "each where clause is [field, op, value]");
      }
      const op = typeof clause[1] === "string" ? QUERY_OPS[clause[1]] : undefined;
      if (!op) {
        throw new Refusal(
          "invalid",
          `op ${String(clause[1])} is not one of ${Object.keys(QUERY_OPS).join(", ")}`,
        );
      }
      if (op === "in") {
        if (!Array.isArray(clause[2]))
          throw new Refusal("invalid", "an `in` clause takes a list of values");
        return { path: clause[0], op, values: clause[2] };
      }
      return { path: clause[0], op, value: clause[2] };
    });
  }
  const order = q["order"];
  if (order !== undefined) {
    // ONE FIELD, as the engine's field index orders by; the document id is
    // always the tie-break, so the order is total and a cursor exact.
    if (!Array.isArray(order) || order.length > 1) {
      throw new Refusal("invalid", 'order names one [field, "asc" | "desc"] in contract 1.0');
    }
    const [first] = order as unknown[];
    if (first !== undefined) {
      if (
        !Array.isArray(first) ||
        typeof first[0] !== "string" ||
        (first[1] !== "asc" && first[1] !== "desc")
      ) {
        throw new Refusal("invalid", 'order names one [field, "asc" | "desc"]');
      }
      out["order_by"] = { path: first[0], ...(first[1] === "desc" ? { desc: true } : {}) };
    }
  }
  if (q["limit"] !== undefined) {
    if (typeof q["limit"] !== "number" || !Number.isInteger(q["limit"])) {
      throw new Refusal("invalid", "limit is a whole number");
    }
    out["limit"] = q["limit"];
  }
  if (q["cursor"] !== undefined) {
    if (typeof q["cursor"] !== "string")
      throw new Refusal("invalid", "cursor is the one an answer gave");
    out["after"] = q["cursor"];
  }
  return out;
}

/** A document as a page reads it. */
export function bridgeDoc(
  doc: DataDoc,
  fields: Record<string, unknown> = doc.fields ?? {},
  pending = false,
): BridgeDoc {
  return {
    id: doc.id,
    data: fields,
    version: doc.version,
    createdBy: doc.created_by.name,
    createdAt: doc.created_at,
    updatedBy: doc.updated_by.name,
    updatedAt: doc.updated_at,
    pending,
  };
}

/** A write's result as a page handles it. */
export function bridgeOutcome(result: WriteResult): BridgeWriteOutcome {
  switch (result.verdict) {
    case "applied":
      return { verdict: "applied", seq: result.seq };
    case "pending":
      return { verdict: "pending" };
    case "unknown":
      throw new Refusal("unavailable", result.reason);
    case "refused":
      throw new Refusal(
        result.code === "write_budget"
          ? "rate_limited"
          : result.code === "forbidden"
            ? "forbidden"
            : "invalid",
        result.reason,
      );
    default:
      return {
        verdict: result.verdict,
        reason: result.reason,
        ...(result.conflict ? { conflict: result.conflict.map((d) => bridgeDoc(d)) } : {}),
      };
  }
}

/** A query error as the refusal a page is answered with. */
function refusalOf(err: unknown): Refusal {
  if (err instanceof Refusal) return err;
  if (err instanceof QueryError) {
    switch (err.message) {
      case "not_found":
        return new Refusal("not_found", "there is no such collection, document or asset");
      case "unauthorized":
        return new Refusal("forbidden", "your share does not allow this");
      case "bad_params":
        return new Refusal("invalid", err.detail ?? "the call's arguments were refused");
      default:
        return new Refusal("unavailable", "the engine could not be reached; nothing changed");
    }
  }
  return new Refusal("unavailable", "the engine could not be reached; nothing changed");
}

const str = (v: unknown, what: string): string => {
  if (typeof v !== "string" || v === "")
    throw new Refusal("invalid", `${what} is a non-empty string`);
  return v;
};

/** The host for one embedded page. */
export class PageHost {
  private port: MessagePort | null = null;
  private viewer: BridgeViewer | null = null;
  private connection: BridgeConnection = "live";
  private roster: BridgeRoster = { members: [], truncated: false };
  private nextSubscription = 1;
  private readonly subscriptions = new Map<
    number,
    { collection: string; sent: Map<string, string> }
  >();
  private readonly stopStore: () => void;
  private readonly send: (req: KeyedRequest) => Promise<KeyedAnswer>;

  constructor(private readonly options: PageHostOptions) {
    this.send = options.write ?? keyedWrite;
    this.stopStore = options.store ? options.store.subscribe(() => this.pushData()) : () => {};
  }

  /** The page said hello and holds its port: tell it what it may do and who is viewing. */
  attach(port: MessagePort): void {
    this.port = port;
    this.subscriptions.clear();
    port.postMessage({
      type: BRIDGE_MESSAGE.init,
      contract: RUNTIME_CONTRACT,
      capabilities: [...this.options.capabilities],
      viewer: this.viewer,
      connection: this.connection,
    });
    this.pushPresence();
  }

  /** The frame is gone: nothing more is said to it. */
  detach(): void {
    this.port = null;
    this.subscriptions.clear();
  }

  /** Stop: the frame is unmounted. */
  stop(): void {
    this.detach();
    this.stopStore();
  }

  /** Who is viewing changed (a role moved with a share). */
  setViewer(viewer: BridgeViewer | null): void {
    const changed = JSON.stringify(viewer) !== JSON.stringify(this.viewer);
    this.viewer = viewer;
    if (changed) this.event({ topic: "identity", viewer });
  }

  /** The link to the engine moved. */
  setConnection(state: BridgeConnection): void {
    if (state === this.connection) return;
    this.connection = state;
    this.event({ topic: "connection", state });
  }

  /** The room moved. */
  setRoster(roster: BridgeRoster): void {
    this.roster = roster;
    this.pushPresence();
  }

  private pushPresence(): void {
    if (!this.options.capabilities.includes("presence")) return;
    this.event({ topic: "presence", roster: this.roster });
  }

  private event(payload: Record<string, unknown>): void {
    this.port?.postMessage({ type: BRIDGE_MESSAGE.event, ...payload });
  }

  /** One message from the page, which may be anything at all. */
  async receive(data: unknown, port: MessagePort): Promise<void> {
    const call = readCall(data);
    if (!call) return;
    try {
      const value = await this.answer(call);
      port.postMessage({
        type: BRIDGE_MESSAGE.result,
        id: call.id,
        ok: true,
        value: value ?? null,
      });
    } catch (err) {
      const refusal = refusalOf(err);
      port.postMessage({
        type: BRIDGE_MESSAGE.result,
        id: call.id,
        ok: false,
        error: { code: refusal.code, message: refusal.message },
      });
    }
  }

  private async answer(call: BridgeCall): Promise<unknown> {
    const needs = capabilityOf(call.method);
    if (needs && !this.options.capabilities.includes(needs)) {
      throw new Refusal(
        "capability_not_declared",
        `this page's version does not declare the \`${needs}\` capability`,
      );
    }
    const { artifact, socket } = this.options;
    const store = this.options.store;
    const p = call.params;
    const needStore = (): ArtifactDataStore => {
      if (!store)
        throw new Refusal("capability_not_declared", "this page's version declares no datastore");
      return store;
    };
    switch (call.method) {
      case "identity.me":
        if (!this.viewer) throw new Refusal("unavailable", "who is viewing is not known yet");
        return this.viewer;
      case "data.schema": {
        const held = needStore();
        if (!held.schema) await held.loadSchema();
        if (!held.schema) throw refusalOf(held.schemaFailure);
        return held.schema.collections.filter((c) => c.name !== STATE_COLLECTION);
      }
      case "data.get": {
        try {
          const doc = await socket.query("artifact_data_doc", {
            id: artifact,
            collection: str(p["collection"], "collection"),
            doc: str(p["id"], "id"),
          });
          return doc.deleted ? null : bridgeDoc(doc);
        } catch (err) {
          if (err instanceof QueryError && err.message === "not_found") return null;
          throw err;
        }
      }
      case "data.list": {
        const page = await socket.query("artifact_data", {
          id: artifact,
          collection: str(p["collection"], "collection"),
          ...(typeof p["cursor"] === "string" ? { cursor: p["cursor"] } : {}),
          ...(typeof p["limit"] === "number" ? { limit: p["limit"] } : {}),
        });
        return {
          docs: page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d)),
          ...(page.after ? { cursor: page.after } : {}),
        };
      }
      case "data.query": {
        const q = p["query"];
        if (typeof q !== "object" || q === null || Array.isArray(q))
          throw new Refusal("invalid", "query is an object");
        const page = await socket.query("artifact_data_query", {
          id: artifact,
          collection: str(p["collection"], "collection"),
          q: JSON.stringify(engineQuery(q as Record<string, unknown>)),
        });
        return {
          docs: page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d)),
          ...(page.after ? { cursor: page.after } : {}),
        };
      }
      case "data.subscribe": {
        const collection = str(p["collection"], "collection");
        if (collection === STATE_COLLECTION)
          throw new Refusal("not_found", "there is no such collection");
        const held = needStore();
        await held.load(collection);
        const view = held.view(collection);
        if (view?.status === "failed") throw refusalOf(view.error);
        const subscription = this.nextSubscription++;
        const docs = held.docsOf(collection);
        const sent = new Map(
          docs.map((d) => [d.doc.id, signature(d.doc.version, d.pending, d.fields)]),
        );
        this.subscriptions.set(subscription, { collection, sent });
        return { subscription, docs: docs.map((d) => bridgeDoc(d.doc, d.fields, d.pending)) };
      }
      case "data.unsubscribe": {
        if (typeof p["subscription"] === "number") this.subscriptions.delete(p["subscription"]);
        return null;
      }
      case "data.write": {
        const ops = p["ops"];
        if (!Array.isArray(ops) || ops.length === 0)
          throw new Refusal("invalid", "a write carries at least one op");
        if (
          ops.some(
            (op) =>
              typeof op !== "object" ||
              op === null ||
              (op as { collection?: unknown }).collection === STATE_COLLECTION,
          )
        ) {
          throw new Refusal(
            "invalid",
            "each op is an object naming a collection of the page's datastore",
          );
        }
        // EACH OP IS THE ENGINE'S TO HOLD to the grammar, the collection's
        // rules and this version's declared capabilities; what is checked
        // here is only that none reaches the per-viewer state collection.
        return bridgeOutcome(await needStore().write(ops as DataOp[]));
      }
      case "state.get": {
        const key = str(p["key"], "key");
        const answer = await socket.query("artifact_data_state", { id: artifact });
        return answer.state.find((e) => e.key === key)?.value ?? null;
      }
      case "state.list": {
        const prefix = typeof p["prefix"] === "string" ? p["prefix"] : "";
        const answer = await socket.query("artifact_data_state", { id: artifact });
        return answer.state
          .map((e) => e.key)
          .filter((k) => k.startsWith(prefix))
          .sort();
      }
      case "state.set":
      case "state.delete": {
        const key = str(p["key"], "key");
        const change =
          call.method === "state.delete"
            ? { key, delete: true }
            : { key, value: p["value"] ?? null };
        const answer = await this.send({
          ...writeState(artifact, change, this.options.clientID, this.options.version),
          key: this.options.newKey(),
        });
        if (answer.kind === "done") return null;
        if (answer.kind === "unknown") throw new Refusal("unavailable", answer.text);
        throw new Refusal(
          answer.error?.status === 429
            ? "rate_limited"
            : answer.error?.status === 403
              ? "forbidden"
              : "invalid",
          answer.text,
        );
      }
      case "presence.set": {
        const activity =
          p["activity"] === "editing" || p["activity"] === "idle" ? p["activity"] : "viewing";
        const raw =
          typeof p["focus"] === "object" && p["focus"] !== null
            ? (p["focus"] as Record<string, unknown>)
            : {};
        const focus: BridgeRoster["members"][number]["focus"] = { version: this.options.version };
        for (const key of ["region", "collection", "doc", "field"] as const) {
          if (typeof raw[key] === "string") focus[key] = (raw[key] as string).slice(0, 128);
        }
        this.options.setPresence(activity, focus);
        return null;
      }
      case "assets.list": {
        const answer = await socket.query("artifact_assets", { id: artifact });
        return answer.assets.map((a) => ({
          id: a.id,
          name: a.name,
          contentType: a.content_type,
          size: a.object.size,
        }));
      }
      case "openLink": {
        // ONE RULE FOR EVERY WAY A FRAME ASKS TO LEAVE: the frame protocol's
        // open-link message and this call both go through `externalUrl`, so
        // a link carrying a user name or password (a trusted name in front
        // of another host) or past its length bound never reaches the
        // interstitial. The bridge's contract is narrower than the protocol's
        // by `mailto:`, which it does not open.
        const url = externalUrl(str(p["url"], "url"));
        if (!url || (url.protocol !== "https:" && url.protocol !== "http:")) {
          throw new Refusal(
            "invalid",
            `openLink takes an absolute https or http URL of at most ${EXTERNAL_URL_MAX} ` +
              "characters, with no user name or password",
          );
        }
        return this.options.openLink(url);
      }
    }
  }

  /** What changed in each subscribed collection since it was last told, sent as one event each. */
  private pushData(): void {
    const store = this.options.store;
    if (!this.port || !store) return;
    for (const [subscription, sub] of this.subscriptions) {
      const docs = store.docsOf(sub.collection);
      const upsert: BridgeDoc[] = [];
      const seen = new Set<string>();
      for (const d of docs) {
        seen.add(d.doc.id);
        const sig = signature(d.doc.version, d.pending, d.fields);
        if (sub.sent.get(d.doc.id) === sig) continue;
        sub.sent.set(d.doc.id, sig);
        upsert.push(bridgeDoc(d.doc, d.fields, d.pending));
      }
      const remove: string[] = [];
      for (const id of sub.sent.keys()) {
        if (!seen.has(id)) remove.push(id);
      }
      for (const id of remove) sub.sent.delete(id);
      if (upsert.length > 0 || remove.length > 0) {
        this.event({ topic: "data", subscription, upsert, remove });
      }
    }
  }
}

/** What tells one sent state of a document from the next: its version, and its values while a write on it is out. */
function signature(version: number, pending: boolean, fields: Record<string, unknown>): string {
  return pending ? `${version}:p:${JSON.stringify(fields)}` : `${version}`;
}
