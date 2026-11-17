/**
 * The kind and title of an artifact a reference names (`components/ArtifactLink.tsx`),
 * read once per id for every reference in the tab: a page naming one artifact
 * ten times asks once.
 */

import { useCallback, useContext, useSyncExternalStore } from "react";
import type { ArtifactKind } from "~/protocol/index.ts";
import { ClientContext, type Client } from "../store-hooks.ts";
import { queryErrorCode } from "~/protocol/socket.ts";
import { replacesData } from "../useQuery.ts";

/** What a reference draws: the artifact's kind and its title. */
export interface ArtifactHeadline {
  kind: ArtifactKind;
  title: string;
}

/** A reading: `undefined` while it is asked, `null` where the reader may not see it or it is gone. */
type Reading = ArtifactHeadline | null | undefined;

interface Entry {
  reading: Reading;
  listeners: Set<() => void>;
  /** Its last read failed to answer: the next reference drawn asks again. */
  retry: boolean;
}

const tables = new WeakMap<Client["socket"], Map<string, Entry>>();

function entryFor(socket: Client["socket"], id: string): Entry {
  const table = tables.get(socket) ?? new Map<string, Entry>();
  tables.set(socket, table);
  const entry = table.get(id);
  if (entry) return entry;
  const made: Entry = { reading: undefined, listeners: new Set(), retry: false };
  table.set(id, made);
  ask(socket, id, made);
  return made;
}

function ask(socket: Client["socket"], id: string, entry: Entry): void {
  entry.retry = false;
  socket.query("artifact", { id }).then(
    (a) => settle(entry, { kind: a.kind, title: a.title }),
    (err: unknown) => {
      // An answer about the artifact stands; a failure to answer is asked again.
      const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
      entry.retry = !replacesData(code);
      settle(entry, null);
    },
  );
}

function settle(entry: Entry, reading: Reading): void {
  entry.reading = reading;
  for (const listener of entry.listeners) listener();
}

/**
 * The artifact `id` names, as this reader may see it. Outside a client (a
 * suite, a page outside the app) nothing is read and every reference is null.
 */
export function useArtifactHeadline(id: string | null): Reading {
  const client = useContext(ClientContext);
  const socket = client?.socket ?? null;
  const subscribe = useCallback(
    (listener: () => void) => {
      if (!socket || !id) return () => {};
      const entry = entryFor(socket, id);
      if (entry.retry) ask(socket, id, entry);
      entry.listeners.add(listener);
      return () => void entry.listeners.delete(listener);
    },
    [socket, id],
  );
  const read = useCallback(
    () => (socket && id ? entryFor(socket, id).reading : null),
    [socket, id],
  );
  return useSyncExternalStore(subscribe, read, read);
}
