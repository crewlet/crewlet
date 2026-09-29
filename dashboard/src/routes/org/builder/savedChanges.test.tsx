/**
 * What this tab last saved from the builder: a settings revision, the chart's
 * writes, or both.
 *
 * What these protect: a save's own answer records its epoch, which the apply
 * strip matches each node's applied epoch against, and a later record of the
 * same revision found by settling (which carries no epoch) keeps it; and the
 * chart's writes, once applied on this node, stay applied when the same save
 * is recorded again from an answer that said otherwise.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import {
  clearSavedChanges,
  markChartAppliedHere,
  recordSavedChanges,
  useSavedChanges,
} from "./savedChanges.ts";

afterEach(() => clearSavedChanges());

const POSITION = "CREWLET_CHART_LOG@1:12";

test("the same revision recorded again without an epoch keeps the one its answer gave", () => {
  const { result } = renderHook(() => useSavedChanges());
  const settings = (revisionId: string, parentRevisionId: string, epoch: number | null) => ({
    settings: { revisionId, parentRevisionId, epoch },
    chart: null,
  });
  act(() => recordSavedChanges(settings("r2", "r1", 7)));
  expect(result.current?.settings).toEqual({ revisionId: "r2", parentRevisionId: "r1", epoch: 7 });

  act(() => recordSavedChanges(settings("r2", "r1", null)));
  expect(result.current?.settings?.epoch).toBe(7);

  // Another revision, or a newer epoch for this one, is recorded as it is.
  act(() => recordSavedChanges(settings("r2", "r1", 8)));
  expect(result.current?.settings?.epoch).toBe(8);
  act(() => recordSavedChanges(settings("r3", "r2", null)));
  expect(result.current?.settings).toEqual({
    revisionId: "r3",
    parentRevisionId: "r2",
    epoch: null,
  });
});

test("chart writes applied here stay applied, and a later save's are recorded as they are", () => {
  const { result } = renderHook(() => useSavedChanges());
  act(() =>
    recordSavedChanges({ settings: null, chart: { position: POSITION, appliedHere: false } }),
  );
  expect(result.current?.chart).toEqual({ position: POSITION, appliedHere: false });

  // The read back is linearizable, so this node has applied what it read.
  act(() => markChartAppliedHere());
  expect(result.current?.chart?.appliedHere).toBe(true);

  // The same save recorded again from an answer of `pending` does not undo it.
  act(() =>
    recordSavedChanges({ settings: null, chart: { position: POSITION, appliedHere: false } }),
  );
  expect(result.current?.chart?.appliedHere).toBe(true);

  // The control: a save that reached further is a different save.
  const later = "CREWLET_CHART_LOG@1:15";
  act(() => recordSavedChanges({ settings: null, chart: { position: later, appliedHere: false } }));
  expect(result.current?.chart).toEqual({ position: later, appliedHere: false });
});

test("marking the chart applied changes nothing when no chart write was recorded", () => {
  const { result } = renderHook(() => useSavedChanges());
  act(() => markChartAppliedHere());
  expect(result.current).toBeNull();
  act(() =>
    recordSavedChanges({
      settings: { revisionId: "r2", parentRevisionId: "r1", epoch: 3 },
      chart: null,
    }),
  );
  const before = result.current;
  act(() => markChartAppliedHere());
  expect(result.current).toBe(before);
});
