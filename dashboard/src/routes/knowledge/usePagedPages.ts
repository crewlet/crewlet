/**
 * `pages`, followed past its first window.
 *
 * A LISTING STOPPED AT FIFTY. The container browse asked for the default window
 * and drew what came back as the container — fifty rows under a container of
 * four hundred, with nothing on screen to say the other three hundred and fifty
 * existed. The answer carries the TOTAL and an `after` cursor now; this is the
 * reader of both, shared by the browse and every level of the tree.
 *
 * # The first window polls and the rest are held
 *
 * The first window is an ordinary `useQuery`, polled. The windows after it are
 * asked ONCE, on "Load more", and held against the question they were asked
 * under — a different container or filter is a different list, so they are
 * dropped rather than appended to it. Re-asking every held window on each poll
 * would multiply the listing's cost by its length. The same rule
 * `routes/work/usePagedItems.ts` keeps for the board, for the same reason.
 *
 * # A page is drawn once
 *
 * The cursor resumes strictly after the last row it was minted on, so a page
 * renamed between two asks can land on both windows. The merge keeps the FIRST
 * occurrence by id.
 */

import { useCallback, useMemo, useState } from "react";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import {
  queryFailure,
  type LogRefusal,
  type PageSummary,
  type PagesAnswer,
  type QueryFailure,
  type QueryRefusal,
} from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { SkillLoad } from "~/contract/pages.ts";

/**
 * How many pages one window asks for: the engine's own ceiling
 * (`pages.MaxLimit`). A tree level or a container is read whole in one window
 * at any size a company's wiki reaches in practice, so "Load more" is the rare
 * case rather than the way a reader walks the tree — and a window is titles,
 * never bodies, so five hundred rows are tens of kilobytes.
 */
export const PAGES_WINDOW = 500;

export interface PagedPages {
  /** The first window as the engine answered it — total, coverage. */
  data: PagesAnswer | null;
  loading: boolean;
  error: QueryErrorCode | null;
  /** Why the first window was refused, beside `error` — what `QueryState` names. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Every page loaded, each once, in the listing's own order. */
  rows: PageSummary[];
  /** How many pages the listing matches in all, or null before an answer. */
  total: number | null;
  /** There is a window after the ones loaded. */
  more: boolean;
  /** Ask for the next window. A no-op when there is none or one is in flight. */
  loadMore: () => void;
  paging: boolean;
  /**
   * Why the last "Load more" failed, until the next one — WHOLE
   * ([queryFailure]), its refusal and the engine's own sentence included, so
   * the screen can name the grant that would admit the reader rather than a
   * bare code.
   */
  pageFailure: QueryFailure | null;
  /**
   * Who each loaded TOOL SKILL reached as a skill, by page id, from every
   * window — present only on a `skills: true` listing on a node that reads the
   * usage domain, and null otherwise (which is not "nobody loaded it").
   */
  loadedBy: Record<string, SkillLoad[]> | null;
}

/** Pages from several windows, each once, first occurrence kept. */
export function mergeWindows(windows: readonly (readonly PageSummary[])[]): PageSummary[] {
  const seen = new Set<string>();
  const out: PageSummary[] = [];
  for (const window of windows) {
    for (const row of window) {
      if (seen.has(row.id)) continue;
      seen.add(row.id);
      out.push(row);
    }
  }
  return out;
}

const NO_WINDOWS: PagesAnswer[] = [];

export function usePagedPages(
  params: Record<string, unknown>,
  options: { pollMs: number; enabled?: boolean },
): PagedPages {
  const { socket } = useClient();
  const asked = useMemo(() => ({ limit: PAGES_WINDOW, ...params }), [params]);
  const first = useQuery("pages", asked, {
    pollMs: options.pollMs,
    enabled: options.enabled ?? true,
  });
  const question = JSON.stringify(asked);
  const [held, setHeld] = useState<{ question: string; windows: PagesAnswer[] } | null>(null);
  const [paging, setPaging] = useState(false);
  const [pageFailure, setPageFailure] = useState<QueryFailure | null>(null);
  const windows = held && held.question === question ? held.windows : NO_WINDOWS;
  const last = windows.length > 0 ? windows[windows.length - 1] : first.data;
  const after = last?.after ?? "";

  const load = useCallback(async () => {
    if (!after || paging) return;
    setPaging(true);
    setPageFailure(null);
    try {
      const window = await socket.query(
        "pages",
        withFloor("pages", JSON.stringify({ ...asked, after })),
      );
      setHeld((prev) =>
        prev && prev.question === question
          ? { question, windows: [...prev.windows, window] }
          : { question, windows: [window] },
      );
    } catch (err) {
      setPageFailure(queryFailure(err));
    } finally {
      setPaging(false);
    }
  }, [socket, asked, question, after, paging]);

  const rows = useMemo(
    () => mergeWindows([first.data?.pages ?? [], ...windows.map((w) => w.pages ?? [])]),
    [first.data, windows],
  );

  const loadedBy = useMemo(() => {
    const answers = [first.data, ...windows].filter((w): w is PagesAnswer => Boolean(w));
    if (!answers.some((w) => w.skill_loaded_by)) return null;
    return Object.assign({}, ...answers.map((w) => w.skill_loaded_by ?? {})) as Record<
      string,
      SkillLoad[]
    >;
  }, [first.data, windows]);

  return {
    loadedBy,
    data: first.data,
    loading: first.loading,
    error: first.error,
    refusal: first.refusal,
    rows,
    total: first.data ? (first.data.total ?? rows.length) : null,
    more: after !== "",
    loadMore: () => void load(),
    paging,
    pageFailure,
  };
}
