/**
 * Who this browser is signed in as, read ONCE by the frame.
 *
 * `GET /auth/session` answers who a session is and what it holds whatever else
 * the engine refuses it, so it is the frame's FIRST question: the sidebar's
 * user block names the reader from it while the socket's `viewer` question is
 * out (or never answered), and the frame decides from it whether to dial the
 * live socket. It used to be asked three times over — by the frame before
 * dialling, by the user block, and by the refusal panel — so a signed-out load
 * of `/dashboard` sent two at once and a third from the sign-in it was routed
 * to. A screen that needs who is reading — the Account, Home's greeting for an
 * unbound reader, People marking the reader's own row, reads THIS,
 * never a read of its own, and the Account re-reads it through [reload].
 *
 * # Every session is dialled
 *
 * The live socket admits every principal the engine resolves whose stage may
 * act (`internal/api/stream`): what a reader may SEE on it is decided per push
 * by their access (the overview for everybody, a seat's transcript for its
 * lead and an admin, Worklet's pushes for whoever holds Worklet), so a
 * person invited with no app still reads the org chart, the agents' status
 * and their own account. So the frame dials for every session it reads and
 * holds nothing back for one that holds no app; access given later reaches
 * the open tab by the engine closing its socket to reconnect (1013), after
 * which the viewer is asked again. A session the engine refuses the socket
 * outright — a person whose seat was taken out of the chart — is the
 * handshake's refusal, drawn by `app/frame/AccessRefused.tsx`.
 */

import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useMemo,
  type ReactNode,
} from "react";
import { takeSession } from "./session.ts";
import { useRest, type RestResult } from "./useRest.ts";
import { auth, type RestError, type SessionAnswer } from "~/protocol/index.ts";

export interface FrameSession {
  /** What `GET /auth/session` answered, or null before an answer and after a refusal. */
  answer: SessionAnswer | null;
  /** Why the latest read did not answer: a `401` is nobody, anything else says nothing. */
  error: RestError | null;
  /** The read's state as `QueryState` renders it — for the Account, which draws it. */
  code: RestResult<SessionAnswer>["code"];
  refusal: RestResult<SessionAnswer>["refusal"];
  loading: boolean;
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
      // read by here, since no sign-in in it ever said (`lib/reader.ts`) —
      // and a tab whose cookie somebody else's sign-in moved is handed to
      // them, with nothing of its last reader kept (`lib/session.ts`).
      takeSession(session.person);
      return session;
    },
    { refetchOnFocus: true },
  );
  const answer = read.data;
  const { reload: readAgain } = read;
  const reload = useCallback(() => readAgain({ quiet: true }), [readAgain]);
  const { error, code, refusal, loading } = read;
  const value = useMemo(
    () => ({ answer, error, code, refusal, loading, reload }),
    [answer, error, code, refusal, loading, reload],
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
