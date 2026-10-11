/**
 * The guarded half of one seat — the company document's entry for it — as the
 * console's seat page reads it.
 *
 * THE CONSOLE'S, AND ONLY THE CONSOLE'S. The document is an admin's
 * (ADR-0031), so a hook that asks it is a read the person shell must never
 * make (`app/shell.test.ts` walks every module a person-shell chunk reaches
 * and refuses a question above `member`). It sat in `lib/seats.ts` beside
 * the pure halves it composes — [seatSettings] and [seatReading], which stay
 * there because they ask nothing — and that put a `config` read in the
 * import graph of every person screen that indexed the chart.
 */

import { useMemo } from "react";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { knownRefusal, useViewer } from "~/lib/viewer.ts";
import {
  indexOrg,
  seatReading,
  seatSettings,
  type Seat,
  type SeatReading,
  type SeatSettings,
} from "~/lib/seats.ts";
import { apiToken } from "~/protocol/index.ts";
import type { CompanyDocument } from "~/protocol/index.ts";

/**
 * What [useSeatSetup] answers. A type of its own so a screen that READ it once
 * can hand it to every panel drawing it: `useQuery` shares no request between
 * two callers, so a panel calling the hook again fetches the whole guarded
 * document a second time.
 */
export interface SeatSetup {
  seat: Seat | undefined;
  settings: SeatSettings | null;
  reading: SeatReading;
  /** The raw answer, for a panel that states its own read: the document
   *  (never beside a refusal), whether it is in flight, and the refusal. */
  config: { doc: CompanyDocument | null; loading: boolean; error: string | null };
}

/**
 * The guarded half of one seat, for any screen that draws it: the company
 * document's entry for the seat and the four-state reading of it.
 *
 * ONE READ, ONE ORDER. The document is an admin's (ADR-0031), and every
 * screen that read it on its own either forgot the refusal-first order
 * [seatReading] exists for — and printed a revoked reader's model beside a
 * banner saying the answer needs a token — or collapsed "absent", "refused"
 * and "not read yet" into one sentence. The seat is resolved by its handle
 * alone, the address [seatPath] builds (`OrgIndex.byHandle`).
 *
 * A reader the engine would refuse is never asked: see [knownRefusal].
 */
export function useSeatSetup(handle: string): SeatSetup {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = handle ? index.byHandle.get(handle) : undefined;
  // NOT ASKED WHERE THE ANSWER IS KNOWN: the document is an admin's.
  const known = knownRefusal("config", useViewer(), apiToken() !== "");
  const config = useQuery("config", undefined, { enabled: !!seat && known === null });
  // THE REFUSAL THE ENGINE WOULD ANSWER stands in for the one it was not
  // asked for, so every panel says the same thing for both. Reported as
  // nothing, a reader never asked read "No company configuration is active"
  // on the Settings tab — a claim about the company made from no answer.
  const error = known ?? config.error;
  // NOTHING FROM THE DOCUMENT BESIDE A REFUSAL: `useQuery` keeps its last good
  // answer through a failed ask, which suits a poll and is wrong for a guarded
  // read.
  const settings = useMemo<SeatSettings | null>(
    () => (seat && config.data && !error ? seatSettings(config.data, seat) : null),
    [seat, config.data, error],
  );
  const reading = useMemo(() => seatReading(settings, error), [settings, error]);
  return {
    seat,
    settings,
    reading,
    config: {
      doc: error ? null : (config.data ?? null),
      loading: known === null && config.loading,
      error,
    },
  };
}
