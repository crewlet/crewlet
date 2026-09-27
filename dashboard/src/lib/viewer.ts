/**
 * Who the dashboard thinks you are.
 *
 * THE FRAME HAS NEVER HAD A VIEWER, and everything personal in it was fiction
 * without one. `routes/MyWork.tsx` picked the alphabetically first seat and
 * called the result "My work"; `work_views` was asked without a viewer, so no
 * pinned or personal view could arrive at all and a shared board was
 * pixel-identical to one somebody had pinned; and the five `preset=` values
 * the tracker's grammar resolves against the caller's own identity were parsed,
 * tested, documented and never sent.
 *
 * The resolution is two steps and NEITHER IS NEW: Tier A's `api.auth.tokens`
 * maps a presented credential to an operator id, and a seat binds one of those
 * ids with `contact.crewlet_operator_id` (`internal/org/role.go`). What was
 * missing is a question that walks it — `viewer`.
 *
 * THREE STATES, and a screen has to tell them apart:
 *
 *   - BOUND — a token, and a seat that names its operator id. This person has
 *     an inbox, a queue, pins and favourites.
 *   - UNBOUND — a token with no seat naming it. An ORDINARY state, which
 *     `org.HumanContact` says in as many words, so a screen says what to bind
 *     rather than reporting a fault.
 *   - ANONYMOUS — no token at all. Every guarded surface is locked, and the
 *     lock says what it needs.
 *
 * And a FOURTH that is not one of them: NOBODY SAID YET. The three above are
 * answers; a query that has not come back is the absence of one, and it is
 * [ViewerState.loading] rather than a value. See [useViewer].
 */

import { createContext, createElement, useContext, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import type { Viewer } from "~/protocol/index.ts";

export interface ViewerState {
  /** The operator id the presented token resolves to, or "". */
  operatorID: string;
  /** Whether the guarded questions are answerable for this caller. */
  operator: boolean;
  /** The seat this token is bound to, or "" — see the note above. */
  handle: string;
  /** That seat's display name, or "". */
  name: string;
  /**
   * The changes the engine will make as this person — every tool
   * `POST /operator/act/{tool}` serves a token bound to a seat, and NONE for
   * an anonymous or unbound caller, who may not act (ADR-0024). Read by
   * `lib/useWriteAccess.ts`, and by nothing that decides for itself.
   */
  acts: readonly string[];
  kind: "agent" | "human" | "";
  /**
   * The project this person's create lands in when it names none — the
   * engine's own default for their seat, never a guess from the chart — or
   * "" when there is none, and a create must name one.
   */
  project: string;
  /** A token is presented and no seat names its id. */
  unbound: boolean;
  /** No token is presented at all. */
  anonymous: boolean;
  /** Nobody has said yet — no answer has arrived, or the last read failed. */
  loading: boolean;
  /**
   * The FIRST read is still out: nothing has answered and nothing has failed.
   * The narrow half of `loading`, for a surface that would rather wait one
   * round trip than act on no answer — a guarded section asks nothing it may
   * be refused until this clears — and must not wait for ever on a read that
   * failed, which `loading` alone cannot tell it apart from.
   */
  asking: boolean;
}

/**
 * The viewer, polled rarely.
 *
 * A viewer changes on exactly two events — the reader presents a different
 * token, or an epoch lands that binds a seat to their id — so this is a slow
 * poll rather than a push, and the token dialog's own reconnect re-asks it.
 */
function useViewerRead(): ViewerState {
  const { data, loading, error } = useQuery("viewer", undefined, { pollMs: 300_000 });
  const operatorID = data?.operator_id ?? "";
  const handle = data?.handle ?? "";

  // A FAILED READ IS NOT AN ANSWER, and reading it as one picks the worst of
  // the three. The engine reports anonymity as `operator_id: ""` with NO error
  // (`internal/api/queries/viewer.go`), and `viewer` is registered with plain
  // `Register`, so it is never refused as unauthorized either — an error from
  // this query therefore means only that nothing came back. It is reachable:
  // `sendQuery` gives up after ten seconds, and the hub drops the OLDEST
  // queued envelope under per-client backpressure, so a busy company can evict
  // this very answer. Folded into ANONYMOUS it locked an authenticated
  // operator out of the whole app rail, My work and the inbox — for the five
  // minutes until the next poll, with every other query on the page working.
  const unknown = loading || (error !== null && data === null);
  return {
    operatorID,
    operator: data?.operator ?? false,
    handle,
    name: data?.name ?? "",
    acts: data?.acts ?? [],
    project: data?.project ?? "",
    kind: (data?.kind as ViewerState["kind"]) ?? "",
    unbound: operatorID !== "" && handle === "",
    anonymous: !unknown && operatorID === "",
    loading: unknown,
    asking: loading && data === null && error === null,
  };
}

const Reading = createContext<ViewerState | null>(null);

/**
 * Ask who this browser is ONCE, for everything under it.
 *
 * `app/Shell.tsx` mounts it around the whole frame, so the sidebar, the page
 * header and every screen read one answer. Each of them asking for itself was
 * a standing `viewer` query per caller — three from the frame alone, more
 * from a screen — each holding one of the socket's four query slots while a
 * screen's first read waited, and each polling on its own clock, so two
 * surfaces could disagree about who the reader is for up to five minutes.
 */
export function ViewerProvider({ children }: { children: ReactNode }) {
  return createElement(Reading.Provider, { value: useViewerRead() }, children);
}

/**
 * Who the frame read this browser as.
 *
 * THROWS OUTSIDE A [ViewerProvider] rather than asking for itself, as the
 * kit's `useAppShell` throws outside a shell: a fallback read here is the
 * per-caller read this module exists to remove, back again wherever a caller
 * is mounted outside the frame — and it would work, so nothing would say so.
 */
export function useViewer(): ViewerState {
  const viewer = useContext(Reading);
  if (viewer === null) {
    throw new Error(
      "useViewer() outside a ViewerProvider: the frame mounts one (FrameReadings, app/Shell.tsx), and a suite that mounts a screen without the frame mounts one around it",
    );
  }
  return viewer;
}

/** The wire answer, re-exported so a screen types its own reads. */
export type { Viewer };
