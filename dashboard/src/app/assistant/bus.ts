/**
 * The assistant's in-app bus: how the drawer and the screen under it talk.
 *
 * A MODULE-SCOPED EMITTER, NEVER `postMessage` AND NEVER A DOM EVENT. An
 * artifact's sandboxed frame can post a message to this window, and any
 * script on the page can dispatch a DOM event, so either would let something
 * other than the drawer ask a screen to apply an action. A module binding is
 * reachable only by code this build imported, which is the whole of the
 * trust this channel needs: what travels on it is still parsed and gated by
 * the screen that receives it (`context.ts`).
 *
 * FOUR EVENTS, typed by name, so a listener for one cannot be handed
 * another's payload:
 *
 *  - `assistant.action.apply`: the drawer asks the screen to apply a page
 *    action it claimed on the server;
 *  - `assistant.action.result`: the screen says what became of it;
 *  - `assistant.context.invalidate`: the screen's context moved (the route,
 *    its filters, its data or its selection), so a prepared change may be
 *    stale;
 *  - `assistant.navigation.open`: the drawer asks the frame to go to an
 *    address the engine built, already checked to resolve (`chunk/openInApp.ts`).
 *
 * It lives in the ENTRY, a few hundred bytes, because both ends do: the
 * screen's registry is mounted with the screen, and the drawer is a chunk.
 */

/** What a page reports about an action it was sent; never `applying`, which is the claim. */
export type AssistantActionOutcome = "applied" | "stale" | "blocked" | "invalid" | "no_handler";

/** Why the screen's context moved. */
export type AssistantInvalidateReason = "route" | "filters" | "data" | "selection";

/** Each event's payload, by its name. */
export interface AssistantBusEvents {
  "assistant.action.apply": {
    correlation_id: string;
    action: string;
    payload: unknown;
    source_revision: string;
  };
  "assistant.action.result": {
    correlation_id: string;
    outcome: AssistantActionOutcome;
    reason?: string;
    revision: string;
    receipt?: string;
    /** The screen left an undo for it (`context.ts`), so its receipt may offer Undo. */
    undoable?: boolean;
  };
  "assistant.context.invalidate": {
    reason: AssistantInvalidateReason;
    revision: string;
    /** Each page action's own revision now, by id ([actionRevision]). */
    revisions: Readonly<Record<string, string>>;
  };
  "assistant.navigation.open": {
    path: string[];
    query: Record<string, string>;
    mode: "here" | "new_tab";
  };
}

export type AssistantBusEvent = keyof AssistantBusEvents;

type Listener<E extends AssistantBusEvent> = (payload: AssistantBusEvents[E]) => void;

const listeners: { [E in AssistantBusEvent]: Set<Listener<E>> } = {
  "assistant.action.apply": new Set(),
  "assistant.action.result": new Set(),
  "assistant.context.invalidate": new Set(),
  "assistant.navigation.open": new Set(),
};

/**
 * Hand `payload` to every listener of `event`, in the order they subscribed.
 *
 * A LISTENER THAT THROWS DOES NOT SILENCE THE REST: each is called in its own
 * frame of the loop, and the first error is rethrown once every listener has
 * heard the event, so a screen's fault is reported rather than swallowed and
 * the drawer still hears its own result.
 */
export function emit<E extends AssistantBusEvent>(event: E, payload: AssistantBusEvents[E]): void {
  let failure: unknown = null;
  for (const listener of [...listeners[event]] as Listener<E>[]) {
    try {
      listener(payload);
    } catch (err) {
      failure ??= err;
    }
  }
  if (failure !== null) throw failure;
}

/** Listen for `event` until the returned function is called. */
export function on<E extends AssistantBusEvent>(event: E, listener: Listener<E>): () => void {
  const set = listeners[event] as Set<Listener<E>>;
  set.add(listener);
  return () => {
    set.delete(listener);
  };
}
