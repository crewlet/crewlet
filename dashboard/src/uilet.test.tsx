/**
 * What the dashboard relies on the design system to do.
 *
 * Not a second copy of uilet's own suites. Every case here is a property an
 * engine screen is built on and would lose SILENTLY on a bump: a menu trigger
 * that stops forwarding its popup state announces nothing and still renders, a
 * link that stops withholding its opener still navigates, and an icon control
 * that loses its name is a button a screen reader calls "button". The package
 * is a dependency Dependabot moves on its own, so the seam it moves across is
 * the one place these have to be asserted.
 *
 * The first case is also a probe of the build: it fails the moment the inline
 * rule in `vitest.config.ts` is dropped, because an externalised
 * `@crewlethq/ui` throws `Unknown file extension ".css"` on its first
 * side-effect import and takes the rest of the suite with it.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createRef } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { AddGlyph, ArrowUpwardGlyph, MoreVertGlyph } from "@crewlethq/icons/glyphs";
import { Button, ButtonLink, CodeBlock, CopyButton, IconButton } from "@crewlethq/ui";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("a uilet component renders, so the packages are inlined rather than externalised", () => {
  render(<Button>Save</Button>);
  expect(screen.getByRole("button", { name: "Save" })).toBeDefined();
});

// A menu trigger and a list's Move buttons are built on Button, and each needs
// something a plain click target does not: a ref to hand focus back to, the
// popup state a screen reader announces, a way out of the tab order, and a
// data attribute the canvas reads to find the node a button belongs to.
test("a button hands a ref, aria and data attributes, tabIndex and keys to its element", () => {
  const ref = createRef<HTMLButtonElement>();
  const onKeyDown = vi.fn();
  render(
    <Button
      ref={ref}
      variant="tertiary"
      size="small"
      leadingIcon={<MoreVertGlyph />}
      title="Actions"
      aria-haspopup="menu"
      aria-expanded={false}
      aria-controls="actions-menu"
      tabIndex={-1}
      data-node="seat:ceo"
      onKeyDown={onKeyDown}
    />,
  );
  const button = screen.getByRole("button", { name: "Actions" });
  expect(ref.current).toBe(button);
  expect(button.getAttribute("aria-haspopup")).toBe("menu");
  expect(button.getAttribute("aria-expanded")).toBe("false");
  expect(button.getAttribute("aria-controls")).toBe("actions-menu");
  expect(button.getAttribute("tabindex")).toBe("-1");
  expect(button.getAttribute("data-node")).toBe("seat:ceo");
  fireEvent.keyDown(button, { key: "ArrowDown" });
  expect(onKeyDown).toHaveBeenCalledTimes(1);
});

test("an icon control is named by its label, and its tooltip says the same thing", () => {
  render(
    <>
      <IconButton label="Move up" icon={<ArrowUpwardGlyph />} />
      <IconButton label="Move goal 2 of 3 up" title="Move up" icon={<ArrowUpwardGlyph />} />
      <Button leadingIcon={<AddGlyph />}>Add</Button>
    </>,
  );
  const [plain, named, labelled] = screen.getAllByRole("button");
  expect(plain!.getAttribute("aria-label")).toBe("Move up");
  expect(plain!.getAttribute("title")).toBe("Move up");
  // The name may be longer than the tooltip: "Move goal 2 of 3 up" where the
  // pointer reader only needs "Move up".
  expect(named!.getAttribute("aria-label")).toBe("Move goal 2 of 3 up");
  expect(named!.getAttribute("title")).toBe("Move up");
  // A button with visible text is named by that text, not by a duplicate.
  expect(labelled!.getAttribute("aria-label")).toBeNull();
});

// A control that goes somewhere is a real link, drawn by the same recipe as
// the button beside it rather than a class list spelled at the call site.
test("a button link is an anchor wearing exactly the class list its button wears", () => {
  render(
    <>
      <Button variant="primary" size="small" leadingIcon={<AddGlyph />}>
        Create
      </Button>
      <ButtonLink
        variant="primary"
        size="small"
        leadingIcon={<AddGlyph />}
        href="#/org?lens=builder"
      >
        Create
      </ButtonLink>
    </>,
  );
  const button = screen.getByRole("button", { name: "Create" });
  const link = screen.getByRole("link", { name: "Create" });
  expect(link.className).toBe(button.className);
  expect(link.getAttribute("href")).toBe("#/org?lens=builder");
  // In the application's own hash routes, it stays in this tab.
  expect(link.getAttribute("target")).toBeNull();
});

test("an external link opens a tab without handing over the referrer or the opener", () => {
  render(
    <ButtonLink external href="https://example.com/install">
      Install
    </ButtonLink>,
  );
  const link = screen.getByRole("link", { name: /Install/ });
  expect(link.getAttribute("target")).toBe("_blank");
  // `noreferrer` implies `noopener` everywhere it is honoured, so one word
  // withholds both the referrer and the handle back to this window.
  expect(link.getAttribute("rel")).toBe("noreferrer");
});

/** Give the control the Clipboard API, and report what it was handed. */
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

