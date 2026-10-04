/**
 * An object's tabs fold what does not fit into "More", and every tab stays
 * reachable without scrolling.
 *
 * THE PHONE CASE IS THE CASE. A seat's six tabs at 390px ran past the strip's
 * edge with the scrollbar hidden: Settings began five pixels past the end and
 * nothing said it was there. Measured here the way the strip measures — the
 * twin's tabs 100 wide, the trigger 70 — so the fold is the arithmetic the
 * page runs, not a guess about it.
 */

import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { ObjectTabs, type ObjectTab } from "./ObjectTabs.tsx";

const TABS: ObjectTab[] = [
  { value: "overview", label: "Overview" },
  { value: "work", label: "Work", count: 12 },
  { value: "turns", label: "Turns" },
  { value: "memory", label: "Memory" },
  { value: "schedules", label: "Schedules" },
  { value: "settings", label: "Settings" },
];

const had = {
  rect: HTMLElement.prototype.getBoundingClientRect,
  client: Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth"),
};

/** Every twin tab 100 wide, the twin's "More" 70, the strip `space` wide. */
function lay(space: number): void {
  HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
    const inTwin = !!this.closest(".object-tabs-twin-row");
    const w =
      inTwin && this.getAttribute("role") === "tab"
        ? 100
        : this.hasAttribute("data-twin-more")
          ? 70
          : 0;
    return { width: w, height: 20, top: 0, left: 0, right: w, bottom: 20, x: 0, y: 0 } as DOMRect;
  };
  Object.defineProperty(HTMLElement.prototype, "clientWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return this.classList.contains("object-tabs") ? space : 0;
    },
  });
}

afterEach(() => {
  cleanup();
  HTMLElement.prototype.getBoundingClientRect = had.rect;
  if (had.client) Object.defineProperty(HTMLElement.prototype, "clientWidth", had.client);
  else delete (HTMLElement.prototype as unknown as Record<string, unknown>).clientWidth;
});

function Harness({ start = "overview" }: { start?: string }) {
  const [value, setValue] = useState(start);
  return (
    <>
      <ObjectTabs ariaLabel="SWE's sections" items={TABS} value={value} onValueChange={setValue} />
      <output data-testid="chosen">{value}</output>
    </>
  );
}

const drawn = () =>
  within(screen.getByRole("tablist", { name: "SWE's sections" }))
    .getAllByRole("tab")
    .map((t) => t.textContent?.replace(/\d+$/, ""));

async function folded(): Promise<string[]> {
  fireEvent.click(screen.getByRole("button", { name: /More/ }));
  const items = await screen.findAllByRole("menuitem");
  return items.map((i) => i.textContent?.replace(/\d+$/, "") ?? "");
}

// 366 of room: "More" (70) and Overview (100) are 170, Work makes 270, and a
// third tab would be 370.
test("a phone's strip draws the tabs that fit and every other tab is one press away", async () => {
  lay(366);
  render(<Harness />);
  await waitFor(() => expect(drawn()).toEqual(["Overview", "Work"]));
  // EVERY TAB IS REACHABLE: drawn or in the menu, each exactly once.
  const rest = await folded();
  expect(rest).toEqual(["Turns", "Memory", "Schedules", "Settings"]);
  expect([...drawn(), ...rest].sort()).toEqual(TABS.map((t) => t.label).sort());
});

test("a tab picked from More is selected, and drawn on the strip in the last place that fits", async () => {
  lay(366);
  render(<Harness />);
  await waitFor(() => expect(drawn()).toHaveLength(2));
  await folded();
  fireEvent.click(screen.getByRole("menuitem", { name: /Settings/ }));
  expect(screen.getByTestId("chosen").textContent).toBe("settings");
  // THE ONE THE READER IS ON IS ALWAYS DRAWN.
  await waitFor(() => expect(drawn()).toEqual(["Overview", "Settings"]));
  const selected = screen
    .getAllByRole("tab")
    .find((t) => t.getAttribute("aria-selected") === "true");
  expect(selected?.textContent).toBe("Settings");
});

test("a strip with room for every tab draws no More", async () => {
  lay(2000);
  render(<Harness />);
  await waitFor(() => expect(drawn()).toHaveLength(6));
  expect(screen.queryByRole("button", { name: /More/ })).toBeNull();
});

// THE TWIN IS A SECOND COPY OF A CONTROL: nothing may reach it.
test("the twin the widths are read from is hidden from everything", () => {
  lay(366);
  const { container } = render(<Harness />);
  const twin = container.querySelector(".object-tabs-twin") as HTMLElement;
  expect(twin.getAttribute("aria-hidden")).toBe("true");
  expect(twin.hasAttribute("inert")).toBe(true);
  expect(screen.getAllByRole("tablist")).toHaveLength(1);
});
