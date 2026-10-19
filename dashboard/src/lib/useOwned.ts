/**
 * A resource an effect owns: built when the effect runs, stopped when it is
 * cleaned up, and never handed out once it is stopped.
 *
 * Building a live resource in `useMemo` and stopping it in an effect's cleanup
 * looks equivalent and is not. React may run an effect's cleanup and then the
 * effect again while keeping every memo (StrictMode does it on every mount,
 * and a hidden `Activity` does it on every show), so the memo hands back the
 * object the cleanup just stopped, and the screen reads a store that never
 * hears another push. A memo's factory may also run for a render React then
 * throws away, leaking whatever the constructor started.
 *
 * Here the effect is the only place a resource is made, so every one made is
 * stopped exactly once, and a render only ever sees the one its own `deps`
 * built: until that one exists, and after its `deps` changed, it sees null.
 */

import { useEffect, useState } from "react";

export function useOwned<T>(
  make: () => T | null,
  stop: (value: T) => void,
  deps: readonly unknown[],
): T | null {
  const [held, setHeld] = useState<{ value: T; deps: readonly unknown[] } | null>(null);
  useEffect(() => {
    const value = make();
    setHeld(value === null ? null : { value, deps });
    return value === null ? undefined : () => stop(value);
    // The resource is rebuilt for its `deps` alone; `make` and `stop` are read as they are then.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return held !== null && sameDeps(held.deps, deps) ? held.value : null;
}

function sameDeps(a: readonly unknown[], b: readonly unknown[]): boolean {
  return a.length === b.length && a.every((v, i) => Object.is(v, b[i]));
}