async function press(name: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: new RegExp(name, "i") }));
  });
}

// The dashboard is read at http://<node>:8000 as often as at localhost, and
// the Clipboard API is gated on a secure context, so it is simply undefined
// there. This is the one screen affordance that fails ONLY on the machine
// nobody develops on.
test("a copy control falls back to execCommand where the clipboard API is absent", async () => {
  vi.stubGlobal("navigator", { ...globalThis.navigator, clipboard: undefined });
  const copied: string[] = [];
  const exec = vi.fn(() => {
    copied.push(document.querySelector("textarea")?.value ?? "");
    return true;
  });
  Object.defineProperty(document, "execCommand", { writable: true, value: exec });

  render(<CopyButton text="fallback text" />);
  await press("copy");

  expect(exec).toHaveBeenCalledWith("copy");
  expect(copied).toEqual(["fallback text"]);
  expect(screen.getByRole("button").textContent).toContain("Copied");
  // The scratch field is not left behind for the next reader to tab into.
  expect(document.querySelector("textarea")).toBeNull();
});

test("a copy control says so when the browser refuses, rather than looking like it worked", async () => {
  const clipboard = withClipboard();
  clipboard.fail = true;
  Object.defineProperty(document, "execCommand", { writable: true, value: () => false });

  render(<CopyButton text="anything" />);
  await press("copy");

  expect(screen.getByRole("button").textContent).toContain("Copy failed");
});

// A live turn is pushed twice per tool round, so a string prop would mean a
// full serialization of the whole record on every push for a control nobody
// has pressed.
test("a copy control resolves a thunk on the press, not on every render", async () => {
  const clipboard = withClipboard();
  let calls = 0;
  const produce = () => {
    calls += 1;
    return "assembled once";
  };
  const { rerender } = render(<CopyButton text={produce} />);
  rerender(<CopyButton text={produce} />);
  rerender(<CopyButton text={produce} />);
  expect(calls).toBe(0);

  await press("copy");
  expect(calls).toBe(1);
  expect(clipboard.written).toEqual(["assembled once"]);
});

test("the copy status is not part of the control's accessible name", async () => {
  withClipboard();
  render(<CopyButton text="x" />);
  await press("copy");

  // getByRole matches on the accessible NAME, so an exact-name query is the
  // assertion: with the live region inside the button, the control was named
  // "Copied copied to the clipboard".
  expect(screen.getByRole("button", { name: "Copied" })).toBeDefined();
  expect(screen.getByRole("status").textContent).toBe("copied to the clipboard");
});

// Select-all is a DOCUMENT verb, so on a screen that is mostly one record it
// took the nav, the stat row and every phase card with it. A record block
// claims the key while it has focus, and the engine's record screens are
// built on that.
test("a selectable record block takes select-all for itself, on any keyboard layout", () => {
  render(
    <div>
      <p>page furniture nobody asked to select</p>
      <CodeBlock plain selectable label="The turn record, as JSON" code={'{"turn_id":"t-1"}'} />
    </div>,
  );
  const block = screen.getByRole("region", { name: "The turn record, as JSON" });

  // A CYRILLIC LAYOUT: the physical A key reports `key: "ф"`. Browsers resolve
  // select-all from the key's POSITION, so a handler matching only `key`
  // declines here and the page-wide select-all happens instead.
  const event = new KeyboardEvent("keydown", {
    key: "\u0444",
    code: "KeyA",
    ctrlKey: true,
    bubbles: true,
    cancelable: true,
  });
  block.dispatchEvent(event);

  expect(event.defaultPrevented).toBe(true);
  const selection = window.getSelection();
  expect(selection?.toString()).toBe('{"turn_id":"t-1"}');
  expect(selection?.toString()).not.toContain("page furniture");
});

test("a record block that did not ask for select-all is not a tab stop and takes no keys", () => {
  // There are dozens of these on one phase card, and a keyboard reader would
  // have to step through every one.
  const { container } = render(<CodeBlock plain code="body" />);
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
