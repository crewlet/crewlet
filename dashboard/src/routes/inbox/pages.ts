/**
 * A notice read past its first page, for the lists the Inbox opens from its
 * two quiet links (Snoozed and Earlier).
 *
 * PAGED FORWARD, NEVER CUT. The engine answers fifty notices at a time
 * (`inbox.MaxRows`) with a cursor behind a full page, so a list that
 * read one page and stopped would hide the fifty-first snooze for as long as
 * the first fifty stood. Each "Show more" reads the page after the last one
 * shown, through the pager every paged list shares (`lib/usePaged.ts`).
 *
 * THE FIRST PAGE IS PUSHED AS WELL AS POLLED: the engine sends
 * `inbox_changed` when a committed batch moves the record it is kept under
 * (`refetchOnInboxOf`), so the Snoozed count moves with the snooze rather than
 * a poll later. The poll stays, because a frame lost to a reconnect is never
 * sent again.
 *
 * UNTIL "SHOW MORE". The next page starts after the last notice the first was
 * answered with, and the first read again under a newer notice pushes that
 * one out of both, so from the press the first page is HELD: neither its poll
 * nor a push asks it again. A mark made from this tab still does, and the
 * pages shown are then read again from it, so the mark is drawn wherever the
 * notice is. It was one reader per page, each its own `useQuery` asked again
 * from its own fixed cursor — the hole, on every page boundary.
 */

import { useClient } from "~/lib/store-hooks.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type {
  LogRefusal,
  QueryRefusal,
  WorkInboxAnswer,
  WorkInboxNotice,
} from "~/protocol/index.ts";

export interface Paged {
  /** Every page's notices in the order read, or null until the first answers. */
  notices: WorkInboxNotice[] | null;
  /** The first page's answer, for what it says about the whole: the one held, while one is. */
  first: WorkInboxAnswer | null;
  /** More lie past the last page read. */
  more: boolean;
  /** A page's read was refused, by the engine's own code. */
  error: QueryErrorCode | null;
  /** Why, where the engine said: the grant that would admit, or the log's refusal. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Read the page after the last one. */
  next: () => void;
}

type Params = Record<string, string | number | boolean>;

/** No question: the list is closed, or nobody is bound to read it as. */
const NO_PARAMS: Params = {};

/** A page's notices, the history row each came from, and the cursor after them. */
const noticesOf = (a: WorkInboxAnswer) => a.notices ?? [];
const noticeIdOf = (n: WorkInboxNotice) => n.record_id;
const noticesAfter = (a: WorkInboxAnswer) => a.next_cursor ?? "";

/**
 * `inboxOf` is the record the notices are kept under (`ViewerState.owner`),
 * whose `inbox_changed` push asks the first page again.
 */
export function usePagedInbox(params: Params | undefined, pollMs: number, inboxOf: string): Paged {
  const { socket } = useClient();
  // THE PAGES BELONG TO ONE QUESTION. Asked a different one (another person,
  // another scope), the cursors of the last are meaningless and are dropped.
  const asked = params ?? NO_PARAMS;
  const hold = usePageHold<WorkInboxAnswer>(asked);
  const first = useQuery("work_inbox", asked, {
    enabled: params !== undefined,
    pollMs,
    refetchOnInboxOf: inboxOf,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) =>
      socket.query("work_inbox", withFloor("work_inbox", JSON.stringify({ ...asked, cursor }))),
    noticesOf,
    noticeIdOf,
    noticesAfter,
  );

  return {
    notices: params && paged.answers.length > 0 ? paged.items : null,
    first: params ? (paged.answers[0] ?? null) : null,
    more: params !== undefined && paged.more,
    error: paged.error ?? paged.pageError,
    refusal: paged.error ? paged.refusal : null,
    next: paged.older,
  };
}
