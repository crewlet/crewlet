/**
 * What THIS TAB remembers of the assistant across a reload: whether it was
 * open, which conversation it showed, the page the drawer last showed it on,
 * and whether it had work in flight. Nothing else.
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

/** Which conversation the drawer last showed on which page ([pageIdentity]). */
export interface ShownOn {
  conversation: string | null;
  page: string | null;
}

/** What a tab keeps. */
export interface AssistantTab {
  /** The conversation the drawer showed, or null for its home. */
  conversation: string | null;
  /** Whether the drawer was open. */
  open: boolean;
  /** Where the drawer last showed a conversation, or null where it never did. */
  shown: ShownOn | null;
  /** The conversation had a turn running or a change waiting on approval. */
  busy: boolean;
}

const NOTHING: AssistantTab = { conversation: null, open: false, shown: null, busy: false };

/** A conversation id as the engine mints one: a UUID, and nothing that is not. */
export const CONVERSATION_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** What this tab kept, or nothing where it kept nothing this build can read. */
export function readTab(): AssistantTab {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEYS.assistantTab);
    if (!raw) return NOTHING;
    const value = JSON.parse(raw) as Partial<AssistantTab>;
    return {
      conversation: idOrNull(value.conversation),
      open: value.open === true,
      shown: shownOf(value.shown),
      busy: value.busy === true,
    };
  } catch {
    // Refused or unreadable: the tab keeps nothing, and opens closed.
    return NOTHING;
  }
}

function idOrNull(value: unknown): string | null {
  return typeof value === "string" && CONVERSATION_ID.test(value) ? value : null;
}

function shownOf(value: unknown): ShownOn | null {
  if (typeof value !== "object" || value === null) return null;
  const { conversation, page } = value as Record<string, unknown>;
  return { conversation: idOrNull(conversation), page: typeof page === "string" ? page : null };
}

/**
 * How the drawer opened by the person's hand starts: the conversation it last
 * showed where this is that page or it has work in flight, else a fresh one.
 */
export function reopenChoice(tab: AssistantTab, page: string | null): "resume" | "fresh" {
  if (tab.busy) return "resume";
  const { shown } = tab;
  return shown !== null && shown.page === page && shown.conversation === tab.conversation
    ? "resume"
    : "fresh";
}

/**
 * Ask Crewlet took the person to `page`: the conversation open now goes with
 * it, so the drawer opened there resumes it.
 */
export function carryConversation(page: string | null): void {
  if (page === null) return;
  writeTab({ shown: { conversation: readTab().conversation, page } });
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
