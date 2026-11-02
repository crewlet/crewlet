/**
 * A CSS transition on the elements a selector matches, as a browser reports
 * it: a `CSSTransition` on the element itself, running, with its end ahead.
 * jsdom runs no transitions, and the kit's `usePresence` waits on an element's
 * own motion through `getAnimations()`, so a suite fakes that motion here and
 * ends it by hand.
 *
 * `finish()` ends the transition everywhere it runs; `restore()` puts
 * `getAnimations` back as it was, and belongs in a `finally`.
 */
export function fakeTransition(
  selector: string,
  property: string,
): { finish: () => void; restore: () => void } {
  let close = () => {};
  const finished = new Promise<void>((resolve) => (close = resolve));
  const motion = { playState: "running" };
  const had = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "getAnimations");
  Object.defineProperty(HTMLElement.prototype, "getAnimations", {
    configurable: true,
    value(this: HTMLElement) {
      if (!this.matches(selector)) return [];
      return [
        {
          get playState() {
            return motion.playState;
          },
          transitionProperty: property,
          currentTime: 0,
          effect: { target: this, getComputedTiming: () => ({ endTime: 300 }) },
          finished,
        } as unknown as Animation,
      ];
    },
  });
  return {
    finish() {
      motion.playState = "finished";
      close();
    },
    restore() {
      if (had) Object.defineProperty(HTMLElement.prototype, "getAnimations", had);
      else delete (HTMLElement.prototype as { getAnimations?: unknown }).getAnimations;
    },
  };
}
