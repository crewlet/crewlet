/**
 * The host end of a gallery PREVIEW's bridge: it answers the datastore reads a
 * page makes to draw itself, and nothing else.
 *
 * WHY A HOST AT ALL. A page that keeps its content in its datastore draws an
 * empty state with nobody answering its reads, and an empty Retro board is not
 * a snapshot of the Retro board. This host answers exactly the reads that
 * drawing needs, through the same questions the page host asks
 * (`bridgeReads.ts`): a document, a page of a collection, a query, the schema,
 * and a subscription answered once with the collection as it stands.
 *
 * A SNAPSHOT, NOT A LIVE VIEW. A preview is the page as it stood when the
 * gallery drew it, and its frame stays loaded after, so this host answers
 * each distinct read ONCE and every repeat of it from that answer: a page
 * that polls its datastore is drawn as it stood and asks the engine nothing
 * more, however long the gallery stays open. A refusal is kept the same way,
 * so a page retrying a read that failed does not retry it against the
 * engine. And it answers at most [PREVIEW_READS] distinct reads, so a page
 * that keeps inventing new ones is refused rather than read for.
 *
 * A NEWCOMER'S STATE. A page that keeps each viewer's own state (`state`:
 * the tab they chose, a filter, a dismissed hint) is handed the state of
 * somebody opening it for the first time: empty, and kept by this host alone
 * for as long as the frame lives, so a page that remembers a choice as it
 * draws reads it back as it would, and it is never the reader's own. The
 * reader's state is never read, since a card is not their visit, and nothing
 * the page sets reaches the engine. A page that keeps state used to be shown
 * as its kind's drawing instead, so a dashboard that remembered its branch
 * filter had no preview at all.
 *
 * WHAT IT NEVER DOES, by construction rather than by a check: it holds no
 * write path to the engine (no keyed write, no datastore copy, no state
 * write), so a datastore write, an undo or a print is refused before anything
 * is asked; it pushes nothing after a subscription's first answer, so it
 * keeps no live view of the room's data; it names no viewer and joins no
 * room, so the page learns nobody and shows nobody as viewing; and it files
 * no ready report. The frame around it is inert and takes no pointer, and the
 * engine decides every read again as the signed-in reader.
 */

import { RUNTIME_CONTRACT } from "~/contract/artifacts.ts";
import type { LiveSocket } from "~/protocol/index.ts";
import {
  BRIDGE_MESSAGE,
  capabilityOf,
  readCall,
  type BridgeCall,
  type RuntimeCapability,
} from "~/runtime/bridge.ts";
import {
  Refusal,
  bridgeDoc,
  readAssets,
  readData,
  readSchema,
  refusalOf,
  str,
} from "./bridgeReads.ts";

/** The most documents a subscription's one answer carries: more than a card can show. */
export const PREVIEW_SNAPSHOT_LIMIT = 200;

/**
 * The most distinct reads a preview's page is answered. Drawing a page takes
 * a handful (its schema, a collection or two, a query per chart); sixty-four
 * covers a busy board many times over, and bounds what a page that builds a
 * new read on every tick can ask the engine for through a card.
 */
export const PREVIEW_READS = 64;

/**
 * The capabilities a preview's page is told it holds: the frame's grant, and
 * of it only what this host answers. Anything else the version declares is
 * not offered, so the page's runtime never asks for it.
 */
export const PREVIEW_HOST_CAPABILITIES: readonly RuntimeCapability[] = ["db", "assets", "state"];

/**
 * The most keys a preview's newcomer state holds, and the most bytes one
 * value may take as JSON: a page draws with a few remembered choices, and a
 * page that writes more on every tick is refused rather than kept in the
 * gallery's memory.
 */
export const PREVIEW_STATE_KEYS = 64;
export const PREVIEW_STATE_VALUE_BYTES = 16 * 1024;

/**
 * Whether a frame granted `granted` is drawn from something this host answers
 * (its datastore, or a state it remembers), and so has a host on its port. A
 * page granted its assets alone draws from its own files and has nothing on
 * its port, as before.
 */
