/**
 * What jsdom does not provide, and the component suites need.
 *
 * Kept to the genuine gaps. A polyfill that changes behaviour rather than
 * supplying a missing API would make the suite agree with a browser nobody
 * runs.
 */

// jsdom implements neither. Both are read at module scope by layout-aware
// components, so their absence is a throw rather than a wrong answer.
if (!("matchMedia" in globalThis)) {
  Object.defineProperty(globalThis, "matchMedia", {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

if (!("ResizeObserver" in globalThis)) {
  Object.defineProperty(globalThis, "ResizeObserver", {
    writable: true,
    value: class {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    },
  });
}

if (!("scrollTo" in globalThis)) {
  Object.defineProperty(globalThis, "scrollTo", { writable: true, value: () => {} });
}

// localStorage is the third gap, and the one that only shows up on somebody
// else's machine. jsdom exposes it from the document's ORIGIN, so whether it
// is there depends on how the environment was constructed rather than on the
// version: two suites that store an operator token passed every local run and
// failed in CI with `localStorage is undefined`, which is a property of the
// runner, not of the code under test.
//
// An in-memory Storage rather than a mock: the production reads are wrapped
// in try/catch precisely because a real browser can refuse them, so a test
// double that cannot store would exercise the fallback on every run and never
// the path an operator actually takes.
if (!("localStorage" in globalThis)) {
  const store = new Map<string, string>();
  const memory: Storage = {
    get length() {
      return store.size;
    },
    key: (index: number) => [...store.keys()][index] ?? null,
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => void store.set(key, String(value)),
    removeItem: (key: string) => void store.delete(key),
    clear: () => store.clear(),
  };
  Object.defineProperty(globalThis, "localStorage", { writable: true, value: memory });
}
