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

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { createRef } from "react";
import { afterEach, expect, test, vi } from "vitest";
import {
  AddGlyph,
  ArrowUpwardGlyph,
  ComputerGlyph,
  DarkModeGlyph,
  LightModeGlyph,
  MoreVertGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Button,
  ButtonLink,
  CodeBlock,
  CopyButton,
  IconButton,
  SegmentedControl,
  TabPanel,
  Tabs,
  Tag,
  tabId,
} from "@crewlethq/ui";
import { Checkbox, Kbd, keyGlyph } from "@crewlethq/ui";
import { drawnClasses } from "./testing.tsx";

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

// EB02. The badge this replaces drew a pressed filter as `background:
// currentColor` with its label still in that same ink, which is 1:1: the
// Model screen's "4 failed" filter became an unreadable block the moment
// somebody used it. A press is a state, and a state does not get to repaint
// the ground a label was measured against.
test("a filter tag that is on keeps the ground its label was measured on", () => {
  const off = drawnClasses(Tag, {
    variant: "danger",
    onClick: () => {},
    children: "4 failed",
  });
  const on = drawnClasses(Tag, {
    variant: "danger",
    onClick: () => {},
    pressed: true,
    children: "4 failed",
  });
  expect(on).toEqual(off);
});

test("a filter tag says whether it is on, and still reads as its own label", () => {
  const { rerender } = render(
    <Tag variant="danger" pressed onClick={() => {}}>
      4 failed
    </Tag>,
  );
  const filter = screen.getByRole("button", { name: "4 failed" });
  expect(filter.getAttribute("aria-pressed")).toBe("true");
  rerender(
    <Tag variant="danger" onClick={() => {}}>
      4 failed
    </Tag>,
  );
  expect(screen.getByRole("button", { name: "4 failed" }).getAttribute("aria-pressed")).toBe(
    "false",
  );
});

// The two rows a screen draws look alike and are NOT alike: one is a set of
// sections and one is a setting. The engine picks which, and the difference is
// a history entry per keypress, so it is checked here rather than assumed.
const LENSES = [
  { value: "chart", label: "Chart" },
  { value: "directory", label: "Directory" },
  { value: "charter", label: "Charter" },
];

function tabRow(row: "tabs" | "segmented") {
  const onValueChange = vi.fn();
  render(
    <>
      {row === "tabs" ? (
        <Tabs
          ariaLabel="View"
          value="chart"
          items={LENSES}
          onValueChange={onValueChange}
          panelId="panel"
        />
      ) : (
        <SegmentedControl
          label="View"
          value="chart"
          options={LENSES}
          onValueChange={onValueChange}
          semantics="tabs"
          panelId="panel"
        />
      )}
      <TabPanel id="panel" value="chart">
        content
      </TabPanel>
    </>,
  );
  return onValueChange;
}

test.each(["tabs", "segmented"] as const)("%s: the arrows move focus and select nothing", (row) => {
  const onValueChange = tabRow(row);
  const tabs = screen.getAllByRole("tab");
  tabs[0]!.focus();
  fireEvent.keyDown(tabs[0]!, { key: "ArrowRight" });
  expect(document.activeElement).toBe(tabs[1]);
  fireEvent.keyDown(tabs[1]!, { key: "End" });
  expect(document.activeElement).toBe(tabs[2]);
  fireEvent.keyDown(tabs[2]!, { key: "ArrowRight" });
  expect(document.activeElement).toBe(tabs[0]);
  fireEvent.keyDown(tabs[0]!, { key: "ArrowLeft" });
  expect(document.activeElement).toBe(tabs[2]);
  // A horizontal row of tabs does not move on Up and Down.
  fireEvent.keyDown(tabs[2]!, { key: "ArrowDown" });
  expect(document.activeElement).toBe(tabs[2]);
  // NOTHING SELECTED: each of those would have pushed a history entry.
  expect(onValueChange).not.toHaveBeenCalled();
  fireEvent.click(tabs[2]!);
  expect(onValueChange).toHaveBeenCalledWith("charter");
});

test.each(["tabs", "segmented"] as const)("%s: one tab stop, and the panel it names", (row) => {
  tabRow(row);
  const tabs = screen.getAllByRole("tab");
  expect(tabs.map((tab) => tab.tabIndex)).toEqual([0, -1, -1]);
  expect(tabs[0]!.getAttribute("aria-selected")).toBe("true");
  for (const tab of tabs) expect(tab.getAttribute("aria-controls")).toBe("panel");
  const panel = screen.getByRole("tabpanel");
  expect(panel.getAttribute("aria-labelledby")).toBe(tabId("panel", "chart"));
  expect(document.getElementById(tabId("panel", "chart"))).toBe(tabs[0]);
  expect(screen.getByRole("tabpanel", { name: "Chart" })).toBe(panel);
});

