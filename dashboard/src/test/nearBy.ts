import { act } from "@testing-library/react";

/** A stand-in IntersectionObserver whose sections come near only when a case says so. */
export class NearBy {
  static made: NearBy[] = [];
  targets: Element[] = [];
  constructor(private readonly said: IntersectionObserverCallback) {
    NearBy.made.push(this);
  }
  observe(el: Element) {
    this.targets.push(el);
  }
  unobserve() {}
  disconnect() {
    this.targets = [];
  }
  takeRecords() {
    return [];
  }
  /** Say the sections of `paths` came within one view. */
  static near(...paths: string[]) {
    for (const o of NearBy.made) {
      const entries = o.targets
        .filter((t) => paths.includes((t as HTMLElement).dataset.path ?? ""))
        .map(
          (target) => ({ target, isIntersecting: true }) as unknown as IntersectionObserverEntry,
        );
      if (entries.length > 0) act(() => o.said(entries, o as unknown as IntersectionObserver));
    }
  }
}