export function hostAnswers(granted: readonly RuntimeCapability[]): boolean {
  return granted.includes("db") || granted.includes("state");
}

export class PreviewHost {
  private nextSubscription = 1;
  /** Each distinct read the page has made, by method and parameters, as it was answered. */
  private readonly answered = new Map<string, Promise<unknown>>();
  /** The newcomer's state the page has set while its frame lives: empty at first, never the reader's. */
  private readonly state = new Map<string, unknown>();

  constructor(
    private readonly options: {
      artifact: string;
      socket: LiveSocket;
      /** What the frame's ticket grants; the page is told the part of it this host answers. */
      granted: readonly RuntimeCapability[];
    },
  ) {}

  /** The page said hello: tell it what it may read, that it is live, and that nobody is named. */
  attach(port: MessagePort): void {
    port.postMessage({
      type: BRIDGE_MESSAGE.init,
      contract: RUNTIME_CONTRACT,
      capabilities: this.options.granted.filter((c) => PREVIEW_HOST_CAPABILITIES.includes(c)),
      viewer: null,
      connection: "live",
    });
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
    const { artifact, socket, granted } = this.options;
    const needs = capabilityOf(call.method);
    if (needs && (!granted.includes(needs) || !PREVIEW_HOST_CAPABILITIES.includes(needs))) {
      throw new Refusal("capability_not_declared", `a preview is not granted \`${needs}\``);
    }
    const p = call.params;
    switch (call.method) {
      case "assets.list":
        return this.once(call, () => readAssets({ artifact, socket }));
      case "data.get":
      case "data.list":
      case "data.query": {
        const method = call.method;
        return this.once(call, () => readData(method, p, { artifact, socket }));
      }
      case "data.schema":
        return this.once(call, () => readSchema({ artifact, socket }));
      case "data.subscribe": {
        // ONCE, AS IT STANDS: the collection's first page is the answer, and
        // no change is pushed after it. A page subscribing again is handed
        // the same rows under a subscription of its own.
        const docs = await this.once(call, async () => {
          const page = await socket.query("artifact_data", {
            id: artifact,
            collection: str(p["collection"], "collection"),
            limit: PREVIEW_SNAPSHOT_LIMIT,
          });
          return page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d));
        });
        return { subscription: this.nextSubscription++, docs };
      }
      case "data.unsubscribe":
        return null;
      case "state.get":
        return this.state.get(str(p["key"], "key")) ?? null;
      case "state.list": {
        const prefix = typeof p["prefix"] === "string" ? p["prefix"] : "";
        return [...this.state.keys()].filter((k) => k.startsWith(prefix)).sort();
      }
      case "state.set": {
        const key = str(p["key"], "key");
        const value = p["value"] ?? null;
        if (!this.state.has(key) && this.state.size >= PREVIEW_STATE_KEYS) {
          throw new Refusal("rate_limited", "a preview keeps only a few remembered choices");
        }
        if (JSON.stringify(value).length > PREVIEW_STATE_VALUE_BYTES) {
          throw new Refusal("invalid", "a preview keeps only small remembered choices");
        }
        this.state.set(key, value);
        return null;
      }
      case "state.delete":
        this.state.delete(str(p["key"], "key"));
        return null;
      default:
        throw new Refusal("forbidden", "a preview only reads; open the artifact to do this");
    }
  }

  /** The answer to a read, asked of the engine the first time the page makes it and kept after. */
  private once(call: BridgeCall, read: () => Promise<unknown>): Promise<unknown> {
    const key = `${call.method} ${JSON.stringify(call.params)}`;
    const held = this.answered.get(key);
    if (held) return held;
    if (this.answered.size >= PREVIEW_READS) {
      return Promise.reject(
        new Refusal("forbidden", "a preview has read all it may; open the artifact to read more"),
      );
    }
    const answer = read();
    this.answered.set(key, answer);
    return answer;
  }
}
