/**
 * Keyboard semantics of the sortable table.
 *
 * A sortable column was a click handler on a header cell, which a keyboard
 * could not reach at all. The row controls this suite also covered are the
 * design system's now, and what the engine decides about them, which row is a
 * set of SECTIONS and which is a SETTING, is asserted at that seam in
 * `uilet.test.tsx`.
 *
 * Enter and Space are not driven here: on a real `<button>` the browser turns
 * both into the click this suite does drive, and jsdom does not synthesise it.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { DataTable } from "./DataTable.tsx";

afterEach(cleanup);

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
