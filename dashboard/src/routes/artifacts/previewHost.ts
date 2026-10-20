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
 * WHAT IT NEVER DOES, by construction rather than by a check: it holds no
 * write path (no keyed write, no datastore copy, no state), so a write, an
 * undo, a state change or a print is refused before anything is asked; it
 * pushes nothing after a subscription's first answer, so it keeps no live
 * view of the room's data; it names no viewer and joins no room, so the page
 * learns nobody and shows nobody as viewing; and it files no ready report.
 * The frame around it is inert and takes no pointer, and the engine decides
 * every read again as the signed-in reader.
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
 * The capabilities a preview's page is told it holds: the frame's grant, and
 * of it only what this host answers. Anything else the version declares is
 * not offered, so the page's runtime never asks for it.
 */
export const PREVIEW_HOST_CAPABILITIES: readonly RuntimeCapability[] = ["db", "assets"];

export class PreviewHost {
  private nextSubscription = 1;

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
        return readAssets({ artifact, socket });
      case "data.get":
      case "data.list":
      case "data.query":
        return readData(call.method, p, { artifact, socket });
      case "data.schema":
        return readSchema({ artifact, socket });
      case "data.subscribe": {
        // ONCE, AS IT STANDS: the collection's first page is the answer, and
        // no change is pushed after it.
        const page = await socket.query("artifact_data", {
          id: artifact,
          collection: str(p["collection"], "collection"),
          limit: PREVIEW_SNAPSHOT_LIMIT,
        });
        return {
          subscription: this.nextSubscription++,
          docs: page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d)),
        };
      }
      case "data.unsubscribe":
        return null;
      default:
        throw new Refusal("forbidden", "a preview only reads; open the artifact to do this");
    }
  }
}
