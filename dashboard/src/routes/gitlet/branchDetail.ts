/**
 * One branch read in detail by its name, and its protection in words: a
 * repository's default branch on its pages, a change's target on its rail.
 */

import { useEffect, useMemo, useState } from "react";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import type {
  CodeRefListing,
  CodeRefProtection,
  CodeRefsAnswer,
  QueryRefusal,
  LogRefusal,
} from "~/protocol/index.ts";

const HEADS = "refs/heads/";
const TAGS = "refs/tags/";

/** A find's page: a name matches any part of others', so the exact one may be on a later page. */
const FIND_LIMIT = 20;

export interface BranchDetail {
  /** The branch's row, or null where no page holds it (yet). */
  row: CodeRefListing | null;
  loading: boolean;
  error: QueryErrorCode | null;
  refusal: QueryRefusal | LogRefusal | null;
  detail: string | undefined;
}

/** One branch read in detail by its name: the find's pages walked until its exact ref is met. */
export function useBranchDetail(repo: string, branch: string, enabled = true): BranchDetail {
  return useRefDetail(repo, HEADS + branch, enabled);
}

/** One branch or tag read in detail by its whole ref, as [useBranchDetail] reads a branch. */
export function useRefDetail(repo: string, want: string, enabled = true): BranchDetail {
  const tag = want.startsWith(TAGS);
  const params = useMemo(
    () => ({
      name: repo,
      ...(tag ? { prefix: TAGS } : {}),
      contains: want.slice(tag ? TAGS.length : HEADS.length),
      detail: true,
      limit: FIND_LIMIT,
    }),
    [repo, want, tag],
  );
  const first = useCodeQuery("code_refs", params, { enabled });
  const inFirst = first.data?.refs_detail?.find((r) => r.ref === want);
  const next = first.data?.next ?? "";
  const key = `${JSON.stringify(params)}\n${next}`;
  const [later, setLater] = useState<{ key: string; row: CodeRefListing | null } | null>(null);
  useEffect(() => {
    if (inFirst || !next || later?.key === key) return undefined;
    let live = true;
    void (async () => {
      let cursor = next;
      let row: CodeRefListing | null = null;
      while (cursor && !row) {
        const page: CodeRefsAnswer | null = await codeRest
          .query("code_refs", { ...params, cursor })
          .catch(() => null);
        row = page?.refs_detail?.find((r) => r.ref === want) ?? null;
        cursor = page?.next ?? "";
      }
      if (live) setLater({ key, row });
    })();
    return () => {
      live = false;
    };
  }, [inFirst, next, key, later, params, want]);
  const searching = !inFirst && next !== "" && later?.key !== key;
  return {
    row: inFirst ?? (later?.key === key ? later.row : null),
    loading: first.loading || searching,
    error: first.error,
    refusal: first.refusal,
    detail: first.detail,
  };
}

/** A branch's rules as clauses: "no force-push, no delete, pushes through changes only". */
export function protectionWords(p: CodeRefProtection): string {
  return [
    p.allow_force ? "force-push allowed" : "no force-push",
    p.allow_delete ? "delete allowed" : "no delete",
    p.direct_push === "none"
      ? "pushes through changes only"
      : p.direct_push === "persons"
        ? "persons push directly"
        : `direct pushes by ${p.direct_push}`,
  ].join(", ");
}

/** A branch's rules in words: "Protected by release/*: no force-push, no delete, pushes through changes only". */
export function protectionTitle(p: CodeRefProtection): string {
  const names = p.patterns.map((pattern) =>
    pattern.startsWith(HEADS) ? pattern.slice(HEADS.length) : pattern,
  );
  return `Protected by ${names.join(" and ")}: ${protectionWords(p)}`;
}
