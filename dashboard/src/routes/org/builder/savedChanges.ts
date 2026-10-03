/**
 * What this tab last saved from the org builder, for as long as the tab lives.
 *
 * A SAVE IS NOT AN APPLY, and it is two of them. The settings answer once a
 * revision is stored and activated, and each node then applies it on its own
 * reconcile tick, and may refuse it. The chart answers once its records are
 * durable, and each node applies them from the log — this one before it
 * answered `200`, or not yet when it answered `202`. Until both have been
 * applied here, the org projection every read screen draws from can still
 * describe the company before the save, and the screen the operator switches
 * to right after saving has to say so. The Builder is unmounted by that
 * switch, so what it saved is kept here, outside any one screen, rather than
 * in the Builder's own state.
 *
 * In memory only: a reload starts without it, and Settings › Nodes and
 * Settings › Backups & retention remain the places to read where every node
 * stands.
 */

import { useSyncExternalStore } from "react";

/** A settings revision a save activated. */
export interface SavedSettings {
  readonly revisionId: string;
  /**
   * The revision the save was built on, which is the saved revision's parent;
   * `null` for the company's first revision. What the save changed is the
   * saved revision against this one: against the active revision, a save
   * that is active now differs from nothing.
   */
  readonly parentRevisionId: string | null;
  /**
   * The epoch the write activated, from its answer. `null` when the save's
   * answer was lost and the write was found afterwards by reading the
   * revision history, which carries no epoch.
   */
  readonly epoch: number | null;
}

/** The chart writes a save made. */
export interface SavedChart {
  /** The furthest position they reached on the chart's log: `STREAM@generation:seq`. */
  readonly position: string;
  /**
   * Whether this node has applied them: every write answered `applied`, or
   * the chart was read back here afterwards (a chart read is linearizable,
   * so its answer includes every write before it).
   */
  readonly appliedHere: boolean;
}

export interface SavedChanges {
  readonly settings: SavedSettings | null;
  readonly chart: SavedChart | null;
}

let current: SavedChanges | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

/**
 * Records a save.
 *
 * ONE SAVE RECORDED TWICE KEEPS WHAT WAS KNOWN OF IT. The settings revision
 * a save's own answer recorded carries its epoch, and a later record of the
 * same revision found by settling carries none: the epoch is what the strip
 * matches a node's applied epoch against, so the second record keeps it. And
 * a chart position once applied here stays applied.
 */
export function recordSavedChanges(saved: SavedChanges): void {
  const settings =
    saved.settings &&
    saved.settings.epoch === null &&
    current?.settings?.revisionId === saved.settings.revisionId
      ? { ...saved.settings, epoch: current.settings.epoch }
      : saved.settings;
  const chart =
    saved.chart && current?.chart?.position === saved.chart.position
      ? { ...saved.chart, appliedHere: saved.chart.appliedHere || current.chart.appliedHere }
      : saved.chart;
  current = { settings, chart };
  emit();
}

/** Marks the recorded chart writes applied on this node: the chart was read back here. */
export function markChartAppliedHere(): void {
  if (!current?.chart || current.chart.appliedHere) return;
  current = { ...current, chart: { ...current.chart, appliedHere: true } };
  emit();
}

/** Forgets the last save, once the operator dismisses its status. */
export function clearSavedChanges(): void {
  if (current === null) return;
  current = null;
  emit();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The last save of this tab, or `null`. */
export function useSavedChanges(): SavedChanges | null {
  return useSyncExternalStore(
    subscribe,
    () => current,
    () => null,
  );
}
