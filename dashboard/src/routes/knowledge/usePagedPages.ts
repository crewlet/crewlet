/**
 * `pages`, followed past its first window.
 *
 * A LISTING STOPPED AT FIFTY. The container browse asked for the default window
 * and drew what came back as the container — fifty rows under a container of
 * four hundred, with nothing on screen to say the other three hundred and fifty
 * existed. The answer carries the TOTAL and an `after` cursor now; this is the
 * reader of both, shared by the browse and every level of the tree.
 *
 * # The first window polls until "Load more", and is then held
 *
 * The first window is an ordinary `useQuery`, polled. The windows after it are
 * asked ONCE, on "Load more", and a different container or filter is a
 * different list, so they are dropped rather than appended to it. Re-asking
 * every held window on each poll would multiply the listing's cost by its
 * length. The same rule `routes/work/usePagedItems.ts` keeps for the board, for
 * the same reason.
 *
 * So from "Load more" the first window is HELD too (`lib/usePaged.ts`): the
 * next one starts after the last page the first was answered with, and a page
 * filed into its range would push that one out of both when the first was read
 * again on its poll. A write from this tab still reads it again, and the
 * windows loaded are then read again from it.
 *
 * # A page is drawn once
 *
 * The cursor resumes strictly after the last row it was minted on, so a page
 * renamed between two asks can land on both windows. The merge keeps the FIRST
 * occurrence by id (`lib/usePaged.ts`).
 */

import { useMemo } from "react";
import { useClient } from "~/lib/store-hooks.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { LogRefusal, PageSummary, PagesAnswer, QueryRefusal } from "~/protocol/index.ts";
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
  /** The first window as the engine answered it — total, coverage: the one held, while one is. */
  data: PagesAnswer | null;
  loading: boolean;
  error: QueryErrorCode | null;
  /** What the first window's refusal said beyond its code — see `useQuery`. */
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
  /** Whether the first window is held, so the listing is one reading: see the file's doc. */
  held: boolean;
  /** Why the last "Load more" failed, until the next one. */
  pageError: QueryErrorCode | null;
  /**
   * Who each loaded TOOL SKILL reached as a skill, by page id, from every
   * window — present only on a `skills: true` listing on a node that reads the
   * usage domain, and null otherwise (which is not "nobody loaded it").
   */
  loadedBy: Record<string, SkillLoad[]> | null;
}

/** A window's pages, each page's id, and the cursor to the window after. */
const pagesOf = (w: PagesAnswer) => w.pages ?? [];
const pageIdOf = (row: PageSummary) => row.id;
const pagesAfter = (w: PagesAnswer) => w.after ?? "";

export function usePagedPages(
  params: Record<string, unknown>,
  options: { pollMs: number; enabled?: boolean },
): PagedPages {
  const { socket } = useClient();
  const asked = useMemo(() => ({ limit: PAGES_WINDOW, ...params }), [params]);
  const hold = usePageHold<PagesAnswer>(asked);
  const first = useQuery("pages", asked, {
    pollMs: options.pollMs,
    enabled: options.enabled ?? true,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (after) => socket.query("pages", withFloor("pages", JSON.stringify({ ...asked, after }))),
    pagesOf,
    pageIdOf,
    pagesAfter,
  );

  const loadedBy = useMemo(() => {
    if (!paged.answers.some((w) => w.skill_loaded_by)) return null;
    return Object.assign({}, ...paged.answers.map((w) => w.skill_loaded_by ?? {})) as Record<
      string,
      SkillLoad[]
    >;
  }, [paged.answers]);

  // THE FIRST WINDOW AS DRAWN: the one held, while one is, so the total is the
  // one the rows were read under.
  const data = paged.answers[0] ?? null;
  return {
    loadedBy,
    data,
    loading: paged.loading,
    error: paged.error,
    refusal: paged.refusal,
    rows: paged.items,
    total: data ? (data.total ?? paged.items.length) : null,
    more: paged.more,
    loadMore: paged.older,
    paging: paged.paging,
    held: paged.held,
    pageError: paged.pageError,
  };
}