test("a setting is a radio group whose arrows select as they move", () => {
  const onValueChange = vi.fn();
  render(
    <SegmentedControl
      label="Theme"
      semantics="radio"
      value="system"
      options={[
        { value: "light", label: "", icon: <LightModeGlyph />, title: "Light" },
        { value: "system", label: "", icon: <ComputerGlyph />, title: "Follow the system" },
        { value: "dark", label: "", icon: <DarkModeGlyph />, title: "Dark" },
      ]}
      onValueChange={onValueChange}
    />,
  );
  const group = screen.getByRole("radiogroup", { name: "Theme" });
  const radios = within(group).getAllByRole("radio");
  // Named, although their only visible content is a glyph.
  expect(screen.getByRole("radio", { name: "Dark" })).toBe(radios[2]);
  expect(radios[1]!.getAttribute("aria-checked")).toBe("true");
  expect(radios.map((radio) => radio.tabIndex)).toEqual([-1, 0, -1]);
  // A setting has no panel, so it is not a tab row and must not say it is.
  expect(screen.queryByRole("tab")).toBeNull();

  radios[1]!.focus();
  fireEvent.keyDown(radios[1]!, { key: "ArrowDown" });
  expect(document.activeElement).toBe(radios[2]);
  expect(onValueChange).toHaveBeenLastCalledWith("dark");
  fireEvent.keyDown(radios[2]!, { key: "ArrowUp" });
  expect(onValueChange).toHaveBeenLastCalledWith("system");
});

// A checkbox is named by its label, described by its consequence, and toggled
// from anywhere on its row. All three were hand-rolled twice in this dashboard
// before, and the row that says what ticking a box DELETES is the one that has
// to be read aloud after the name rather than as part of it.
test("a checkbox is named by its label alone, and its consequence describes it", () => {
  render(
    <Checkbox
      framed
      tone="danger"
      checked={false}
      onCheckedChange={() => {}}
      label="Also remove the accounts Crewlet created"
      description="Each agent's account at the vendor is deleted."
    />,
  );
  const box = screen.getByRole("checkbox", { name: "Also remove the accounts Crewlet created" });
  const described = document.getElementById(box.getAttribute("aria-describedby") ?? "");
  expect(described?.textContent).toBe("Each agent's account at the vendor is deleted.");
});

test("a press anywhere on a checkbox row toggles it, and reports the new state", () => {
  const onCheckedChange = vi.fn();
  render(<Checkbox label="Clear lead" checked={false} onCheckedChange={onCheckedChange} />);
  fireEvent.click(screen.getByText("Clear lead"));
  expect(onCheckedChange).toHaveBeenCalledWith(true);
});

test("a disabled checkbox cannot be ticked from its row either", () => {
  render(
    <Checkbox
      disabled
      checked={false}
      onCheckedChange={() => {}}
      label="Also remove the accounts Crewlet created"
      description="Each agent's account at the vendor is deleted."
    />,
  );
  const box = screen.getByRole("checkbox") as HTMLInputElement;
  fireEvent.click(screen.getByText("Also remove the accounts Crewlet created"));
  expect(box.checked).toBe(false);
  expect(box.disabled).toBe(true);
});

// A hint that says Ctrl+Z to somebody on a Mac names a key they do not press,
// and one that draws the glyphs alone is read aloud as "place of interest sign
// Z". The builder's toolbar and menus are built on both halves.
test("Mod is Command on Apple platforms and Control everywhere else", () => {
  expect(keyGlyph("Mod", true)).toEqual({ glyph: "\u2318", spoken: "Command" });
  expect(keyGlyph("Mod", false)).toEqual({ glyph: "Ctrl", spoken: "Control" });
  expect(keyGlyph("Alt", true).spoken).toBe("Option");
  expect(keyGlyph("Backspace", true)).toEqual({ glyph: "\u232b", spoken: "Delete" });
  // A letter is printed in capitals, and an unknown key as itself.
  expect(keyGlyph("z", false)).toEqual({ glyph: "Z", spoken: "Z" });
  expect(keyGlyph("F10", false)).toEqual({ glyph: "F10", spoken: "F10" });
});

test("a shortcut's caps are hidden, and one sentence is read instead", () => {
  const { container } = render(<Kbd keys={["Mod", "Shift", "z"]} apple />);
  const drawn = container.querySelector("[aria-hidden='true']")!;
  expect([...drawn.querySelectorAll("kbd")].map((cap) => cap.textContent)).toEqual([
    "\u2318",
    "\u21e7",
    "Z",
  ]);
  expect(container.textContent).toContain("Command plus Shift plus Z");
});
