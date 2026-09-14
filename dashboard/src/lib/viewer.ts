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
 */

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
  kind: "agent" | "human" | "";
  /** A token is presented and no seat names its id. */
  unbound: boolean;
  /** No token is presented at all. */
  anonymous: boolean;
  loading: boolean;
}

/**
 * The viewer, polled rarely.
 *
 * A viewer changes on exactly two events — the reader presents a different
 * token, or an epoch lands that binds a seat to their id — so this is a slow
 * poll rather than a push, and the token dialog's own reconnect re-asks it.
 */
export function useViewer(): ViewerState {
  const { data, loading } = useQuery("viewer", undefined, { pollMs: 300_000 });
  const operatorID = data?.operator_id ?? "";
  const handle = data?.handle ?? "";
  return {
    operatorID,
    operator: data?.operator ?? false,
    handle,
    name: data?.name ?? "",
    kind: (data?.kind as ViewerState["kind"]) ?? "",
    unbound: operatorID !== "" && handle === "",
    anonymous: !loading && operatorID === "",
    loading,
  };
}

/** The wire answer, re-exported so a screen types its own reads. */
export type { Viewer };
