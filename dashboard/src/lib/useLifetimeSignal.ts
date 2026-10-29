/**
 * A signal that aborts when the component that asked for it unmounts: what a
 * request a person started from a dialog or a card is tied to, so an answer
 * that lands after they left neither changes what is gone nor acts for it (a
 * toast, a navigation, a parent's `onDone`).
 *
 * A GETTER, NOT A SIGNAL. The controller is made when the component mounts
 * and aborted when it unmounts, and React may unmount and mount a component
 * again without rendering it in between (Strict Mode does, on purpose), which
 * a signal read during render would miss: it would be the first mount's,
 * already aborted. Asked for at the moment a request starts, it is the
 * current mount's. Asked for before the component has mounted, or after it
 * has gone, it is an aborted one, so nothing starts.
 *
 * Why not a fresh `AbortController` per request: that signal is never
 * aborted, so it ties the request to nothing.
 */

import { useCallback, useEffect, useRef } from "react";

export function useLifetimeSignal(): () => AbortSignal {
  const current = useRef<AbortController | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    current.current = controller;
    return () => controller.abort();
  }, []);
  return useCallback(() => current.current?.signal ?? AbortSignal.abort(), []);
}
