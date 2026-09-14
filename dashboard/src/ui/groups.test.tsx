/**
 * The keyboard contract of a group of choices, and the role each group claims.
 *
 * Both were missing, and they fail in opposite directions. `Segmented` and
 * `Tabs` rendered a row of ordinary buttons under `role="tablist"`: the ARIA
 * promised one tab stop with arrow keys inside it, and the DOM delivered N tab
 * stops with no arrow keys at all — so a keyboard reader got neither the
 * behaviour the role implies nor the behaviour plain buttons would have given
 * them. And `Segmented`'s role was wrong on top of that: a tab controls a
 * panel it labels, and not one call site does that.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Segmented, Tabs } from "./primitives.tsx";

afterEach(cleanup);

type Lens = "chart" | "directory" | "charter";

const LENSES: { value: Lens; label: string }[] = [
  { value: "chart", label: "Chart" },
  { value: "directory", label: "Directory" },
  { value: "charter", label: "Charter" },
];

/** The control as a screen uses it: it owns the value and re-renders. */
function LiveSegmented({ onChange }: { onChange?: (v: Lens) => void }) {
  const [value, setValue] = useState<Lens>("chart");
  return (
    <Segmented<Lens>
      ariaLabel="Org view"
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange?.(v);
      }}
      options={LENSES}
    />
  );
}

// A SEGMENTED CONTROL IS A RADIO GROUP, NOT A TAB LIST.
//
// All eight call sites pick a theme, a density, a grouping, a scope, a window
// or a lens; none of them controls a `tabpanel`, and several sit in a screen
// head with the content they affect hundreds of pixels below. `role="tab"`
// promises a panel relationship that does not exist.
test("a segmented control announces as a radio group", () => {
  render(<LiveSegmented />);
  expect(screen.getByRole("radiogroup", { name: "Org view" })).toBeTruthy();
  expect(screen.getAllByRole("radio")).toHaveLength(3);
  // The CONTROL: no tab role survives anywhere, so this cannot pass on a
  // component that simply added radios beside the tabs it already had.
  expect(screen.queryAllByRole("tab")).toHaveLength(0);
  expect(screen.queryByRole("tablist")).toBeNull();
});

// ONE TAB STOP FOR THE GROUP. Every option being its own stop is what a row of
// plain buttons gives you, and the shell alone renders two of these groups —
// six stops in front of the page content, on every screen.
test("only the selected option is in the page's tab order", () => {
  render(<LiveSegmented />);
  const stops = screen.getAllByRole("radio").map((b) => b.getAttribute("tabindex"));
  expect(stops).toEqual(["0", "-1", "-1"]);
});

// THE ARROW KEYS THE ROLE PROMISES. Without them the group has one reachable
// option and no way to change it from the keyboard at all — which is strictly
// worse than the untouched buttons, because those at least all took focus.
test("arrow keys move the selection, and wrap", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const group = screen.getByRole("radiogroup");

  fireEvent.keyDown(group, { key: "ArrowRight" });
  expect(moved).toHaveBeenLastCalledWith("directory");
  fireEvent.keyDown(group, { key: "ArrowRight" });
  expect(moved).toHaveBeenLastCalledWith("charter");
  // WRAPS: a reader holding the key gets the whole group rather than
  // stopping at an end they cannot see.
  fireEvent.keyDown(group, { key: "ArrowRight" });
  expect(moved).toHaveBeenLastCalledWith("chart");
  fireEvent.keyDown(group, { key: "ArrowLeft" });
  expect(moved).toHaveBeenLastCalledWith("charter");
});

// BOTH AXES. These render as a horizontal row today, but a group that wraps to
// two lines is the same control, and Down is what a reader presses on it.
test("the vertical arrows move it too", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const group = screen.getByRole("radiogroup");
  fireEvent.keyDown(group, { key: "ArrowDown" });
  expect(moved).toHaveBeenLastCalledWith("directory");
  fireEvent.keyDown(group, { key: "ArrowUp" });
  expect(moved).toHaveBeenLastCalledWith("chart");
});

test("Home and End jump to the ends", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const group = screen.getByRole("radiogroup");
  fireEvent.keyDown(group, { key: "End" });
  expect(moved).toHaveBeenLastCalledWith("charter");
  fireEvent.keyDown(group, { key: "Home" });
  expect(moved).toHaveBeenLastCalledWith("chart");
});

// FOCUS FOLLOWS THE SELECTION. Changing which option carries `tabIndex=0`
// without moving focus leaves the reader focused on an option that is no
// longer in the tab order — so their next Tab leaves the group from nowhere,
// and a second arrow press does nothing at all.
test("focus moves with the selection", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  fireEvent.keyDown(group, { key: "ArrowRight" });
  const [, directory] = screen.getAllByRole("radio");
  expect(document.activeElement).toBe(directory);
  expect(directory!.getAttribute("tabindex")).toBe("0");
});

// A KEY THE GROUP DOES NOT HANDLE IS THE BROWSER'S. Tab above all: swallowing
// it is how a group becomes a keyboard trap, and this handler is on the
// container every key inside it bubbles to.
test("keys it does not own are left alone", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  for (const key of ["Tab", "a", "Enter", "PageDown"]) {
    const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
    group.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  }
  // …and one it does own IS taken, or the assertion above passes on a
  // component with no handler at all.
  const own = new KeyboardEvent("keydown", {
    key: "ArrowRight",
    bubbles: true,
    cancelable: true,
  });
  group.dispatchEvent(own);
  expect(own.defaultPrevented).toBe(true);
});

type Tab = "overview" | "model" | "cost";

// TABS KEEP THE TAB ROLE — they are the one control here that genuinely is a
// tab list, with its panels directly beneath it — and gain the same keyboard.
test("a tab list keeps its role and gets the same keyboard", () => {
  function LiveTabs() {
    const [value, setValue] = useState<Tab>("overview");
    return (
      <Tabs<Tab>
        ariaLabel="Seat sections"
        value={value}
        onChange={setValue}
        options={[
          { value: "overview", label: "Overview" },
          { value: "model", label: "Model activity" },
          { value: "cost", label: "Cost" },
        ]}
      />
    );
  }
  render(<LiveTabs />);
  const list = screen.getByRole("tablist", { name: "Seat sections" });
  expect(screen.getAllByRole("tab").map((b) => b.getAttribute("tabindex"))).toEqual([
    "0",
    "-1",
    "-1",
  ]);
  fireEvent.keyDown(list, { key: "ArrowRight" });
  const [, model] = screen.getAllByRole("tab");
  expect(model!.getAttribute("aria-selected")).toBe("true");
  expect(document.activeElement).toBe(model);
});
