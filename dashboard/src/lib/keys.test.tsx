import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, cleanup, act, fireEvent } from "@testing-library/react";
import { LayerHost, Modal, usePopupLayer } from "@crewlethq/ui";
import {
  PrefixWindow,
  isTyping,
  useKeyChords,
  useModalLayerOpen,
  type Chord,
  type Sequence,
} from "./keys.ts";

afterEach(cleanup);

/** Press a key at the window, the way a real keydown reaches the listener. */
function press(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const e = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
  act(() => {
    window.dispatchEvent(e);
  });
  return e;
}

/**
 * Press a key the way the DOM delivers one: at an element, bubbling up.
 *
 * `press` above dispatches AT the window, which reaches our own listener and
 * nothing else — an event dispatched at the window never travels down to a
 * listener on the document. That is a faithful enough simulation while the
 * only listener in play is ours, and it stops being one the moment a second
 * party is listening: uilet's layer stack binds `keydown` on the document, so
 * a press aimed at the window is invisible to every open surface.
 */
function pressFrom(target: Element, key: string): void {
  act(() => {
    target.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
  });
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

  // A DIALOG'S ESCAPE IS THE DIALOG'S, and this is the one case the event
  // itself cannot report. uilet's layer stack calls `preventDefault()` on a
  // press it handled and deliberately not `stopPropagation()`, so the press
  // still reaches the window listener every chord in this product is bound to
  // — and the page ran its own escape behind the open surface. In the product
  // that was a peek rail closing under a dialog, from a press aimed at the
  // dialog, with the dialog closing too so the gesture looked like it worked.
  //
  // Against the REAL stack rather than a mocked predicate: what is being
  // claimed is that our guard and uilet's ordering agree, and a stub of
  // `isModalLayerOpen` would assert only that our own `if` does what it says.
  it("leaves the keyboard to a surface that is on the layer stack", () => {
    const page = vi.fn();
    const dismissed = vi.fn();
    render(
      <LayerHost>
        <Bound chords={[{ key: "escape", run: page, whileTyping: true }]} />
        <Modal open title="A dialog" onClose={dismissed}>
          <p>Body</p>
        </Modal>
      </LayerHost>,
    );
    pressFrom(document.body, "Escape");
    expect(dismissed).toHaveBeenCalledOnce();
    expect(page).not.toHaveBeenCalled();
  });

  // AND HANDS IT BACK. A guard that never lifted would be the same bug with
  // the surfaces swapped: every chord in the product dead for the rest of the
  // session because one dialog was opened once.
  it("takes the keyboard back once nothing is on the stack", () => {
    const page = vi.fn();
    const { rerender } = render(
      <LayerHost>
        <Bound chords={[{ key: "escape", run: page, whileTyping: true }]} />
        <Modal open title="A dialog" onClose={() => {}}>
          <p>Body</p>
        </Modal>
      </LayerHost>,
    );
    pressFrom(document.body, "Escape");
    expect(page).not.toHaveBeenCalled();
    rerender(
      <LayerHost>
        <Bound chords={[{ key: "escape", run: page, whileTyping: true }]} />
      </LayerHost>,
    );
    pressFrom(document.body, "Escape");
    expect(page).toHaveBeenCalledOnce();
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

describe("a control keeps the key that activates it", () => {
  // THE BUTTON IS PRESSED, NOT THE ROW. A grid binds a bare Enter to open its
  // cursor row, and the chord's `preventDefault` used to cancel the platform's
  // own activation of the button the reader had focused — Restore in the
  // trash, the ceiling editor on Budgets — so Enter opened the peek instead.
  it("leaves Enter and Space on a focused button or link to the control", () => {
    const run = vi.fn();
    bind([
      { key: "enter", run },
      { key: " ", run },
    ]);
    for (const make of [
      () => document.createElement("button"),
      () => Object.assign(document.createElement("a"), { href: "#/x" }),
    ]) {
      const el = make();
      document.body.append(el);
      el.focus();
      expect(press("Enter").defaultPrevented).toBe(false);
      expect(press(" ").defaultPrevented).toBe(false);
      el.remove();
    }
    expect(run).not.toHaveBeenCalled();
  });

  it("still runs the chord where nothing on the page claims the key", () => {
    const run = vi.fn();
    bind([{ key: "enter", run }]);
    (document.activeElement as HTMLElement | null)?.blur();
    press("Enter");
    expect(run).toHaveBeenCalledTimes(1);
    // An anchor with no href is not a link, and activates nothing.
    const bare = document.createElement("a");
    bare.tabIndex = 0;
    document.body.append(bare);
    bare.focus();
    press("Enter");
    expect(run).toHaveBeenCalledTimes(2);
    bare.remove();
  });
});

describe("whether a modal surface stands over the page", () => {
  /** What the hook answers, in a component nothing else re-renders. */
  function Watch() {
    return <output aria-label="covered">{String(useModalLayerOpen())}</output>;
  }

  /** A dialog and a menu, each opened and closed by its own state, so the
   *  watcher beside them is re-rendered by the stack and nothing else. */
  function Surfaces() {
    const [dialog, setDialog] = useState(false);
    const [menu, setMenu] = useState(false);
    const popup = usePopupLayer({ open: menu, onDismiss: () => setMenu(false) });
    return (
      <>
        <button onClick={() => setDialog(true)}>open the dialog</button>
        <button onClick={() => setMenu((open) => !open)}>toggle the menu</button>
        {menu && <div ref={popup.panelRef}>a menu</div>}
        <Modal open={dialog} title="A dialog" onClose={() => setDialog(false)}>
          <button onClick={() => setDialog(false)}>close it</button>
        </Modal>
      </>
    );
  }

  // AS A VALUE THAT MOVES, which `isModalLayerOpen` read at a press is not. A
  // chat room counts what it drew as seen only while nothing covers it, and
  // counts it the moment the cover goes, so the answer has to re-render the
  // page when a dialog opens and when it closes. The kit publishes no feed of
  // its stack, and this is the case that fails if the one the hook reads, a
  // closed popup's place in the band, stopped moving with it.
  it("follows a modal opening and closing, and a menu is not one", () => {
    const view = render(
      <LayerHost>
        <Watch />
        <Surfaces />
      </LayerHost>,
    );
    const covered = () => view.getByLabelText("covered").textContent;
    expect(covered()).toBe("false");

    // A MENU IS ON THE STACK AND COVERS NOTHING: it is not modal, and the
    // page's keys still reach past it.
    fireEvent.click(view.getByText("toggle the menu"));
    expect(view.getByText("a menu")).toBeDefined();
    expect(covered()).toBe("false");
    fireEvent.click(view.getByText("toggle the menu"));

    fireEvent.click(view.getByText("open the dialog"));
    expect(covered()).toBe("true");
    fireEvent.click(view.getByText("close it"));
    expect(covered()).toBe("false");
  });
});

// ALT AND SHIFT ARE COMPARED WHERE A CHORD NAMES THEM, and only there. A
// screen that moves between rooms on Alt with an arrow, and between unread
// rooms on Alt and Shift with the same arrow, holds two chords for one key;
// compared on the key alone, both fired for either press, and the bare arrow
// that scrolls a list fired them too.
describe("a chord that names Alt or Shift", () => {
  it("fires only with what it names held, and never for the bare key", () => {
    const step = vi.fn();
    const unread = vi.fn();
    bind([
      { key: "arrowdown", alt: true, shift: false, run: step },
      { key: "arrowdown", alt: true, shift: true, run: unread },
    ]);
    press("ArrowDown");
    expect(step).not.toHaveBeenCalled();
    expect(unread).not.toHaveBeenCalled();
    press("ArrowDown", { altKey: true });
    expect(step).toHaveBeenCalledTimes(1);
    expect(unread).not.toHaveBeenCalled();
    press("ArrowDown", { altKey: true, shiftKey: true });
    expect(step).toHaveBeenCalledTimes(1);
    expect(unread).toHaveBeenCalledTimes(1);
  });

  it("a chord that names neither fires whatever is held, as before", () => {
    const next = vi.fn();
    bind([{ key: "j", run: next }]);
    press("j", { shiftKey: true });
    expect(next).toHaveBeenCalledTimes(1);
  });
});
