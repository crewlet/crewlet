/**
 * Keyboard semantics of the row controls and the sortable table.
 *
 * Three defects, none of them visible with a mouse. A lens or tab row selected
 * on every arrow press, and a tab is a section that pushes a history entry, so
 * Back walked through keypresses. The theme and density controls announced
 * themselves as tabs with no panels. And a sortable column was a click handler
 * on a header cell, which a keyboard could not reach at all.
 *
 * Enter and Space are not driven here: on a real `<button>` the browser turns
 * both into the click this suite does drive, and jsdom does not synthesise it.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { DataTable } from "./DataTable.tsx";
import { Segmented, TabPanel, Tabs, tabId } from "./primitives.tsx";

afterEach(cleanup);

const lenses = [
  { value: "chart", label: "Chart" },
  { value: "directory", label: "Directory" },
  { value: "charter", label: "Charter" },
];

describe("a tab row", () => {
  function mount(Row: "tabs" | "segmented") {
    const onChange = vi.fn();
    const props = { ariaLabel: "View", value: "chart", options: lenses, onChange };
    render(
      <>
        {Row === "tabs" ? (
          <Tabs {...props} panelId="panel" />
        ) : (
          <Segmented {...props} semantics="tabs" panelId="panel" />
        )}
        <TabPanel id="panel" value="chart">
          content
        </TabPanel>
      </>,
    );
    return onChange;
  }

  test.each(["tabs", "segmented"] as const)("%s: arrows move focus and select nothing", (row) => {
    const onChange = mount(row);
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
    expect(onChange).not.toHaveBeenCalled();
    fireEvent.click(tabs[2]!);
    expect(onChange).toHaveBeenCalledWith("charter");
  });

  test.each(["tabs", "segmented"] as const)(
    "%s: one tab stop, and the panel it controls",
    (row) => {
      mount(row);
      const tabs = screen.getAllByRole("tab");
      expect(tabs.map((t) => t.tabIndex)).toEqual([0, -1, -1]);
      expect(tabs[0]!.getAttribute("aria-selected")).toBe("true");
      for (const tab of tabs) expect(tab.getAttribute("aria-controls")).toBe("panel");
      const panel = screen.getByRole("tabpanel");
      expect(panel.getAttribute("aria-labelledby")).toBe(tabId("panel", "chart"));
      expect(document.getElementById(tabId("panel", "chart"))).toBe(tabs[0]);
      expect(screen.getByRole("tabpanel", { name: "Chart" })).toBe(panel);
    },
  );
});

describe("a setting", () => {
  const themes = [
    { value: "light", label: "", icon: "sun" as const, title: "Light" },
    { value: "system", label: "", icon: "monitor" as const, title: "Follow the system" },
    { value: "dark", label: "", icon: "moon" as const, title: "Dark" },
  ];

  test("is a radio group whose arrows select as they move", () => {
    const onChange = vi.fn();
    render(
      <Segmented
        ariaLabel="Theme"
        semantics="radio"
        value="system"
        options={themes}
        onChange={onChange}
      />,
    );
    const group = screen.getByRole("radiogroup", { name: "Theme" });
    const radios = within(group).getAllByRole("radio");
    // Named, although their only visible content is an icon.
    expect(screen.getByRole("radio", { name: "Dark" })).toBe(radios[2]);
    expect(radios[1]!.getAttribute("aria-checked")).toBe("true");
    expect(radios.map((r) => r.tabIndex)).toEqual([-1, 0, -1]);
    expect(screen.queryByRole("tab")).toBeNull();

    radios[1]!.focus();
    fireEvent.keyDown(radios[1]!, { key: "ArrowDown" });
    expect(document.activeElement).toBe(radios[2]);
    expect(onChange).toHaveBeenLastCalledWith("dark");
    fireEvent.keyDown(radios[2]!, { key: "ArrowUp" });
    expect(onChange).toHaveBeenLastCalledWith("system");
  });
});

describe("a sortable table", () => {
  const rows = [
    { name: "beta", tokens: 5 },
    { name: "alpha", tokens: 9 },
  ];

  test("sorts from a button in the header, with aria-sort on the cell", () => {
    const { container } = render(
      <DataTable
        rows={rows}
        rowKey={(r) => r.name}
        columns={[
          { key: "name", header: "Seat", cell: (r) => r.name, sortValue: (r) => r.name },
          { key: "note", header: "Note", cell: () => "" },
        ]}
      />,
    );
    const button = screen.getByRole("button", { name: /Seat/ });
    const header = screen.getByRole("columnheader", { name: /Seat/ });
    expect(header.contains(button)).toBe(true);
    // An unsortable column is a plain header, with nothing to press.
    expect(
      within(screen.getByRole("columnheader", { name: "Note" })).queryByRole("button"),
    ).toBeNull();

    expect(header.getAttribute("aria-sort")).toBeNull();
    fireEvent.click(button);
    expect(header.getAttribute("aria-sort")).toBe("descending");
    fireEvent.click(button);
    expect(header.getAttribute("aria-sort")).toBe("ascending");
    const firstCell = container.querySelector("tbody tr td");
    expect(firstCell?.textContent).toBe("alpha");
  });
});
