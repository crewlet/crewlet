/**
 * The last revision this tab saved from the builder.
 *
 * What these protect: a save's own answer records its epoch, which the apply
 * strip matches each node's applied epoch against, and a later record of the
 * same revision found by settling (which carries no epoch) keeps it.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { clearSavedRevision, recordSavedRevision, useSavedRevision } from "./savedRevision.ts";

afterEach(() => clearSavedRevision());

test("the same revision recorded again without an epoch keeps the one its answer gave", () => {
  const { result } = renderHook(() => useSavedRevision());
  act(() => recordSavedRevision({ revisionId: "r2", parentRevisionId: "r1", epoch: 7 }));
  expect(result.current).toEqual({ revisionId: "r2", parentRevisionId: "r1", epoch: 7 });

  act(() => recordSavedRevision({ revisionId: "r2", parentRevisionId: "r1", epoch: null }));
  expect(result.current?.epoch).toBe(7);

  // Another revision, or a newer epoch for this one, is recorded as it is.
  act(() => recordSavedRevision({ revisionId: "r2", parentRevisionId: "r1", epoch: 8 }));
  expect(result.current?.epoch).toBe(8);
  act(() => recordSavedRevision({ revisionId: "r3", parentRevisionId: "r2", epoch: null }));
  expect(result.current).toEqual({ revisionId: "r3", parentRevisionId: "r2", epoch: null });
});
