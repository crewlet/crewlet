/**
 * Who the dashboard thinks you are.
 *
 * THE FRAME HAS NEVER HAD A VIEWER, and everything personal in it was fiction
 * without one. `routes/MyWork.tsx` picked the alphabetically first seat and
 * called the result "My work"; `work_views` was asked without a viewer, so no
 * pinned or personal view could arrive at all and a shared board was
 * pixel-identical to one somebody had pinned; and the five `preset=` values
 * Worklet's grammar resolves against the caller's own identity were parsed,
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
 *   - UNBOUND — a principal with no seat: a Tier A token's session, a service
 *     account acting as itself, a pipeline — an ORDINARY state, and never a
 *     person's, since a person holds a human seat for as long as they exist.
 *     It has a record all the same — its pins, its inbox marks, its
 *     priorities — kept under its LOGIN, which is where its assistant writes
 *     it; what it lacks is the work the chart addresses to a seat. A screen
 *     shows its record and says what binding would add
 *     (`components/people.tsx`'s `UnboundRemedy`).
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

import {
  createContext,
  createElement,
  useContext,
  useEffect,
  useMemo,
  type ReactNode,
} from "react";
import { useQuery } from "./useQuery.ts";
import { useFrameSession } from "./frameSession.ts";
import { ADMIN, CHATLET, DOCLET, WORKLET, holds } from "./access.ts";
import type { SessionAnswer, Viewer } from "~/protocol/index.ts";
import type { AssistantModelNotice } from "~/contract/assistant.ts";
import {
  assistantModelNotice,
  assistantUnavailable,
  type AssistantAccessReason,
} from "~/protocol/assistantWire.ts";

export interface ViewerState {
  /** The login the engine resolved this browser to, or "". */
  login: string;
  /**
   * The access this caller carries, as the engine reports it — already the
   * effective set on the node that answered (an admin's every app included,
   * admin itself dropped where the node refuses it).
   */
  grants: string[];
  /**
   * Whether this caller holds `admin` — the company and the engine, and the
   * ADMIN PATH of every relation rule: it reads anybody's work record (their
   * queue, their inbox, their day) and every seat's transcript, where
   * otherwise only the owner and whoever leads them may. A lead reads their
   * reports' too, which this screen cannot know; the engine decides, and a
   * screen that asks on a lead's behalf renders the refusal it gets.
   */
  admin: boolean;
  /**
   * Whether this caller holds WORKLET: every work item, project, view, inbox
   * and queue question. A surface that reads Worklet asks nothing without it,
   * and says what would open it rather than drawing a refusal.
   */
  worklet: boolean;
  /** Whether this caller holds DOCLET: every page question. */
  doclet: boolean;
  /** Whether this caller holds CHATLET: every room question. */
  chatlet: boolean;
  /** The seat the directory binds this caller to, or "" — see the note above. */
  handle: string;
  /**
   * The name this caller's OWN record is kept under — their inbox, pins,
   * priorities and personal views. Their seat when bound, their login when
   * not; "" only for a caller with no record at all. Every personal read asks
   * by this, never by `handle`.
   */
  owner: string;
  /**
   * That SEAT's display name, or "" — the chart's name for a place, and never
   * what the reader is called ([readerName]).
   */
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
  /**
   * Whether this caller may change the company DOCUMENT — the engine's own
   * answer, false for every credential a managed document does not list
   * as a writer (ADR-0030). Read by `lib/useWriteAccess.ts`.
   */
  configWriter: boolean;
  /** Who manages the company document, or empty when nothing does. */
  configManagedBy: readonly string[];
  /**
   * Why this caller may not ask the Dashboard Assistant, or null when they
   * may: the engine's own first failing condition, and `disabled` for an
   * engine whose answer carries no `assistant` member at all.
   */
  assistant: AssistantAccessReason | null;
  /**
   * What the reader is told of the assistant's model before they ask (it is
   * signed out, its plan is spent, its CLI cannot start), or null when nothing
   * is known to be wrong with it: the drawer's notice, never a reason above.
   */
  assistantModel: AssistantModelNotice | null;
  /**
   * On a company talking on a vendor's chat, which vendor surfaces it
   * declares, named to a reader holding Chatlet only: empty everywhere else.
   * WHETHER the company runs Chatlet is every reader's, on the org
   * projection (`lib/services.ts`); this is the one further fact a place
   * naming the vendor needs (`chatVendorName`).
   */
  chatVendors: readonly string[];
}

/** The mounted reads of the viewer, asked again by [rereadViewer]. */
const rereads = new Set<() => void>();

