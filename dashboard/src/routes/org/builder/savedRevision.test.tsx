/**
 * The last revision this tab saved from the builder.
 *
 * What these protect: a save's own answer records its epoch, which the apply
 * strip matches each node's applied epoch against, and a later record of the
 * same revision found by settling (which carries no epoch) keeps it, and an
 * epoch learned for a save without one is kept for that save alone.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import {
  clearSavedRevision,
  pinSavedEpoch,
  recordSavedRevision,
  useSavedRevision,
} from "./savedRevision.ts";

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

// AN EPOCH LEARNED FOR A SAVE WITHOUT ONE is kept for that save, and only
// while it has none: the strip pins it from an effect, and a newer save
// recorded before that effect ran is not the one it learned about. Mutation:
// pin whatever the store holds and the newer save takes the older one's
// epoch, or a known epoch is overwritten.
test("an epoch learned for a save is pinned to that save alone, once", () => {
  const { result } = renderHook(() => useSavedRevision());
  act(() => recordSavedRevision({ revisionId: "r2", parentRevisionId: "r1", epoch: null }));
  act(() => pinSavedEpoch("r2", 12));
  expect(result.current).toEqual({ revisionId: "r2", parentRevisionId: "r1", epoch: 12 });

  // Learned once: a later answer naming another epoch does not move it.
  act(() => pinSavedEpoch("r2", 13));
  expect(result.current?.epoch).toBe(12);

  // A pin for a save this store no longer holds changes nothing.
  act(() => recordSavedRevision({ revisionId: "r3", parentRevisionId: "r2", epoch: null }));
  act(() => pinSavedEpoch("r2", 14));
  expect(result.current).toEqual({ revisionId: "r3", parentRevisionId: "r2", epoch: null });
});
