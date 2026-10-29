/**
 * A signal that aborts when the component that asked for it unmounts: what
 * the screen's own side of a request checks, so an answer that lands after
 * the person left neither changes what is gone nor acts for it (a toast, a
 * navigation, a parent's `onDone`).
 *
 * TWO USES, NEVER MIXED UP. A read or a check the screen may abandon takes
 * this signal, and stops when the screen goes. A WRITE THAT HAS STARTED DOES
 * NOT: it runs to its answer, since stopped part way it can leave half a
 * change (a sealed secret no document names, a removal nobody knows the end
 * of). The screen reads `.aborted` before it acts on that answer instead.
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
