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
 * maps a presented key to its id and its ROLE, and a seat links one of those
 * ids with `contact.crewlet_operator_id` (`internal/org/role.go`). What was
 * missing is a question that walks it — `viewer`.
 *
 * TWO INDEPENDENT FACTS, and a screen must not fold one into the other. The
 * ROLE says what the key reaches — a member reads what the company published,
 * an admin also what the machine processed and how it is run (ADR-0031) — and
 * the LINK says who it acts as. A linked member is a teammate; an unlinked
 * admin is a pipeline's credential; both are ordinary.
 *
 * THREE STATES of the link, and a screen has to tell them apart:
 *
 *   - LINKED — a key, and a seat that names its id. This person has an inbox,
 *     a queue, pins and favourites.
 *   - UNBOUND — a key with no seat naming it. An ORDINARY state, which
 *     `org.HumanContact` says in as many words, so a screen says what to link
 *     rather than reporting a fault.
 *   - ANONYMOUS — no key at all. Everything past the anonymous posture's reach
 *     is locked, and the lock says what it needs.
 *
 * And a FOURTH that is not one of them: NOBODY SAID YET. The three above are
 * answers; a query that has not come back is the absence of one, and it is
 * [ViewerState.loading] rather than a value. See [useViewer].
 */

import { createContext, createElement, useContext, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import type { Viewer, ViewerAdmin } from "~/protocol/index.ts";
import type { Reach, TokenRole } from "~/contract/access.ts";

export interface ViewerState {
  /** The id of the key this browser presents, or "" for none. */
  tokenID: string;
  /** What that key is for, or "" when none is presented (ADR-0031). */
  role: TokenRole | "";
  /**
   * How far this caller reaches — the engine's own answer, compared against
   * by every surface that names a reach. `open` until somebody has said.
   */
  reach: Reach;
  /**
   * Whether this caller reaches the ADMIN surfaces: what the machine
   * processed and how it is run — every guarded section, `/config`,
   * `/secrets`. The engine's reach and never a guess from the role, so a
   * disabled guard's admin answer and a key's are read alike.
   */
  admin: boolean;
  /** Whether a human seat links this key — see the note above. */
  linked: boolean;
  /** The seat this key is linked to, or "" — see the note above. */
  handle: string;
  /** That seat's display name, or "". */
  name: string;
  /**
   * The handles of the people in this person's line, whose personal records
   * the engine answers them for beside their own. Empty for an unlinked or
   * anonymous caller.
   */
  line: readonly string[];
  /**
   * The changes the engine will make as this person — every tool
   * `POST /operator/act/{tool}` serves a key linked to a seat, and NONE for
   * an anonymous or unlinked caller, who may not act (ADR-0024). Read by
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
  /** A key is presented and no seat names its id. */
  unbound: boolean;
  /** No key is presented at all. */
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
  /**
   * Whether this caller may change the company DOCUMENT — the engine's own
   * answer: an admin key, and on a managed document one it lists as a writer
   * (ADR-0030). Read by `lib/useWriteAccess.ts`.
   */
  configWriter: boolean;
  /** Who manages the company document, or empty when nothing does. */
  configManagedBy: readonly string[];
  /**
   * The people holding an admin key, in handle order — who a member asks for
   * what their key does not reach. Empty for an anonymous caller, who is
   * never told an admin's name, and where no person holds one.
   */
  admins: readonly ViewerAdmin[];
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
  const tokenID = data?.token_id ?? "";
  const reach = data?.reach ?? "open";

  // A FAILED READ IS NOT AN ANSWER, and reading it as one picks the worst of
  // the three. The engine reports anonymity as `token_id: ""` with NO error
  // (`internal/api/queries/viewer.go`), and `viewer` is the one question at
  // reach `open`, so it is never refused as unauthorized either — an error
  // from this query therefore means only that nothing came back. It is
  // reachable: `sendQuery` gives up after ten seconds, and the hub drops the
  // OLDEST queued envelope under per-client backpressure, so a busy company
  // can evict this very answer. Folded into ANONYMOUS it locked an
  // authenticated person out of the whole app rail, My work and the inbox —
  // for the five minutes until the next poll, with every other query on the
  // page working.
  const unknown = loading || (error !== null && data === null);
  const linked = data?.linked ?? false;
  return {
    tokenID,
    role: data?.role ?? "",
    reach,
    admin: reach === "admin",
    linked,
    handle: data?.handle ?? "",
    name: data?.name ?? "",
    line: data?.line ?? [],
    acts: data?.acts ?? [],
    project: data?.project ?? "",
    kind: (data?.kind as ViewerState["kind"]) ?? "",
    // THE ENGINE'S `linked`, never a handle's absence: the two say the same
    // today, and the field exists so a screen does not have to infer it.
    unbound: tokenID !== "" && !linked,
    anonymous: !unknown && tokenID === "",
    loading: unknown,
    asking: loading && data === null && error === null,
    configWriter: data?.config_writer ?? false,
    configManagedBy: data?.config_managed_by ?? [],
    admins: data?.admins ?? [],
  };
}

/**
 * Who a reader asks for what their key does not reach: the FIRST admin the
 * engine named (`viewer.admins`, in handle order) by their name — their handle
 * where the seat has none — or "an admin" where nobody is named: no person
 * holds an admin key, or the reader is anonymous and is told nobody.
 *
 * ONE PERSON, NOT A LIST. A sentence naming every admin is a roster nobody
 * asked for, and the first is as good as any for "who do I ask".
 */
export function adminToAsk(admins: readonly ViewerAdmin[]): string {
  const first = admins[0];
  return first ? first.name || first.handle : "an admin";
}

/**
 * Whether the engine answers this reader for `handle`'s PERSONAL records —
 * their inbox, their priorities, their day.
 *
 * THE ENGINE'S RULE, stated once on this side (`viewerParty`): a person reads
 * their own seat's records and those of the seats in their LINE, and nobody
 * else's. An ADMIN KEY GIVES NO MORE HERE — what the machine processed is an
 * admin's, and a colleague's inbox is not that — so this asks the link and
 * the line, never the role. A screen asks only where it may be answered, and
 * withholds by a sentence elsewhere rather than drawing a refusal.
 */
export function readsPersonOf(
  viewer: Pick<ViewerState, "handle" | "line">,
  handle: string,
): boolean {
  if (handle === "" || viewer.handle === "") return false;
  return viewer.handle === handle || viewer.line.includes(handle);
}

/**
 * The refusal an ADMIN's read would get from this reader, when it is known
 * BEFORE the question — or null, and the question is asked.
 *
 * NOT ASKED WHERE THE ANSWER IS KNOWN. A browser presenting no key is refused
 * every admin answer (`unauthorized`), and so is a member's key once the
 * viewer has said that is what it is (`forbidden`) — asking only puts a
 * refusal on the wire on every page that reads one. Without a key the STORED
 * TOKEN decides rather than the viewer, which arrives a round trip later and
 * would put the one refused request back — unless the viewer has answered
 * that this reader is an admin anyway, which a disabled guard makes of
 * everybody. A key is asked with until the viewer says it is not an admin's,
 * because a member's refusal costs one request and an admin waiting a round
 * trip costs every page.
 *
 * THE REFUSAL ITSELF IS RETURNED, not a bool, so a screen that does not ask
 * still draws exactly what the engine would have answered.
 */
export function knownRefusal(
  viewer: Pick<ViewerState, "loading" | "admin" | "anonymous">,
  tokenStored: boolean,
): "unauthorized" | "forbidden" | null {
  if (!viewer.loading) {
    if (viewer.admin) return null;
    return viewer.anonymous ? "unauthorized" : "forbidden";
  }
  return tokenStored ? null : "unauthorized";
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
export type { Viewer, ViewerAdmin };
