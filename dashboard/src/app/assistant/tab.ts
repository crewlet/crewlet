/**
 * What THIS TAB remembers of the assistant across a reload: whether it was
 * open, and which conversation it showed. Nothing else.
 *
 * IN `sessionStorage`, for the reader record's reason (`lib/reader.ts`): it
 * describes one tab, two tabs of one browser may be on two conversations, and
 * the hand-over that empties the tab's storage when somebody else signs in
 * (`lib/session.ts`) takes it with everything else, so a shared browser never
 * shows one person's conversation to the next. The conversation itself is the
 * engine's, read again under the session on the way back.
 *
 * AN ID AND A FLAG, NEVER CONTENT: what was said lives in the engine, owner
 * keyed, and a tab keeps only where to find it. A value this build cannot read
 * (another build's, a hand edit) reads as nothing kept.
 */

import { STORAGE_KEYS } from "~/lib/storage.ts";

/** What a tab keeps. */
export interface AssistantTab {
  /** The conversation the drawer showed, or null for its home. */
  conversation: string | null;
  /** Whether the drawer was open. */
  open: boolean;
}

const NOTHING: AssistantTab = { conversation: null, open: false };

/** A conversation id as the engine mints one: a UUID, and nothing that is not. */
const ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** What this tab kept, or nothing where it kept nothing this build can read. */
export function readTab(): AssistantTab {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEYS.assistantTab);
    if (!raw) return NOTHING;
    const value = JSON.parse(raw) as Partial<AssistantTab>;
    return {
      conversation:
        typeof value.conversation === "string" && ID.test(value.conversation)
          ? value.conversation
          : null,
      open: value.open === true,
    };
  } catch {
    // Refused or unreadable: the tab keeps nothing, and opens closed.
    return NOTHING;
  }
}

/** Keep `change` over what this tab kept. */
export function writeTab(change: Partial<AssistantTab>): void {
  try {
    const next = { ...readTab(), ...change };
    sessionStorage.setItem(STORAGE_KEYS.assistantTab, JSON.stringify(next));
  } catch {
    // A tab that cannot keep it opens closed after a reload; nothing is lost
    // but the convenience.
  }
}
