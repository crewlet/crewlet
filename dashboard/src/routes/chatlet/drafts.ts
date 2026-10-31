/**
 * What a person has typed and not sent, per room and per thread.
 *
 * ONE STORE FOR THE SCREEN, held by the chat frame and read by everything that
 * says something about a draft: the composer that writes it, the Rooms
 * panel's pencil on a room with one, and Drafts & sent, which lists them. A
 * draft kept inside its composer was lost the moment the room changed, and
 * nothing outside the room could say it existed.
 *
 * IN MEMORY FOR THE TAB'S LIFE, AND NOWHERE ELSE. Not in the address, where a
 * link somebody pastes into a room would carry half a sentence with it, and
 * not in browser storage either: what this tab has not said is not state
 * anything else on the machine should be able to read. A reload starts clean,
 * which is what closing a half-written letter does.
 */

import { createContext, useContext, useState, useSyncExternalStore } from "react";

/** One unsent draft, as Drafts & sent lists it. */
export interface ChatDraft {
  /** The room the draft is for. */
  room: string;
  /** The thread's root, for a reply; absent for a message to the room. */
  root?: string;
  text: string;
  /** When it was last typed into, for the newest-first order. */
  at: number;
}

/** The key one box's draft is kept under: the room, or the room and a thread. */
export function draftKey(room: string, root?: string): string {
  return root ? `${room}/${root}` : room;
}

export class ChatDrafts {
  private readonly held = new Map<string, ChatDraft>();
  private readonly listeners = new Set<() => void>();
  /** Bumped on every change, so a reader's snapshot is a cheap number. */
  private version = 0;

  /** What the box under `key` holds, or "" when nothing. */
  get(key: string): string {
    return this.held.get(key)?.text ?? "";
  }

  /**
   * Keep what a box holds. Text that is only whitespace is no draft: an empty
   * box clears its entry, so a room is never marked for a stray space.
   */
  set(key: string, text: string, room: string, root?: string): void {
    const had = this.held.get(key);
    if (text.trim() === "") {
      if (!had) return;
      this.held.delete(key);
    } else {
      if (had?.text === text) return;
      this.held.set(key, { room, ...(root ? { root } : {}), text, at: Date.now() });
    }
    this.version += 1;
    for (const listener of this.listeners) listener();
  }

  /** Whether a room's own box, or a reply box in one of its threads, holds words. */
  has(room: string): boolean {
    for (const draft of this.held.values()) if (draft.room === room) return true;
    return false;
  }

  /** Every draft, newest edit first. */
  list(): ChatDraft[] {
    return [...this.held.values()].sort((a, b) => b.at - a.at);
  }

  /** How many boxes hold an unsent draft. */
  get count(): number {
    return this.held.size;
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  snapshot = (): number => this.version;
}

/** The screen's one store, provided by the chat frame. */
export const DraftsContext = createContext<ChatDrafts | null>(null);

/**
 * The screen's drafts, re-rendering the caller on every change. Outside a
 * provider the caller holds a store of its own, so a box drawn alone (in a
 * test, say) still keeps what is typed into it, and two such boxes never
 * share one.
 */
export function useChatDrafts(): ChatDrafts {
  const [own] = useState(() => new ChatDrafts());
  const drafts = useContext(DraftsContext) ?? own;
  useSyncExternalStore(drafts.subscribe, drafts.snapshot, drafts.snapshot);
  return drafts;
}
