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
import type { DataOp, LiveSocket } from "~/protocol/index.ts";
import { RUNTIME_CONTRACT, STATE_COLLECTION } from "~/contract/artifacts.ts";
import {
  BRIDGE_MESSAGE,
  capabilityOf,
  readCall,
  type BridgeCall,
  type BridgeConnection,
  type BridgeDoc,
  type BridgePerson,
  type BridgeRoster,
  type BridgeViewer,
  type BridgeWriteOutcome,
  type RuntimeCapability,
} from "~/runtime/bridge.ts";
import type { ArtifactDataStore, WriteResult } from "~/lib/artifacts/datastore.ts";
import { Refusal, bridgeDoc, readAssets, readData, refusalOf, str } from "./bridgeReads.ts";

export { bridgeDoc, engineQuery } from "./bridgeReads.ts";
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
  /**
   * Prints this page alone, as the reader's own print of it; resolves once
   * the print was handed to the browser. Absent where the host offers no
   * print, whose every request is then refused.
   */
  print?: (() => Promise<boolean>) | undefined;
  /**
   * Whether the reader acted on this document just now (a click or a key in
   * the page counts, since a frame's activation reaches its host): the gate
   * on a print, so a page cannot open the print dialog on its own. The
   * browser's own `navigator.userActivation` outside a suite.
   */
  readerActed?: (() => boolean) | undefined;
  /** Says where the reader is in the room. */
  setPresence: (
    activity: "viewing" | "editing" | "idle",
    focus: BridgeRoster["members"][number]["focus"],
  ) => void;
  /** Sends one keyed write; `keyedWrite` outside a suite. */
  write?: (req: KeyedRequest) => Promise<KeyedAnswer>;
}

/**
 * A write's result as a page handles it: RESOLVED for every verdict the engine
 * decided, whichever side of the log decided it, and REJECTED only where it
 * decided nothing (a spent budget, an answer nobody could establish), as the
 * runtime contract says (`docs/reference/artifact-runtime.md`).
 */
export function bridgeOutcome(result: WriteResult): BridgeWriteOutcome {
  switch (result.verdict) {
    case "applied":
      return { verdict: "applied", seq: result.seq };
    case "pending":
      return { verdict: "pending" };
    case "unknown":
      throw new Refusal("unavailable", result.reason);
    case "rate_limited":
      throw new Refusal("rate_limited", result.reason);
    default:
      return {
        verdict: result.verdict,
        reason: result.reason,
        ...(result.conflict ? { conflict: result.conflict.map((d) => bridgeDoc(d)) } : {}),
      };
  }
}

/** The host for one embedded page. */
export class PageHost {
  private port: MessagePort | null = null;
  private viewer: BridgeViewer | null = null;
  private connection: BridgeConnection = "live";
  private roster: BridgeRoster = { members: [], truncated: false };
  private people: readonly BridgePerson[] = [];
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

  /** The chart moved: the people a page that declared `identity` may name. */
  setPeople(people: readonly BridgePerson[]): void {
    this.people = people;
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
      case "identity.people":
        // FROM THE CHART THIS TAB HOLDS, as every screen draws a seat: the
        // people the viewer already sees, in display facts only.
        return [...this.people];
      case "data.schema": {
        const held = needStore();
        if (!held.schema) await held.loadSchema();
        if (!held.schema) throw refusalOf(held.schemaFailure);
        return held.schema.collections.filter((c) => c.name !== STATE_COLLECTION);
      }
      case "data.get":
      case "data.list":
      case "data.query":
        return readData(call.method, p, { artifact, socket });
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
      case "data.undo":
      case "data.redo": {
        // THE PAGE'S OWN WRITES, on this tab: the store the page writes
        // through keeps them, so an undo never reaches a collaborator's
        // write, and null says there is nothing to step.
        const held = needStore();
        const result = call.method === "data.undo" ? await held.undo() : await held.redo();
        return result === null ? null : bridgeOutcome(result);
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
      case "assets.list":
        return readAssets({ artifact, socket });
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
      case "print": {
        if (!this.options.print) {
          throw new Refusal("unavailable", "this page cannot be printed here");
        }
        // ONLY ON THE READER'S GESTURE: a page that could print whenever it
        // liked could put the print dialog in front of the reader at will.
        const acted =
          this.options.readerActed ?? (() => navigator.userActivation?.isActive ?? false);
        if (!acted()) {
          throw new Refusal("forbidden", "print runs only on the reader's own click or key press");
        }
        return this.options.print();
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
