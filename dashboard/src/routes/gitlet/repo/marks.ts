/**
 * Each loaded history page's checks and changes, asked once per page; asked
 * again each minute only while a check on screen is still running.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { codeRest } from "~/lib/useCodeQuery.ts";
import { QueryError, type CodeCommitMark } from "~/protocol/index.ts";

/** How often a page holding a running or pending rollup is asked again: the code log's minute. */
export const MARKS_POLL_MS = 60_000;

/** What a page of marks answered: each commit's marks, or why none came. */
interface PageMarks {
  marks?: CodeCommitMark[];
  failed?: MarksFailure;
}

/** Why the marks were not read: the refusal's code and the engine's sentence, where it wrote one. */
export interface MarksFailure {
  code: string;
  sentence: string | null;
}

export interface CommitMarks {
  /** A commit's marks: undefined until its page answers, null where the answer holds none. */
  of: (oid: string) => CodeCommitMark | null | undefined;
  /** The first failure among the pages asked, until they are asked again. */
  failed: MarksFailure | null;
  retry: () => void;
}

/** A page's key: the repository and the whole ids it names. */
const keyOf = (repo: string, oids: readonly string[]) => `${repo}\n${oids.join(",")}`;

const unsettled = (marks: readonly CodeCommitMark[] | undefined) =>
  (marks ?? []).some((m) =>
    (m.checks ?? []).some((c) => c.state === "running" || c.state === "pending"),
  );

function failureOf(err: unknown): MarksFailure {
  if (!(err instanceof QueryError)) return { code: "query_failed", sentence: null };
  const refusal = err.refusal as { detail?: string | null } | null;
  return { code: err.message, sentence: err.detail ?? refusal?.detail ?? null };
}

/** The marks of every list of whole commit ids given (a history's pages, a page of branch heads). */
export function useCommitMarks(repo: string, pages: readonly (readonly string[])[]): CommitMarks {
  const [held, setHeld] = useState<Readonly<Record<string, PageMarks>>>({});
  const asking = useRef(new Map<string, AbortController>());
  const keys = useMemo(
    () =>
      pages
        .filter((oids) => oids.length > 0)
        .map((oids) => ({ key: keyOf(repo, oids), oids: oids.join(",") })),
    [repo, pages],
  );

  const ask = useCallback(
    (key: string, oids: string) => {
      if (asking.current.has(key)) return;
      const abort = new AbortController();
      asking.current.set(key, abort);
      codeRest
        .query("code_commit_marks", { name: repo, commits: oids }, abort.signal)
        .then(
          (answer): PageMarks => ({ marks: answer.commit_marks ?? [] }),
          (err: unknown): PageMarks => ({ failed: failureOf(err) }),
        )
        .then((page) => {
          if (abort.signal.aborted) return;
          asking.current.delete(key);
          setHeld((h) => ({ ...h, [key]: page }));
        });
    },
    [repo],
  );

  // EACH PAGE ONCE: a page already answered, or on its way, is not asked again here.
  useEffect(() => {
    for (const { key, oids } of keys) if (!(key in held)) ask(key, oids);
  }, [keys, held, ask]);

  // A PAGE LEFT (another repository, a new history) is let go of.
  useEffect(() => {
    const live = new Set(keys.map((k) => k.key));
    for (const [key, abort] of asking.current) {
      if (!live.has(key)) {
        abort.abort();
        asking.current.delete(key);
      }
    }
  }, [keys]);
  useEffect(() => {
    const running = asking.current;
    return () => {
      for (const abort of running.values()) abort.abort();
      running.clear();
    };
  }, []);

  // ASKED AGAIN EACH MINUTE only while a page holds a check still running or pending.
  const polling = keys.filter(({ key }) => unsettled(held[key]?.marks));
  const pollingNow = useRef(polling);
  pollingNow.current = polling;
  const pollKey = polling.map((p) => p.key).join("\n\n");
  useEffect(() => {
    if (pollKey === "") return undefined;
    const tick = setInterval(() => {
      for (const { key, oids } of pollingNow.current) ask(key, oids);
    }, MARKS_POLL_MS);
    return () => clearInterval(tick);
  }, [pollKey, ask]);

  const byOid = useMemo(() => {
    const out = new Map<string, CodeCommitMark | null>();
    for (const { key, oids } of keys) {
      const page = held[key];
      if (!page?.marks) continue;
      for (const oid of oids.split(",")) out.set(oid, null);
      for (const m of page.marks) out.set(m.commit, m);
    }
    return out;
  }, [keys, held]);

  const failed = keys.map(({ key }) => held[key]?.failed).find((f) => f !== undefined) ?? null;

  const retry = useCallback(() => {
    setHeld((h) => {
      const next: Record<string, PageMarks> = {};
      for (const [key, page] of Object.entries(h)) if (!page.failed) next[key] = page;
      return next;
    });
  }, []);

  return { of: (oid) => byOid.get(oid), failed, retry };
}
