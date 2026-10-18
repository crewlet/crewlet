/**
 * A value kept per reader in this browser, validated on every read: what one
 * person made of a page at one desk (their Home's board, the order of their
 * pinned shelf), which belongs to neither the company document nor its audit
 * trail.
 *
 * KEYED BY THE READER (`lib/reader.ts`), so one browser shared by two people
 * keeps each their own; a tab that has not learned its reader keeps the value
 * in memory for the visit and stores nothing, as `lib/starred.ts` does.
 *
 * STORAGE IS UNTRUSTED INPUT. `valid` is handed whatever parsed, from any
 * build or any hand, and returns the value to use: a caller checks every field.
 */

import { useSyncExternalStore } from "react";
import { currentReader, onReader } from "./reader.ts";
import type { StorageKey } from "./storage.ts";

/**
 * A value kept per reader under `prefix` (a registered `STORAGE_KEYS` prefix,
 * the reader's id following it), validated on every read.
 *
 * `useSyncExternalStore` needs a referentially stable snapshot, so the parsed
 * value is cached per key and dropped on a write, a `storage` event from
 * another tab, or the tab learning its reader.
 */
export function readerStore<T>(prefix: StorageKey, fallback: T, valid: (raw: unknown) => T) {
  const listeners = new Set<() => void>();
  let cache: { key: string; value: T } | null = null;
  let memory: T = fallback;
  const keyOf = (): string | null => {
    const reader = currentReader();
    return reader === null ? null : `${prefix}${reader}`;
  };
  const read = (): T => {
    const key = keyOf();
    if (key === null) return memory;
    if (cache?.key === key) return cache.value;
    let value = fallback;
    try {
      const text = window.localStorage.getItem(key);
      if (text !== null) value = valid(JSON.parse(text));
    } catch {
      // Blocked storage or a value that is not JSON: the default Home.
    }
    cache = { key, value };
    return value;
  };
  const emit = () => {
    for (const fn of listeners) fn();
  };
  const write = (next: T) => {
    const key = keyOf();
    if (key === null) {
      memory = next;
    } else {
      cache = { key, value: next };
      try {
        window.localStorage.setItem(key, JSON.stringify(next));
      } catch {
        // A quota or a blocked store: kept for this visit, as with no reader.
      }
    }
    emit();
  };
  const follow = (e: StorageEvent) => {
    if (e.key !== null && e.key !== keyOf()) return;
    cache = null;
    emit();
  };
  const subscribe = (fn: () => void) => {
    if (listeners.size === 0) window.addEventListener("storage", follow);
    listeners.add(fn);
    const unread = onReader(() => {
      cache = null;
      fn();
    });
    return () => {
      unread();
      listeners.delete(fn);
      if (listeners.size === 0) window.removeEventListener("storage", follow);
    };
  };
  return {
    read,
    write,
    update: (edit: (current: T) => T) => write(edit(read())),
    use: (): T => useSyncExternalStore(subscribe, read, () => fallback),
  };
}
