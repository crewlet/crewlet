/**
 * An artifact's datastore as a screen holds it ([ArtifactDataStore]), for as
 * long as the screen is mounted, and re-rendered when it moves.
 */

import { useState, useSyncExternalStore } from "react";
import { newActOpID } from "~/protocol/index.ts";
import { useClient } from "../store-hooks.ts";
import { useOwned } from "../useOwned.ts";
import { ArtifactDataStore, rowID } from "./datastore.ts";
import type { DataVia } from "./api.ts";

/**
 * A store for artifact `id`, written as `via` says, or null while `enabled` is
 * false. A new artifact or a new `via` (another version of a page) is a new
 * store, since what the engine holds a write to has changed; it is null until
 * that store is made, so a screen never reads the one it replaced.
 */
export function useDataStore(id: string, via: DataVia, enabled: boolean): ArtifactDataStore | null {
  const { socket } = useClient();
  const viaKey = via.via === "bridge" ? `bridge:${via.version}` : "dashboard";
  // ONE CLIENT ID PER STORE, so a page and the sheet beside it recognise
  // their own writes in the store's history and nobody else's.
  const [clientID] = useState(() => `dash-${rowID()}`);
  // OWNED BY AN EFFECT, never a memo: a store a cleanup stopped is never
  // handed back to the screen that reads it (`useOwned` says why).
  return useOwned(
    () =>
      enabled && id
        ? new ArtifactDataStore({ artifact: id, socket, via, clientID, newKey: () => newActOpID() })
        : null,
    (store) => store.stop(),
    // `via` is read through its key: a new object naming the same surface is the same store.
    [enabled, id, socket, viaKey, clientID],
  );
}

/** Re-renders whenever `store` moves, and answers a version a memo can key on. */
export function useStoreVersion(store: ArtifactDataStore | null): number {
  const [counter] = useState(() => ({ n: 0 }));
  return useSyncExternalStore(
    (fn) =>
      store
        ? store.subscribe(() => {
            counter.n++;
            fn();
          })
        : () => {},
    () => counter.n,
    () => counter.n,
  );
}
