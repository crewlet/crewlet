import { useLayoutEffect, useMemo, useRef } from "react";

/** An object whose every field is an event handler. */
type Handlers<T> = { readonly [K in keyof T]: (...args: never[]) => unknown };

/**
 * One object of handlers for a component's whole life, each calling the
 * newest function its caller gave under that name. A memo or an effect that
 * depends on it is not undone by a caller that builds its handlers inline on
 * every render (a drawer's `on={{ approve: ..., ask: ... }}`), and a handler
 * called after a render runs what that render gave. The names are the first
 * render's; a handler is for events, never called while rendering.
 */
export function useLatestHandlers<T extends Handlers<T>>(handlers: T): T {
  const latest = useRef(handlers);
  useLayoutEffect(() => {
    latest.current = handlers;
  });
  return useMemo(() => {
    const stable: Record<string, (...args: unknown[]) => unknown> = {};
    for (const name of Object.keys(latest.current) as Array<keyof T & string>) {
      stable[name] = (...args: unknown[]) =>
        (latest.current[name] as unknown as (...a: unknown[]) => unknown)(...args);
    }
    return stable as unknown as T;
  }, []);
}
