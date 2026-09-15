import { afterEach, describe, expect, it, vi } from "vitest";
import { render, cleanup, act } from "@testing-library/react";
import { PrefixWindow, isTyping, useKeyChords, type Chord, type Sequence } from "./keys.ts";

afterEach(cleanup);

/** Press a key at the window, the way a real keydown reaches the listener. */
function press(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const e = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
  act(() => {
    window.dispatchEvent(e);
  });
  return e;
}

function Bound({ chords }: { chords: (Chord | Sequence)[] }) {
  useKeyChords(chords);
  return <input aria-label="field" />;
}

/** Mount the bindings and hand back the field, for focusing. */
function bind(chords: (Chord | Sequence)[]) {
  const view = render(<Bound chords={chords} />);
  return view.getByLabelText("field") as HTMLInputElement;
}

describe("whether the keyboard belongs to a field", () => {
  it("says so for the elements a reader types into", () => {
    for (const tag of ["input", "textarea", "select"]) {
      const el = document.createElement(tag);
      document.body.append(el);
      el.focus();
      expect(isTyping(), tag).toBe(true);
      el.remove();
    }
  });

  it("says so for anything contenteditable", () => {
    const el = document.createElement("div");
    // TABINDEX AND NO ROLE, deliberately: jsdom does not make a bare
    // contenteditable focusable, and without focus `document.activeElement`
    // is the body and the predicate is not being asked about this element at
    // all. The role is left off so the tabindex+role branch cannot be what
    // passes — only `isContentEditable` can.
    el.tabIndex = 0;
    Object.defineProperty(el, "isContentEditable", { value: true });
    document.body.append(el);
    el.focus();
    expect(document.activeElement).toBe(el);
    expect(isTyping()).toBe(true);
    el.remove();
  });

  it("says so for a focusable element that handles its own keys", () => {
    // A tree row, a slider, a listbox. `tabindex` ALONE is not enough — a div
    // made focusable to receive a click is common and handles nothing — so it
    // is the pair, which is what an interactive role means.
    const el = document.createElement("div");
    el.tabIndex = 0;
    el.setAttribute("role", "slider");
    document.body.append(el);
    el.focus();
    expect(isTyping()).toBe(true);
    el.removeAttribute("role");
    expect(isTyping()).toBe(false);
    el.remove();
  });

  it("says no when nothing is focused", () => {
    (document.activeElement as HTMLElement | null)?.blur();
    expect(isTyping()).toBe(false);
  });
});

describe("a bare chord", () => {
  it("runs, and stops the browser's own handling", () => {
    const run = vi.fn();
    bind([{ key: "j", run }]);
    const e = press("j");
    expect(run).toHaveBeenCalledOnce();
    expect(e.defaultPrevented).toBe(true);
  });

  it("does not fire while the reader is typing", () => {
    // The failure this file exists to end: a `j` typed into a task's title
    // scrolling the grid behind it.
    const run = vi.fn();
    const field = bind([{ key: "j", run }]);
    field.focus();
    press("j");
    expect(run).not.toHaveBeenCalled();
  });

  it("fires in a field only when it says it may", () => {
    const escape = vi.fn();
    const field = bind([{ key: "escape", run: escape, whileTyping: true }]);
    field.focus();
    press("Escape");
    expect(escape).toHaveBeenCalledOnce();
  });

  it("matches the command modifier both ways round", () => {
    const plain = vi.fn();
    const withMeta = vi.fn();
    bind([
      { key: "k", run: plain },
      { key: "k", run: withMeta, meta: true },
    ]);
    press("k");
    expect(plain).toHaveBeenCalledOnce();
    expect(withMeta).not.toHaveBeenCalled();
    press("k", { metaKey: true });
    expect(withMeta).toHaveBeenCalledOnce();
    expect(plain).toHaveBeenCalledOnce();
    // Ctrl is the same chord on a machine with no command key.
    press("k", { ctrlKey: true });
    expect(withMeta).toHaveBeenCalledTimes(2);
  });

  it("skips a binding the screen turned off", () => {
    const run = vi.fn();
    bind([{ key: "j", run, when: false }]);
    const e = press("j");
    expect(run).not.toHaveBeenCalled();
    // AND LEAVES THE EVENT ALONE, so a disabled binding does not silently
    // swallow the key from whatever else would have had it.
    expect(e.defaultPrevented).toBe(false);
  });

  it("is case-insensitive about the key", () => {
    const run = vi.fn();
    bind([{ key: "j", run }]);
    press("J", { shiftKey: true });
    expect(run).toHaveBeenCalledOnce();
  });
});

describe("a prefixed sequence", () => {
  it("runs the second key after the first", () => {
    const run = vi.fn();
    bind([{ after: "g", key: "w", run }]);
    press("g");
    expect(run).not.toHaveBeenCalled();
    press("w");
    expect(run).toHaveBeenCalledOnce();
  });

  it("consumes the next key even when nothing matches it", () => {
    // Half a sequence falling through to the plain chords would make `g`
    // then `k` scroll a grid, which is the one thing the reader who typed
    // `g` did not ask for.
    const scroll = vi.fn();
    const go = vi.fn();
    bind([
      { key: "k", run: scroll },
      { after: "g", key: "w", run: go },
    ]);
    press("g");
    press("k");
    expect(scroll).not.toHaveBeenCalled();
    expect(go).not.toHaveBeenCalled();
    // AND THE PREFIX IS SPENT: the next `k` is an ordinary chord again.
    press("k");
    expect(scroll).toHaveBeenCalledOnce();
  });

  it("forgets the prefix after its window", () => {
    vi.useFakeTimers();
    const run = vi.fn();
    bind([{ after: "g", key: "w", run }]);
    press("g");
    act(() => {
      vi.advanceTimersByTime(PrefixWindow + 1);
    });
    press("w");
    expect(run).not.toHaveBeenCalled();
    vi.useRealTimers();
  });

  it("lets a plain chord on the prefix letter win", () => {
    // A screen that binds both `g` and `g i` has said the first is what a
    // bare `g` means.
    const plain = vi.fn();
    const sequence = vi.fn();
    bind([
      { key: "g", run: plain },
      { after: "g", key: "i", run: sequence },
    ]);
    press("g");
    press("i");
    expect(plain).toHaveBeenCalledOnce();
    expect(sequence).not.toHaveBeenCalled();
  });

  it("does not arm while the reader is typing", () => {
    const run = vi.fn();
    const field = bind([{ after: "g", key: "w", run }]);
    field.focus();
    press("g");
    field.blur();
    press("w");
    expect(run).not.toHaveBeenCalled();
  });
});

describe("the listener's lifetime", () => {
  it("stops when the component goes", () => {
    const run = vi.fn();
    const view = render(<Bound chords={[{ key: "j", run }]} />);
    press("j");
    expect(run).toHaveBeenCalledOnce();
    view.unmount();
    press("j");
    expect(run).toHaveBeenCalledOnce();
  });

  it("uses the latest handlers without rebinding", () => {
    // Every caller passes a fresh array each render, since the handlers close
    // over props. A dependency array over an array literal is never equal, so
    // it would tear the listener down and rebuild it on every render.
    const first = vi.fn();
    const second = vi.fn();
    const view = render(<Bound chords={[{ key: "j", run: first }]} />);
    view.rerender(<Bound chords={[{ key: "j", run: second }]} />);
    press("j");
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledOnce();
  });
});
