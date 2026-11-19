/**
 * Whether the reader may open what a line names, and what it is called now,
 * read IN BATCH for every reference a tab draws (`references`).
 *
 * A line the engine writes says what it names (`NamedRef`), and every surface
 * draws each through one renderer (`components/InlineToken.tsx`'s
 * [NamedRefToken]). Each asks here: the references drawn in one tick go to the
 * engine as ONE ask (chunked at [MAX_REFERENCES]), each settled through the
 * question its object's own page asks, as the reader. A page of fifty feed
 * lines asks once, and a reference drawn again (a reload of the feed, the same
 * item on another page) is not asked again on this socket.
 *
 * THREE READINGS: `undefined` while it is asked, `null` where the reader may
 * not open it or it is gone (never told apart), and the reference's title
 * otherwise. A reference the engine could not settle (an outage, not a
 * refusal) reads `undefined` and is asked again the next time it is drawn, as
 * is every reference of an ask that failed to answer.
 *
 * Outside a client (a suite, a page outside the app) nothing is read and every
 * reference stays `undefined`, which draws it as its marked words.
 */

import { useCallback, useContext, useSyncExternalStore } from "react";
import { MAX_REFERENCES } from "~/contract/named.ts";
import { ClientContext, type Client } from "./store-hooks.ts";

/** An open reference: what its object is called now, where its page names one. */
export interface Opened {
  title: string;
}

/** A reading: `undefined` while asked, `null` closed, else open. */
export type Reading = Opened | null | undefined;

/** What a reference is asked by: its kind and its id. */
export interface Asked {
  kind: string;
  id: string;
}

interface Entry {
  reading: Reading;
  listeners: Set<() => void>;
  /** Its last ask did not settle it: the next time it is drawn asks again. */
  retry: boolean;
}

interface Table {
  entries: Map<string, Entry>;
  /** What the next ask carries, gathered until the tick ends. */
  queued: Set<string>;
  flushing: boolean;
}

const tables = new WeakMap<Client["socket"], Table>();

/** The key a reference is asked and kept by, as the engine reads it. */
export const referenceKey = ({ kind, id }: Asked): string => `${kind}:${id}`;

function tableFor(socket: Client["socket"]): Table {
  let table = tables.get(socket);
  if (!table) {
    table = { entries: new Map(), queued: new Set(), flushing: false };
    tables.set(socket, table);
  }
  return table;
}

function entryFor(socket: Client["socket"], key: string): Entry {
  const table = tableFor(socket);
  const entry = table.entries.get(key);
  if (entry) return entry;
  const made: Entry = { reading: undefined, listeners: new Set(), retry: false };
  table.entries.set(key, made);
  queue(socket, table, key);
  return made;
}

/** Puts a reference on the next ask, which leaves when this tick ends. */
function queue(socket: Client["socket"], table: Table, key: string): void {
  table.queued.add(key);
  if (table.flushing) return;
  table.flushing = true;
  queueMicrotask(() => {
    table.flushing = false;
    const keys = [...table.queued];
    table.queued.clear();
    for (let at = 0; at < keys.length; at += MAX_REFERENCES) {
      ask(socket, table, keys.slice(at, at + MAX_REFERENCES));
    }
  });
}

function ask(socket: Client["socket"], table: Table, keys: string[]): void {
  socket.query("references", { refs: keys }).then(
    (answer) => {
      // AN ANSWER OF ANOTHER SHAPE SETTLES NOTHING, as a failed ask does.
      if (!Array.isArray(answer?.references) || !Array.isArray(answer?.unresolved)) {
        retryAll(table, keys);
        return;
      }
      const open = new Map(answer.references.map((r) => [referenceKey(r), r.title ?? ""]));
      const unsettled = new Set(answer.unresolved);
      for (const key of keys) {
        const entry = table.entries.get(key);
        if (!entry) continue;
        if (unsettled.has(key)) {
          entry.retry = true;
          continue;
        }
        const title = open.get(key);
        settle(entry, title === undefined ? null : { title });
      }
    },
    // An ask that failed to answer settles nothing: each is asked again.
    () => retryAll(table, keys),
  );
}

function retryAll(table: Table, keys: readonly string[]): void {
  for (const key of keys) {
    const entry = table.entries.get(key);
    if (entry) entry.retry = true;
  }
}

function settle(entry: Entry, reading: Reading): void {
  entry.reading = reading;
  for (const listener of entry.listeners) listener();
}

/**
 * What the reference names, as this reader may open it; `null` for no
 * reference, which asks nothing.
 */
export function useReference(ref: Asked | null): Reading {
  const client = useContext(ClientContext);
  const socket = client?.socket ?? null;
  const key = ref ? referenceKey(ref) : null;
  const subscribe = useCallback(
    (listener: () => void) => {
      if (!socket || key === null) return () => {};
      const entry = entryFor(socket, key);
      if (entry.retry) {
        entry.retry = false;
        queue(socket, tableFor(socket), key);
      }
      entry.listeners.add(listener);
      return () => void entry.listeners.delete(listener);
    },
    [socket, key],
  );
  const read = useCallback(
    () => (socket && key !== null ? entryFor(socket, key).reading : undefined),
    [socket, key],
  );
  return useSyncExternalStore(subscribe, read, read);
}
