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
