/**
 * Narrowing a rejection to the query error vocabulary.
 */

import { expect, test } from "vitest";

import { queryErrorCode, queryFailure, QueryRefusedError } from "./index.ts";

test.each(["unknown_query", "unauthorized", "unavailable", "not_found", "timeout", "closed"])(
  "%s is a query error code",
  (code) => {
    expect(queryErrorCode(code)).toBe(code);
  },
);

// NOT A CODE, NOT A BRANCH. The dashboard used to handle `no_event_store`,
// which the engine never sent; prose a screen wrote itself is not a code
// either, and neither is an empty or absent error.
test.each(["no_event_store", "The engine refused the read", "", null, undefined])(
  "%s is not a query error code",
  (value) => {
    expect(queryErrorCode(value)).toBeNull();
  },
);

// An own key, not an inherited one: `in` would have accepted every name an
// object inherits, and a message of `toString` would have read as a code.
test("a name the code table only inherits is not a code", () => {
  expect(queryErrorCode("toString")).toBeNull();
  expect(queryErrorCode("constructor")).toBeNull();
});

// A REJECTION READ ONCE, its refusal included: a page of older rows asked
// outside `useQuery` kept the code and dropped the grants the engine named.
test("a failed query is its code and the refusal behind it", () => {
  const refusal = { reason: "operator", grants: ["audit:read"] };
  expect(queryFailure(new QueryRefusedError("unauthorized", refusal))).toEqual({
    error: "unauthorized",
    refusal,
    detail: null,
  });
  expect(queryFailure(new QueryRefusedError("bad_params", null, "days: at most 90"))).toEqual({
    error: "bad_params",
    refusal: null,
    detail: "days: at most 90",
  });
  expect(queryFailure(new Error("timeout"))).toEqual({
    error: "timeout",
    refusal: null,
    detail: null,
  });
  expect(queryFailure("not an error")).toEqual({
    error: "query_failed",
    refusal: null,
    detail: null,
  });
});
