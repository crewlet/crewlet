/**
 * Choosing several from many: the combobox and listbox semantics a screen
 * reader relies on, toggling without losing the search, and Escape closing
 * the list rather than the editor it sits in.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Dialog } from "./Dialog.tsx";
import { Drawer } from "./Drawer.tsx";
import { MultiPicker, type PickerOption } from "./MultiPicker.tsx";

afterEach(cleanup);

const OPTIONS: PickerOption[] = [
  {
    value: "Software Engineer",
    label: "Software Engineer",
    group: "Seats",
    hint: "software-engineer",
  },
  {
    value: "Site Reliability",
    label: "Site Reliability",
    group: "Seats",
    hint: "site-reliability",
  },
  { value: "Designer", label: "Designer", group: "Seats", hint: "designer" },
  { value: "Engineering", label: "Engineering", group: "Units" },
];

function Manages({ initial = [] as string[], inDrawer = false, onClose = () => {} }) {
  const [value, setValue] = useState(initial);
  const picker = (
    <MultiPicker label="Manages" options={OPTIONS} value={value} onChange={setValue} />
  );
  return (
    <>
      {inDrawer ? (
        <Drawer title="Edit seat" onClose={onClose}>
          {picker}
        </Drawer>
      ) : (
        picker
      )}
      <pre data-testid="value">{JSON.stringify(value)}</pre>
    </>
  );
}

const current = () => JSON.parse(screen.getByTestId("value").textContent ?? "[]") as string[];
const box = () => screen.getByRole("combobox", { name: "Manages" }) as HTMLInputElement;
const announced = () => screen.getByRole("status").textContent;

function highlighted(): string | null {
  const id = box().getAttribute("aria-activedescendant");
  return id ? (document.getElementById(id)?.textContent ?? null) : null;
}

test("the search box is a labelled list combobox, and the list is multiselectable", () => {
  render(<Manages />);
  expect(box().getAttribute("aria-autocomplete")).toBe("list");
  expect(box().getAttribute("aria-expanded")).toBe("false");
  fireEvent.keyDown(box(), { key: "ArrowDown" });
  const list = screen.getByRole("listbox", { name: "Manages" });
  expect(list.getAttribute("aria-multiselectable")).toBe("true");
  expect(box().getAttribute("aria-controls")).toBe(list.id);
  expect(box().getAttribute("aria-expanded")).toBe("true");
  expect(screen.getByRole("group", { name: "Units" })).toBeDefined();
});

test("typing narrows by label or value, case-insensitively", () => {
  render(<Manages />);
  fireEvent.change(box(), { target: { value: "ENG" } });
  const shown = screen.getAllByRole("option").map((o) => o.textContent);
  expect(shown).toEqual(["Software Engineersoftware-engineer", "Engineering"]);
});

test("Enter toggles the highlighted option, keeps the list and the search, and says what changed", () => {
  render(<Manages />);
  fireEvent.change(box(), { target: { value: "s" } });
  expect(highlighted()).toContain("Software Engineer");
  fireEvent.keyDown(box(), { key: "ArrowDown" });
  expect(highlighted()).toContain("Site Reliability");

  expect(fireEvent.keyDown(box(), { key: "Enter" })).toBe(false);
  expect(current()).toEqual(["Site Reliability"]);
  expect(
    screen.getByRole("option", { name: /Site Reliability/ }).getAttribute("aria-selected"),
  ).toBe("true");
  expect(box().value).toBe("s");
  expect(screen.getByRole("listbox")).toBeDefined();
  expect(announced()).toBe("Added Site Reliability");

  fireEvent.keyDown(box(), { key: "Enter" });
  expect(current()).toEqual([]);
  expect(announced()).toBe("Removed Site Reliability");
});

test("a press on an option toggles it, appended in the order chosen", () => {
  render(<Manages initial={["Designer"]} />);
  fireEvent.click(box());
  fireEvent.mouseDown(screen.getByRole("option", { name: /Engineering/ }));
  expect(current()).toEqual(["Designer", "Engineering"]);
});

test("a chosen value has a named Remove, and Backspace in an empty search removes the last", () => {
  render(<Manages initial={["Designer", "Engineering"]} />);
  const chips = screen.getByRole("list", { name: "Chosen: Manages" });
  expect(chips.textContent).toContain("Designer");
  fireEvent.click(screen.getByRole("button", { name: "Remove Designer" }));
  expect(current()).toEqual(["Engineering"]);
  expect(announced()).toBe("Removed Designer");

  fireEvent.keyDown(box(), { key: "Backspace" });
  expect(current()).toEqual([]);
  expect(announced()).toBe("Removed Engineering");
});

test("a chosen value the options no longer offer is shown as itself, not dropped", () => {
  render(<Manages initial={["Former Unit"]} />);
  expect(screen.getByRole("button", { name: "Remove Former Unit" })).toBeDefined();
});

test("Escape closes the list and leaves the drawer open; a second Escape closes the drawer", () => {
  let closed = 0;
  render(<Manages inDrawer onClose={() => closed++} />);
  box().focus();
  fireEvent.keyDown(box(), { key: "ArrowDown" });
  fireEvent.keyDown(box(), { key: "Escape" });
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(closed).toBe(0);
  fireEvent.keyDown(box(), { key: "Escape" });
  expect(closed).toBe(1);
});

test("a search that matches nothing says so, offers no listbox, and Escape still closes only that", () => {
  let closed = 0;
  render(<Manages inDrawer onClose={() => closed++} />);
  box().focus();
  fireEvent.change(box(), { target: { value: "zzz" } });
  expect(screen.getByText(/Nothing matches/)).toBeDefined();
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(box().getAttribute("aria-expanded")).toBe("false");
  // Enter with a search typed is not a save.
  expect(fireEvent.keyDown(box(), { key: "Enter" })).toBe(false);
  fireEvent.keyDown(box(), { key: "Escape" });
  expect(screen.queryByText(/Nothing matches/)).toBeNull();
  expect(closed).toBe(0);
});

test("a press on the veil with the list open closes the list and leaves the dialog", () => {
  const closed = vi.fn();
  const { container } = render(
    <Dialog title="Edit seat" onClose={closed}>
      <Manages />
    </Dialog>,
  );
  fireEvent.keyDown(box(), { key: "ArrowDown" });
  expect(screen.getByRole("listbox")).toBeDefined();

  const veil = container.querySelector(".veil")!;
  fireEvent.pointerDown(veil);
  fireEvent.click(veil);
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(closed).not.toHaveBeenCalled();
});

test("with no list on screen, Escape is the drawer's on the first press", () => {
  let closed = 0;
  function Empty() {
    return (
      <Drawer title="Edit seat" onClose={() => closed++}>
        <MultiPicker label="Manages" options={[]} value={[]} onChange={() => {}} />
      </Drawer>
    );
  }
  render(<Empty />);
  const input = screen.getByRole("combobox", { name: "Manages" });
  input.focus();
  // Asked to open, with nothing to offer and nothing typed: nothing is drawn.
  fireEvent.click(input);
  expect(screen.queryByRole("listbox")).toBeNull();
  fireEvent.keyDown(input, { key: "Escape" });
  expect(closed).toBe(1);
});
