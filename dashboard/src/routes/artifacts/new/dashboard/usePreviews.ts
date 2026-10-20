/**
 * The live rows New dashboard draws its figures from, one engine read per
 * distinct source and arguments, each read AS THIS READER through the
 * engine's preview route (`GET /artifacts/sources/preview/{name}`), which
 * answers exactly what a board's dataset over the same source would.
 *
 * A read is made once a figure or a source in view needs it, and kept for the
 * page's life: changing a setting back reads nothing again, and the engine's
 * own cache answers a repeat within its life anyway. A read the engine's
 * budget turned away (`429 read_budget`) is asked again once the wait it
 * named is over. Any other failure is said in one sentence of a reader's,
 * never the route's own: what failed is a preview, and its detail names
 * the route and the node rather than anything a person can act on.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import type { DatasetStatus } from "@crewlethq/ui";
import { RestError, type DashboardSourcePreview } from "~/protocol/index.ts";
import { previewSource } from "~/lib/artifacts/api.ts";
import type { DraftDataset } from "./model.ts";

/** The sentence a preview that could not be read is drawn with. */
export const UNREAD = "These rows could not be read right now. Refresh to try again.";

export interface PreviewState {
  answer: DashboardSourcePreview | null;
  loading: boolean;
  /** A read that failed outside the answer's own statuses, in a reader's words. */
  failure: string | null;
}

export interface Previews {
  /** Every read this page has asked for, by dataset key. */
  states: ReadonlyMap<string, PreviewState>;
  /** Read these again now, every one asked so far when none are named. */
  refresh: (keys?: Iterable<string>) => void;
}

/** A preview's state as the design system draws a dataset's. */
export function previewStatus(s: PreviewState | undefined): DatasetStatus {
  if (!s || (!s.answer && s.loading)) return "loading";
  if (s.failure) return "failed";
  switch (s.answer?.status) {
    case "ready":
      return "ready";
    case "refused":
      return "refused";
    case "unanswered":
      return "failed";
  }
  return "loading";
}

/** The rows a preview answered, counted, or undefined while there are none to count. */
export function previewRows(s: PreviewState | undefined): number | undefined {
  return s?.answer?.status === "ready" ? (s.answer.table?.rows.length ?? 0) : undefined;
}

export function usePreviews(needed: readonly DraftDataset[]): Previews {
  const [states, setStates] = useState<ReadonlyMap<string, PreviewState>>(new Map());
  const asked = useRef(new Map<string, DraftDataset>());
  const controllers = useRef(new Map<string, AbortController>());
  const retries = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  const patch = useCallback((key: string, next: (s: PreviewState) => PreviewState) => {
    setStates((prev) => {
      const out = new Map(prev);
      out.set(key, next(prev.get(key) ?? { answer: null, loading: false, failure: null }));
      return out;
    });
  }, []);

  const read = useCallback(
    (d: DraftDataset) => {
      controllers.current.get(d.key)?.abort();
      clearTimeout(retries.current.get(d.key));
      const controller = new AbortController();
      controllers.current.set(d.key, controller);
      patch(d.key, (s) => ({ ...s, loading: true }));
      previewSource(d.source.name, d.args, controller.signal).then(
        (answer) => {
          if (controller.signal.aborted) return;
          patch(d.key, () => ({ answer, loading: false, failure: null }));
        },
        (err: unknown) => {
          if (controller.signal.aborted) return;
          if (err instanceof RestError && err.status === 429 && err.code === "read_budget") {
            // THE BUDGET REFILLS: keep what is drawn and ask again once it has.
            const wait = Math.max(err.retryAfterSeconds ?? 1, 1) * 1000;
            retries.current.set(
              d.key,
              setTimeout(() => read(d), wait),
            );
            patch(d.key, (s) => ({ ...s, loading: false }));
            return;
          }
          patch(d.key, (s) => ({ ...s, loading: false, failure: UNREAD }));
        },
      );
    },
    [patch],
  );

  // A READ IS MADE THE FIRST TIME A DATASET IS NEEDED, never again on its own.
  const wanted = needed.map((d) => d.key).join("\n");
  useEffect(() => {
    for (const d of needed) {
      if (asked.current.has(d.key)) continue;
      asked.current.set(d.key, d);
      read(d);
    }
    // `wanted` names exactly the datasets `needed` holds, so a new array of
    // the same reads asks for nothing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wanted, read]);

  useEffect(
    () => () => {
      for (const c of controllers.current.values()) c.abort();
      for (const t of retries.current.values()) clearTimeout(t);
    },
    [],
  );

  const refresh = useCallback(
    (keys?: Iterable<string>) => {
      const all = keys ? [...keys] : [...asked.current.keys()];
      for (const k of all) {
        const d = asked.current.get(k);
        if (d) read(d);
      }
    },
    [read],
  );

  return { states, refresh };
}
