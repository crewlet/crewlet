/**
 * What jsdom does not provide, and the component suites need — the one thing
 * it puts off that is better done before the first case — and the one rule
 * every case is held to: it says nothing to the console it did not expect.
 *
 * Kept to the genuine gaps. A polyfill that changes behaviour rather than
 * supplying a missing API would make the suite agree with a browser nobody
 * runs.
 */

import { afterAll, afterEach, beforeEach } from "vitest";
import { watchFile } from "./console.ts";

// A WARNING IS A FAILURE OF THE CASE THAT PRINTED IT — see `console.ts` for
// the defects that printed theirs into passing cases. Checked in an
// `afterEach` registered here, before any suite's own, and hooks unwind in
// reverse: so it runs after the suite's own teardown, and what an unmount
// says counts too.
//
// AND OF THE FILE, WHEN NO CASE WAS RUNNING: what it says while it loads, in a
// `beforeAll` or an `afterAll`, or between cases ([watchFile]). The watch
// starts as this file loads, before the suite's own modules do, and what it
// heard outside every case fails the `afterAll` registered here, which runs
// after the file's own.
const watch = watchFile();

beforeEach(() => {
  watch.caseStarts();
});

afterEach(() => {
  const said = watch.caseEnds();
  if (said.length > 0) {
    throw new Error(
      `this case wrote to the console, which is a defect until shown otherwise — silence one it expects with vi.spyOn(console, …).mockImplementation:\n${said.join("\n")}`,
    );
  }
});

afterAll(() => {
  const said = watch.fileEnds();
  if (said.length > 0) {
    throw new Error(
      `this file wrote to the console outside any case — while it loaded, in a beforeAll or an afterAll, or between cases — which is a defect until shown otherwise:\n${said.join("\n")}`,
    );
  }
});

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
// version: two suites that kept a value in storage passed every local run and
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
// reason every production read of storage is wrapped.
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

// WHAT jsdom PUTS OFF UNTIL FIRST USE, done here, while the environment is
// being set up. Not a gap and not a polyfill: the window's first computed
// style loads the CSS parser and parses jsdom's whole user-agent stylesheet,
// which measured 140 to 200 ms, and every test file runs in a process of its
// own, so every file paid it — inside whichever case first asked for an
// element by role, since a role query computes styles. That made the first
// case of every file its slowest for work that is the environment's, and on
// a loaded runner the one that ran out of its five seconds. It is still done
// once per file; it is no longer billed to a case. Only where there is a
// window: a suite of pure functions runs in node, with no document to style.
if (typeof getComputedStyle === "function" && typeof document !== "undefined") {
  getComputedStyle(document.documentElement);
}
