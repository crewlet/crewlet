/**
 * A paged history keeps WHY a page failed, not only that it did.
 *
 * `QueryState` names the grant that would admit the reader, and says when the
 * state log will not lift a refusal, only from the refusal handed beside the
 * code. The paging hook kept a bare code for an older page and nothing at all
 * for the newest one's refusal, so every screen built on it drew the generic
 * banner where the engine had said exactly what would change the answer.
 */

import { expect, test } from "vitest";

import { act, renderHook } from "~/test/inCase.ts";
import { usePaged } from "./usePaged.ts";
import type { QueryResult } from "./useQuery.ts";
import { QueryRefusedError, type QueryRefusal } from "~/protocol/index.ts";

interface Page {
  rows: { id: string }[];
  next: string;
}

const REFUSAL: QueryRefusal = { reason: "owner_or_lead", grants: ["audit:read"] };

function first(over: Partial<QueryResult<Page>> = {}): QueryResult<Page> {
  return {
    data: { rows: [{ id: "a" }], next: "c1" },
    loading: false,
    error: null,
    refusal: null,
    detail: null,
    asked: null,
    refetch: () => {},
    ...over,
  } as QueryResult<Page>;
}

const pick = (page: Page) => page.rows;
const idOf = (row: { id: string }) => row.id;
const cursorOf = (page: Page) => page.next;

test("an older page refused on authority keeps the refusal it was refused with", async () => {
  const page = () => Promise.reject(new QueryRefusedError("unauthorized", REFUSAL, null));
  const { result } = renderHook(() => usePaged(first(), { id: "t-1" }, page, pick, idOf, cursorOf));
  await act(async () => {
    result.current.older();
  });
  expect(result.current.pageFailure).toEqual({
    error: "unauthorized",
    refusal: REFUSAL,
    detail: null,
  });
});

test("the newest page's refusal travels beside its code", () => {
  const { result } = renderHook(() =>
    usePaged(
      first({ data: null, error: "unauthorized", refusal: REFUSAL }),
      { id: "t-1" },
      () => Promise.reject(new Error("never asked")),
      pick,
      idOf,
      cursorOf,
    ),
  );
  expect(result.current.error).toBe("unauthorized");
  expect(result.current.refusal).toEqual(REFUSAL);
});
