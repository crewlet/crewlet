/**
 * The two affordances a JSON panel is: copy it, and select it.
 *
 * Both are asserted here rather than on a screen because both were WRONG in
 * the same way before — silently. A copy that reached no clipboard clicked
 * exactly like one that did, and select-all took the whole document while
 * looking like it had done something. A test that only rendered the button
 * would have passed through both.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { Code, CopyButton } from "./primitives.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/** Give the component the Clipboard API, and report what it was handed. */
function withClipboard(): { written: string[]; fail?: boolean } {
  const state: { written: string[]; fail?: boolean } = { written: [] };
  vi.stubGlobal("navigator", {
    ...globalThis.navigator,
    clipboard: {
      writeText: (text: string) => {
        if (state.fail) return Promise.reject(new Error("refused"));
        state.written.push(text);
        return Promise.resolve();
      },
    },
  });
  return state;
}

async function click(label: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: new RegExp(label, "i") }));
  });
}

describe("CopyButton", () => {
  test("puts the text on the clipboard and says it did", async () => {
    const clipboard = withClipboard();
    render(<CopyButton text='{"turn_id":"t-1"}' />);

    await click("copy");

    expect(clipboard.written).toEqual(['{"turn_id":"t-1"}']);
    // THE FEEDBACK IS THE FEATURE. The clipboard is invisible; without the
    // control saying so, a working copy and a dead button are the same
    // event.
    expect(screen.getByRole("button").textContent).toContain("Copied");
  });

  test("falls back to execCommand where the Clipboard API is absent", async () => {
    // Which is not a hypothetical: the API is gated on a secure context, so
    // it is simply undefined on the http://<lan-ip>:8000 anyone reads the
    // dashboard of a node that is not their laptop at.
    vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: undefined });
    const copied: string[] = [];
    const exec = vi.fn(() => {
      const field = document.querySelector("textarea");
      copied.push(field?.value ?? "");
      return true;
    });
    Object.defineProperty(document, "execCommand", { writable: true, value: exec });

    render(<CopyButton text="fallback text" />);
    await click("copy");

    expect(exec).toHaveBeenCalledWith("copy");
    expect(copied).toEqual(["fallback text"]);
    expect(screen.getByRole("button").textContent).toContain("Copied");
    // The scratch field is not left behind for the next reader to tab into.
    expect(document.querySelector("textarea")).toBeNull();
  });

  test("says so when the browser refuses, rather than looking like it worked", async () => {
    const clipboard = withClipboard();
    clipboard.fail = true;
    Object.defineProperty(document, "execCommand", { writable: true, value: () => false });

    render(<CopyButton text="anything" />);
    await click("copy");

    expect(screen.getByRole("button").textContent).toContain("Copy failed");
  });

  test("settles back to Copy so the label is never a stale claim", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    withClipboard();
    render(<CopyButton text="x" />);

    await click("copy");
    expect(screen.getByRole("button").textContent).toContain("Copied");

    await act(async () => {
      vi.advanceTimersByTime(2500);
    });
    const label = screen.getByRole("button").textContent ?? "";
    expect(label).toContain("Copy");
    expect(label).not.toContain("Copied");
  });
  test("resolves a thunk on click, so a live record is not serialized per frame", async () => {
    const clipboard = withClipboard();
    let calls = 0;
    const produce = () => {
      calls += 1;
      return "assembled once";
    };
    const { rerender } = render(<CopyButton text={produce} />);
    // Re-rendered as a streamed frame would: the thunk is not called.
    rerender(<CopyButton text={produce} />);
    rerender(<CopyButton text={produce} />);
    expect(calls).toBe(0);

    await click("copy");
    expect(calls).toBe(1);
    expect(clipboard.written).toEqual(["assembled once"]);
  });

  test("the status text is not part of the button's accessible name", async () => {
    withClipboard();
    render(<CopyButton text="x" />);
    await click("copy");

    // getByRole matches on the accessible NAME, so an exact-name query is
    // the assertion: with the live region inside the button, the control
    // was named "Copied copied to the clipboard".
    expect(screen.getByRole("button", { name: "Copied" })).toBeDefined();
    expect(screen.getByRole("status").textContent).toBe("copied to the clipboard");
  });
});

describe("Code", () => {
  test("a selectable block takes ⌘A / Ctrl+A for itself", () => {
    render(
      <div>
        <p>page furniture nobody asked to select</p>
        <Code selectable label="The turn record, as JSON">
          {'{"turn_id":"t-1"}'}
        </Code>
      </div>,
    );
    const block = screen.getByRole("region", { name: "The turn record, as JSON" });

    // A CYRILLIC LAYOUT: the physical A key reports `key: "ф"`. Browsers
    // resolve select-all from the key's POSITION, so a handler matching only
    // `e.key` declines here and the page-wide select-all it exists to
    // replace happens instead.
    const event = new KeyboardEvent("keydown", {
      key: "ф",
      code: "KeyA",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    block.dispatchEvent(event);

    // Handled: the browser's document-wide select-all never runs.
    expect(event.defaultPrevented).toBe(true);
    const selection = window.getSelection();
    expect(selection?.toString()).toBe('{"turn_id":"t-1"}');
    // Scoped: the paragraph beside it is not in the range.
    expect(selection?.toString()).not.toContain("page furniture");
  });

  test("plain ⌘A elsewhere in the block's own keys is left alone", () => {
    render(
      <Code selectable label="A block">
        {"body"}
      </Code>,
    );
    const block = screen.getByRole("region");

    for (const init of [
      { key: "a", code: "KeyA" }, // no modifier: typing, not selecting
      { key: "a", code: "KeyA", ctrlKey: true, altKey: true }, // a different chord
      // Ctrl+Shift+A is Chrome's tab search. Swallowing a chord the browser
      // owns takes it away and puts nothing in its place.
      { key: "A", code: "KeyA", ctrlKey: true, shiftKey: true },
      { key: "c", code: "KeyC", ctrlKey: true }, // copy, which the browser must keep
    ]) {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
      block.dispatchEvent(event);
      expect(event.defaultPrevented).toBe(false);
    }
  });

  test("a block that did not ask for it is not focusable and takes no keys", () => {
    // The ordinary case — a tool call's arguments, a prompt — must not
    // become a tab stop. There are dozens of them on one phase card, and a
    // keyboard reader would have to step through every one.
    const { container } = render(<Code>{"body"}</Code>);
    const block = container.querySelector("pre")!;
    expect(block.getAttribute("tabindex")).toBeNull();

    const event = new KeyboardEvent("keydown", {
      key: "a",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    block.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});
