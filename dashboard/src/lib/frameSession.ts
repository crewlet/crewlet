/**
 * Who this browser is signed in as, read ONCE by the frame — and whether that
 * session may read the company at all.
 *
 * `GET /auth/session` answers who a session is and what it holds whatever else
 * the engine refuses it, so it is the frame's FIRST question: the sidebar's
 * user block names the reader from it while the socket's `viewer` question is
 * out (or never answered), and the frame decides from it whether to dial the
 * live socket. It used to be asked three times over — by the frame before
 * dialling, by the user block, and by the refusal panel — so a signed-out load
 * of `/dashboard` sent two at once and a third from the sign-in it was routed
 * to.
 *
 * # The socket is dialled only for a session that may read the company
 *
 * The live socket needs `state:read`: the engine refuses its handshake to a
 * session without it (`internal/api/stream`), and every screen but the
 * reader's Account reads the socket. A session lacking it is NO ACCESS YET —
 * an ordinary state, a person invited with no grants, not a fault — so the
 * frame dials nothing for it (a dial was a refused handshake, its refusal probe
 * and a refused degraded-mode snapshot, twice) and draws one panel in place of
 * every screen but the Account.
 *
 * AND IT ASKS AGAIN, every [NO_ACCESS_RECHECK_MS] and whenever the tab comes
 * back, because nothing else can tell this tab that an administrator has given
 * the grant: the identity move that names the person is pushed over the socket
 * it does not have. The answer that holds the grant is what dials it.
 */

import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  type ReactNode,
} from "react";
import type { Grant } from "~/app/nav.ts";
import { adoptReader } from "./reader.ts";
import { useRest } from "./useRest.ts";
import { auth, type RestError, type SessionAnswer } from "~/protocol/index.ts";

/**
 * The grant the live socket needs — the engine's `iam.GrantStateRead`, which
 * `internal/api/stream` refuses a handshake without — and so the grant every
 * screen but the reader's Account needs.
 */
export const LIVE_VIEW_GRANT: Grant = "state:read";

/**
 * The refusal a session without [LIVE_VIEW_GRANT] is recorded as in the store
 * (`Store.setAccessRefused`), where a refused handshake records the engine's:
 * the frame dials nothing for it, so nothing else would.
 */
export const NO_ACCESS = `this session holds no ${LIVE_VIEW_GRANT}`;

/**
 * How often a session with no access asks again, in ms. The socket's own
 * reconnect ceiling: a tab waiting on something outside it — an engine coming
 * back, an administrator's grant — notices within half a minute, and an idle
 * tab of somebody who can do nothing else here costs the engine two cheap
 * guarded reads a minute rather than a standing poll on every tab.
 */
export const NO_ACCESS_RECHECK_MS = 30_000;

export interface FrameSession {
  /** What `GET /auth/session` answered, or null before an answer and after a refusal. */
  answer: SessionAnswer | null;
  /** Why the latest read did not answer: a `401` is nobody, anything else says nothing. */
  error: RestError | null;
  /** The session holds no [LIVE_VIEW_GRANT]: nothing is dialled, and it is asked again. */
  noAccess: boolean;
  /** Ask again now, quietly. */
  reload: () => Promise<void>;
}

const Reading = createContext<FrameSession | null>(null);

/** Ask who this browser is ONCE, for the frame — see the module's note. */
export function SessionReading({ children }: { children: ReactNode }) {
  const read = useRest(
    "/auth/session",
    async (signal) => {
      const session = await auth.session(signal);
      // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER learns who it is
      // read by here, since no sign-in in it ever said (`lib/reader.ts`).
      adoptReader(session.person);
      return session;
    },
    { refetchOnFocus: true },
  );
  const answer = read.data;
  const noAccess = answer !== null && !(answer.grants ?? []).includes(LIVE_VIEW_GRANT);
  const { reload: readAgain } = read;
  const reload = useCallback(() => readAgain({ quiet: true }), [readAgain]);
  useEffect(() => {
    if (!noAccess) return;
    const timer = setInterval(() => void reload(), NO_ACCESS_RECHECK_MS);
    return () => clearInterval(timer);
  }, [noAccess, reload]);
  const value = useMemo(
    () => ({ answer, error: read.error, noAccess, reload }),
    [answer, read.error, noAccess, reload],
  );
  return createElement(Reading.Provider, { value }, children);
}

/**
 * The session the frame read.
 *
 * THROWS OUTSIDE A [SessionReading], as `useViewer` does outside its provider:
 * a fallback read here is the per-caller read this module exists to remove.
 */
export function useFrameSession(): FrameSession {
  const session = useContext(Reading);
  if (session === null) {
    throw new Error(
      "useFrameSession() outside a SessionReading: the frame mounts one (app/Shell.tsx), and a suite that mounts a part of the frame without it mounts one around it",
    );
  }
  return session;
}