/**
 * Ask the viewer again now, rather than at the next slow poll: for a surface
 * that has just learned something the answer carries, the assistant's model
 * failing or a question refused for it, so its notice is drawn (or cleared)
 * at once.
 */
export function rereadViewer(): void {
  for (const reread of rereads) reread();
}

/**
 * The viewer, polled rarely.
 *
 * A viewer changes on exactly two events — the reader signs in as somebody
 * else, or the directory binds them to a seat — so this is a slow poll rather
 * than a push, and a reconnect re-asks it.
 */
function useViewerRead(): ViewerState {
  const { data, loading, error, refetch } = useQuery("viewer", undefined, { pollMs: 300_000 });
  useEffect(() => {
    rereads.add(refetch);
    return () => {
      rereads.delete(refetch);
    };
  }, [refetch]);
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
    admin: holds(grants, ADMIN),
    worklet: holds(grants, WORKLET),
    doclet: holds(grants, DOCLET),
    chatlet: holds(grants, CHATLET),
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
    configWriter: data?.config_writer ?? false,
    configManagedBy: data?.config_managed_by ?? [],
    assistant: assistantUnavailable(data?.assistant),
    assistantModel: assistantModelNotice(data?.assistant),
    chatVendors: data?.chat_vendors ?? [],
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

/**
 * The reader's OWN name — their directory row's, as the frame's `GET
 * /auth/session` answered it — or "" where the row holds none.
 *
 * NEVER THE SEAT'S NAME. [ViewerState.name] is the chart's name for the seat
 * the reader holds, which is the name of a PLACE and is often a role: a person
 * with no name on their row was called "Founder" by the sidebar and greeted
 * "Good afternoon, Founder" on Home, while their own Account said they had no
 * name at all.
 */
export function ownName(session: Pick<SessionAnswer, "name"> | null): string {
  return session?.name?.trim() ?? "";
}

/**
 * What the reader is CALLED wherever the frame names them: their own name
 * ([ownName]), or else their login — the session's, or `login` while the
 * session has not answered. Never the seat's name, for [ownName]'s reason; the
 * seat is said beside it, as the seat.
 */
export function readerName(
  session: Pick<SessionAnswer, "name" | "login"> | null,
  login = "",
): string {
  return ownName(session) || session?.login || login;
}

/** No grant at all, as one value, so a reading that holds none is stable across renders. */
const NO_GRANTS: readonly string[] = Object.freeze([]);

/**
 * Who the frame has been told the reader is, and what they hold — one answer,
 * from one source ([useHeldReading]).
 */
export interface HeldReading {
  /** The login the reader is signed in as, or "" for nobody. */
  login: string;
  /** The grants they hold. */
  grants: readonly string[];
}

/**
 * WHO THE READER IS AND WHAT THEY HOLD, as every lock and refusal the frame
 * draws decides on it — the sidebar's rows, Settings' column and the section
 * a route opens — or null while nobody has said.
 *
 * THE VIEWER'S ANSWER once it has answered, and otherwise what the frame's
 * `GET /auth/session` answered ([useFrameSession]), which every session gets
 * whatever else the engine refuses it. The viewer is asked over the live
 * socket, which answers nothing before its handshake and never for a session
 * the engine refuses it to: read alone, as Settings' column once read it, the
 * frame drew no lock at all until the socket answered — while the sidebar,
 * reading the session, locked the same sections. One reading, two sources,
 * in that order.
 *
 * THE LOGIN COMES FROM THE SAME SOURCE AS THE GRANTS. A refusal decided on the
 * session's grants while the viewer was still out — the moment between the
 * session's answer and the socket's on every cold load of a closed section —
 * named the reader off the viewer, which had not answered, and told a person
 * signed in that nobody was.
 *
 * ONE READING, decided by one rule (`nav.ts`'s `sectionOpen`): every caller
 * that drew a lock read the grants its own way, and no two agreed.
 */
export function useHeldReading(): HeldReading | null {
  const viewer = useViewer();
  const { answer } = useFrameSession();
  return useMemo(() => {
    if (!viewer.loading) return { login: viewer.login, grants: viewer.grants };
    return answer ? { login: answer.login, grants: answer.grants ?? NO_GRANTS } : null;
  }, [answer, viewer.loading, viewer.login, viewer.grants]);
}

/** What the reader holds, off [useHeldReading] — the locks' half of it. */
export function useHeldGrants(): readonly string[] | null {
  return useHeldReading()?.grants ?? null;
}

/** The wire answer, re-exported so a screen types its own reads. */
export type { Viewer };
