/**
 * A notice read past its first page, for the lists the Inbox opens from its
 * two quiet links (Snoozed and Earlier).
 *
 * PAGED FORWARD, NEVER CUT. The engine answers fifty notices at a time
 * (`tracker.MaxInboxRows`) with a cursor behind a full page, so a list that
 * read one page and stopped would hide the fifty-first snooze for as long as
 * the first fifty stood. Each "Show more" reads the page after the last one
 * shown, and the pages are kept in the order they were read.
 *
 * ONE READER PER PAGE, as a component, because a hook cannot be called a
 * varying number of times: the screen renders `readers` and each reports its
 * page back. Only the first page is polled; a later page is history.
 */

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useQuery } from "~/lib/useQuery.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { WorkInboxAnswer, WorkInboxNotice } from "~/protocol/index.ts";

/** One page as it was answered. */
interface Page {
  notices: WorkInboxNotice[];
  next: string | null;
}

export interface Paged {
  /** Every page's notices in the order read, or null until the first answers. */
  notices: WorkInboxNotice[] | null;
  /** The first page's answer, for what it says about the whole. */
  first: WorkInboxAnswer | null;
  /** More lie past the last page read. */
  more: boolean;
  /** A page's read was refused, by the engine's own code. */
  error: QueryErrorCode | null;
  /** Read the page after the last one. */
  next: () => void;
  /** The readers to render, one per page; they draw nothing. */
  readers: ReactNode;
}

type Params = Record<string, string | number | boolean>;

export function usePagedInbox(params: Params | undefined, pollMs: number): Paged {
  // THE PAGES BELONG TO ONE QUESTION. Asked a different one (another person,
  // another scope), the cursors of the last are meaningless and are dropped.
  const identity = params ? JSON.stringify(params) : "";
  const [cursors, setCursors] = useState<{ of: string; list: string[] }>({
    of: identity,
    list: [""],
  });
  const list = cursors.of === identity ? cursors.list : [""];
  const [pages, setPages] = useState<ReadonlyMap<string, Page>>(new Map());
  const [first, setFirst] = useState<{ of: string; answer: WorkInboxAnswer } | null>(null);
  const [error, setError] = useState<{ of: string; code: QueryErrorCode } | null>(null);

  const onPage = useCallback(
    (of: string, cursor: string, answer: WorkInboxAnswer | null, code: QueryErrorCode | null) => {
      if (code) {
        setError({ of, code });
        return;
      }
      if (!answer) return;
      setError((was) => (was?.of === of ? null : was));
      if (!cursor) setFirst({ of, answer });
      setPages((was) => {
        const next = new Map(was);
        next.set(`${of}|${cursor}`, { notices: answer.notices, next: answer.next_cursor ?? null });
        return next;
      });
    },
    [],
  );

  const read = list.map((cursor) => pages.get(`${identity}|${cursor}`));
  const notices = read[0] ? read.flatMap((page) => page?.notices ?? []) : null;
  const last = read[read.length - 1];
  const following = last?.next ?? null;

  const next = useCallback(() => {
    if (!following) return;
    setCursors((was) => {
      const base = was.of === identity ? was.list : [""];
      return base.includes(following) ? was : { of: identity, list: [...base, following] };
    });
  }, [following, identity]);

  const readers = params
    ? list.map((cursor) => (
        <PageReader
          key={`${identity}|${cursor}`}
          of={identity}
          params={params}
          cursor={cursor}
          pollMs={cursor ? undefined : pollMs}
          onPage={onPage}
        />
      ))
    : null;

  return {
    notices: params ? notices : null,
    first: first?.of === identity ? first.answer : null,
    more: Boolean(following),
    error: error?.of === identity ? error.code : null,
    next,
    readers,
  };
}

function PageReader({
  of,
  params,
  cursor,
  pollMs,
  onPage,
}: {
  of: string;
  params: Params;
  cursor: string;
  pollMs: number | undefined;
  onPage: (
    of: string,
    cursor: string,
    answer: WorkInboxAnswer | null,
    code: QueryErrorCode | null,
  ) => void;
}) {
  const read = useQuery(
    "work_inbox",
    cursor ? { ...params, cursor } : params,
    pollMs ? { pollMs } : {},
  );
  useEffect(() => {
    onPage(of, cursor, read.data, read.error);
  }, [of, cursor, read.data, read.error, onPage]);
  return null;
}
