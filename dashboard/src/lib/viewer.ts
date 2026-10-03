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
 * The answer is the PRINCIPAL the engine resolved for this browser — a person
 * signed in with a session, or a credential — and the seat the identity
 * directory binds them to. Nothing is derived here: the engine used to walk a
 * token id to a seat through a field on the seat's contact block, and that
 * binding is a directory row now (`crewlet iam bind`).
 *
 * THREE STATES, and a screen has to tell them apart:
 *
 *   - BOUND — a principal the directory binds to a seat. This person has an
 *     inbox, a queue, pins and favourites, kept under the seat.
 *   - UNBOUND — a principal with no seat. An ORDINARY state: an operator who is
 *     not in the chart, a pipeline, an automation. They have a record all the
 *     same — their pins, their inbox marks, their priorities — kept under their
 *     LOGIN, which is where their assistant writes it; what they lack is the
 *     work the chart addresses to a seat. A screen shows their record and says
 *     what binding would add, rather than reporting a fault.
 *   - ANONYMOUS — nobody resolved at all. Every guarded surface is locked, and
 *     the lock says what it needs.
 *
 * WHOSE RECORD IS `owner`, never `handle`. The engine answers the one name the
 * caller's own record is kept under, and a screen asking for "mine" by the
 * SEAT asked an unbound caller for nothing while their assistant wrote it under
 * their login.
 *
 * And a FOURTH that is not one of them: NOBODY SAID YET. The three above are
 * answers; a query that has not come back is the absence of one, and it is
 * [ViewerState.loading] rather than a value. See [useViewer].
 */

import { createContext, createElement, useContext, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import type { Viewer } from "~/protocol/index.ts";

export interface ViewerState {
  /** The login the engine resolved this browser to, or "". */
  login: string;
  /** The capabilities this caller carries, as the engine reports them. */
  grants: string[];
  /**
   * Whether this caller holds `fleet:operate` — the deployment's own grant,
   * and the ADMIN PATH of the engine's owner-or-lead rule: it reads anybody's
   * work record (their queue, their inbox, their day) where otherwise only
   * the owner and whoever leads them may. A lead reads their reports' too,
   * which this screen cannot know; the engine decides, and a screen that asks
   * on a lead's behalf renders the refusal it gets.
   *
   * NOT `people:manage`, which is what this read. That grant is authority over
   * PERSON ROWS in the identity directory — who is enrolled and what they
   * carry — and opens nobody's work record: a screen gating on it asked for a
   * colleague's queue on behalf of an administrator the engine refused, and
   * hid it from the operator the engine would have answered.
   */
  operatesFleet: boolean;
  /** The seat the directory binds this caller to, or "" — see the note above. */
  handle: string;
  /**
   * The name this caller's OWN record is kept under — their inbox, pins,
   * priorities and personal views. Their seat when bound, their login when
   * not; "" only for a caller with no record at all. Every personal read asks
   * by this, never by `handle`.
   */
  owner: string;
  /** That seat's display name, or "". */
  name: string;
  /**
   * The changes the engine will make for this caller — every tool
   * `POST /operator/act/{tool}` writes with that the authority table can
   * admit them to before any object is named — and NONE for an anonymous
   * one. An UNBOUND caller acts too, under their own login (ADR-0024): what
   * binding adds is the seat they act as, not the right to act. Read by
   * `lib/useWriteAccess.ts`, and by nothing that decides for itself; a
   * control the list does not name is drawn disabled with the reason, and
   * one it does name may still be refused on its object, which the engine
   * says in its own words.
   */
  acts: readonly string[];
  kind: "agent" | "human" | "";
  /**
   * The project this person's create lands in when it names none — the
   * engine's own default for their seat, never a guess from the chart — or
   * "" when there is none (an unbound caller, or a seat whose team and every
   * team above it own no project), and a create must name one.
   */
  project: string;
  /** Somebody is resolved and the directory binds them to no seat. */
  unbound: boolean;
  /** Nobody is resolved at all. */
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
 * A viewer changes on exactly two events — the reader signs in as somebody
 * else, or the directory binds them to a seat — so this is a slow poll rather
 * than a push, and a reconnect re-asks it.
 */
function useViewerRead(): ViewerState {
  const { data, loading, error } = useQuery("viewer", undefined, { pollMs: 300_000 });
  const login = data?.login ?? "";
  const handle = data?.handle ?? "";
  const grants = data?.grants ?? [];

  // A FAILED READ IS NOT AN ANSWER, and reading it as one picks the worst of
  // the three. An error from this query means only that nothing came back:
  // `sendQuery` gives up after ten seconds, and the hub drops the OLDEST queued
  // envelope under per-client backpressure, so a busy company can evict this
  // very answer. Folded into ANONYMOUS it locked a signed-in person out of the
  // whole app rail, My work and the inbox — for the five minutes until the
  // next poll, with every other query on the page working.
  const unknown = loading || (error !== null && data === null);
  return {
    login,
    grants,
    operatesFleet: grants.includes("fleet:operate"),
    handle,
    owner: data?.owner ?? "",
    name: data?.name ?? "",
    acts: data?.acts ?? [],
    project: data?.project ?? "",
    kind: (data?.kind as ViewerState["kind"]) ?? "",
    unbound: login !== "" && handle === "",
    anonymous: !unknown && login === "",
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
