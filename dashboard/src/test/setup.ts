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

// Web Storage is the third gap, and the one that only shows up on somebody
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
// The test is the VALUE, not the key. In CI the property is present and
// holds undefined, so an `in` check — which is what the two guards above can
// safely use — skipped this polyfill entirely and the suites failed exactly
// as they had before it existed. Reading it can also throw, which is the same
// reason apiToken() wraps its own read.
//
// sessionStorage has exactly the same origin dependence, and it is filled by
// the same factory rather than a second copy, so the two areas cannot come to
// behave differently in the suite while they behave the same in a browser.
// Each area gets its OWN map: a key written to one must not be readable from
// the other.
type StorageArea = "localStorage" | "sessionStorage";

function storageMissing(area: StorageArea): boolean {
  try {
    return !globalThis[area];
  } catch {
    return true;
  }
}

function memoryStorage(): Storage {
  const store = new Map<string, string>();
  return {
    get length() {
      return store.size;
    },
    key: (index: number) => [...store.keys()][index] ?? null,
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => void store.set(key, String(value)),
    removeItem: (key: string) => void store.delete(key),
    clear: () => store.clear(),
  };
}

for (const area of ["localStorage", "sessionStorage"] as const) {
  if (storageMissing(area)) {
    Object.defineProperty(globalThis, area, { writable: true, value: memoryStorage() });
  }
}
