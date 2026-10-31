/**
 * The findings a reviewer has drafted on a change's lines and not yet sent.
 *
 * A REVIEW IS ONE RECORD (`code_review_change`): its verdict, its summary and
 * every finding it raises land together, so a reviewer drafts findings line
 * by line and sends them with the verdict in ONE press. Until then the drafts
 * are the tab's — kept in `sessionStorage`, per change and per revision (a
 * finding names the lines of the revision it was read in, and a draft on an
 * earlier revision is not a finding on the head), so a reload, or the Files
 * tab left and come back to, keeps them, and closing the tab lets them go:
 * nothing half-written outlives the session it was written in, and nothing is
 * recorded before the reviewer says so.
 *
 * READ AND WRITTEN UNDER A TRY: storage can be refused (a private window, a
 * blocked origin), and a refusal keeps the drafts for as long as the screen
 * holds them rather than failing the review.
 */

import { useCallback, useEffect, useState } from "react";
import { STORAGE_KEYS } from "./storage.ts";

/** One drafted finding: what `code_review_change` takes for each of its findings. */
export interface DraftFinding {
  /** The draft's own id, which nothing but this tab knows. */
  id: string;
  path: string;
  side: "old" | "new";
  line_start: number;
  line_end: number;
  severity: string;
  category: string;
  title: string;
  body: string;
  /** The lines to put in place of the finding's, on the newer side only. */
  suggestion?: string;
}

/** Where one revision's drafts are kept. */
export function draftsKey(changeId: string, revision: number): string {
  return `${STORAGE_KEYS.reviewDrafts}${changeId}/${revision}`;
}

/** The drafts kept for a revision — none where storage holds none, or refuses. */
export function readDrafts(changeId: string, revision: number): DraftFinding[] {
  try {
    const raw = sessionStorage.getItem(draftsKey(changeId, revision));
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as DraftFinding[]) : [];
  } catch {
    return [];
  }
}

/**
 * What hears a write of drafts in THIS tab: storage raises no event in the tab
 * that wrote it, and the review that sends a revision's drafts is not the
 * diff that drew them, which must stop drawing them once they are sent.
 */
const heard = new Set<() => void>();

/** Keep a revision's drafts — or forget them, where none is left — and say so. */
function writeDrafts(changeId: string, revision: number, drafts: readonly DraftFinding[]): void {
  try {
    if (drafts.length === 0) sessionStorage.removeItem(draftsKey(changeId, revision));
    else sessionStorage.setItem(draftsKey(changeId, revision), JSON.stringify(drafts));
  } catch {
    // Refused: the drafts stay on screen for as long as it holds them.
  }
  for (const hear of heard) hear();
}

/**
 * Forget a revision's drafts: what a review that raised them does once the
 * engine has taken it.
 */
export function forgetDrafts(changeId: string, revision: number): void {
  writeDrafts(changeId, revision, []);
}

/** Every revision of a change this tab holds drafts on, with how many each. */
export function draftedRevisions(changeId: string): { revision: number; count: number }[] {
  const prefix = `${STORAGE_KEYS.reviewDrafts}${changeId}/`;
  const out: { revision: number; count: number }[] = [];
  try {
    for (let i = 0; i < sessionStorage.length; i++) {
      const key = sessionStorage.key(i);
      if (!key?.startsWith(prefix)) continue;
      const revision = Number(key.slice(prefix.length));
      const count = readDrafts(changeId, revision).length;
      if (Number.isSafeInteger(revision) && count > 0) out.push({ revision, count });
    }
  } catch {
    return [];
  }
  return out.sort((a, b) => a.revision - b.revision);
}

/** A revision's drafts, and the three things a reviewer does with them. */
export interface ReviewDrafts {
  drafts: readonly DraftFinding[];
  add: (draft: Omit<DraftFinding, "id">) => void;
  remove: (id: string) => void;
  /** Forget them all: what a review that carried them does once it lands. */
  clear: () => void;
}

/** The drafts of one revision of one change, kept in the tab's session. */
export function useReviewDrafts(changeId: string, revision: number): ReviewDrafts {
  const [drafts, setDrafts] = useState<DraftFinding[]>(() => readDrafts(changeId, revision));
  // ANOTHER REVISION IS ANOTHER SET, read when the screen moves to it — and
  // read again whenever this tab writes drafts, from wherever it writes them.
  useEffect(() => {
    const hear = () => setDrafts(readDrafts(changeId, revision));
    hear();
    heard.add(hear);
    return () => void heard.delete(hear);
  }, [changeId, revision]);
  const keep = useCallback(
    (next: DraftFinding[]) => writeDrafts(changeId, revision, next),
    [changeId, revision],
  );
  const add = useCallback(
    (draft: Omit<DraftFinding, "id">) =>
      keep([...readDrafts(changeId, revision), { ...draft, id: crypto.randomUUID() }]),
    [changeId, revision, keep],
  );
  const remove = useCallback(
    (id: string) => keep(readDrafts(changeId, revision).filter((d) => d.id !== id)),
    [changeId, revision, keep],
  );
  const clear = useCallback(() => keep([]), [keep]);
  return { drafts, add, remove, clear };
}

/** A draft as the review sends it: every field the tool takes, and nothing of the tab's. */
export function findingOf(draft: DraftFinding): Record<string, unknown> {
  return {
    path: draft.path,
    side: draft.side,
    line_start: draft.line_start,
    line_end: draft.line_end,
    severity: draft.severity,
    category: draft.category,
    title: draft.title,
    ...(draft.body ? { body: draft.body } : {}),
    ...(draft.suggestion !== undefined && draft.side === "new"
      ? { suggestion: draft.suggestion }
      : {}),
  };
}
