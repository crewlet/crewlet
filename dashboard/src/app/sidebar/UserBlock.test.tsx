/**
 * Who this browser is, and the reader's own preferences.
 *
 * THREE STATES, ONE OF THEM A PERSON, each a sentence rather than a blank
 * avatar — because "who does the engine think I am" is the question every
 * write this dashboard makes turns on. The preferences are the mode, the
 * density, the theme and the token; the zone and the date's shape are the
 * browser's.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { themeRegistry } from "@crewlethq/tokens";
import { HIGHLIGHT_REST_MS, ToastProvider } from "@crewlethq/ui";

import { Preferences, UserBlock, whoLine } from "./UserBlock.tsx";
import { Router } from "~/app/router.tsx";
import { reloadForTest } from "~/lib/prefs.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { ViewerState } from "~/lib/viewer.ts";

const nobody: ViewerState = {
  operatorID: "",
  operator: false,
  handle: "",
  name: "",
  acts: [],
  project: "",
  kind: "",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: true,
  configManagedBy: [],
  chatBackend: "",
  chatVendors: [],
};
const ada: ViewerState = {
  ...nobody,
  operatorID: "U0FOUNDER",
  operator: true,
  handle: "ada",
  name: "Ada Lovelace",
  kind: "human",
};

beforeEach(() => {
  localStorage.clear();
  reloadForTest();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  reloadForTest();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-density");
  document.documentElement.removeAttribute("data-palette");
});

describe("who the block says this is", () => {
  test("a bound token is the person, with their seat's role and never a bare handle", () => {
    expect(whoLine(ada, "Founder")).toEqual({ name: "Ada Lovelace", detail: "Founder" });
    // A seat whose name IS the person's says nothing more, and a person is
    // never captioned "human seat", which says what the block already shows.
    expect(whoLine(ada, "Ada Lovelace").detail).toBe("");
    expect(whoLine(ada, "Founder").detail).not.toContain("@");
    // A token bound to an agent's seat says so: acting as an agent is the
    // fact a reader must not miss.
    expect(whoLine({ ...ada, kind: "agent" }, "Release Bot").detail).toBe(
      "Release Bot · agent seat",
    );
  });

  test("a person with no role to show draws no second line", () => {
    render(
      <Router>
        <UserBlock viewer={ada} seatName="Ada Lovelace" onSetToken={() => {}} />
      </Router>,
    );
    const who = screen.getByRole("link", { name: /Ada Lovelace/ });
    expect(who.querySelector(".user-block-detail")).toBeNull();
    expect(who.textContent).not.toContain("human seat");
  });

  test("an unbound token names the token, and says it is not a fault", () => {
    const r = whoLine({ ...nobody, operatorID: "ops-7", operator: true, unbound: true }, "");
    expect(r).toEqual({ name: "Token ops-7", detail: "Not bound to a seat" });
  });

  test("no token and not-yet-known are their own sentences", () => {
    expect(whoLine({ ...nobody, anonymous: true }, "").name).toBe("Anonymous");
    expect(whoLine({ ...nobody, loading: true }, "").name).toBe("Checking who you are");
  });

  test("only a person is a link, and it goes to their seat", () => {
    render(
      <Router>
        <UserBlock viewer={ada} seatName="Founder" onSetToken={() => {}} />
      </Router>,
    );
    expect(screen.getByRole("link", { name: /Ada Lovelace/ }).getAttribute("href")).toBe(
      "#/agents/seats/ada",
    );
    cleanup();
    render(
      <Router>
        <UserBlock viewer={{ ...nobody, anonymous: true }} seatName="" onSetToken={() => {}} />
      </Router>,
    );
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("Anonymous")).toBeTruthy();
  });
});

// THE POPOVER OPENS WHERE THE SIDEBAR PUTS IT. jsdom has no layout, so a
// suite that merely clicks the trigger could not see that the panel was
// closed again on the frame it opened: the kit's placement reads rectangles,
// and at the sidebar's real coordinates (the trigger 26px wide at x=202, the
// panel 320 wide, a 1440x900 window) an end-aligned panel measured the anchor
// from a left edge off the window and closed as "anchor gone". So the
// rectangles are the real ones here.
describe("the preferences popover, laid out", () => {
  const rect = (x: number, y: number, width: number, height: number): DOMRect =>
    ({
      x,
      y,
      left: x,
      top: y,
      width,
      height,
      right: x + width,
      bottom: y + height,
      toJSON: () => ({}),
    }) as DOMRect;
  const window0 = { width: window.innerWidth, height: window.innerHeight };

  beforeEach(() => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1440 });
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 900 });
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (this.getAttribute("aria-label") === "Preferences" && this.tagName === "BUTTON") {
        return rect(202, 860, 26, 26);
      }
      if (this.getAttribute("role") === "dialog") return rect(0, 0, 320, 420);
      return rect(0, 0, 0, 0);
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    Object.defineProperty(window, "innerWidth", { configurable: true, value: window0.width });
    Object.defineProperty(window, "innerHeight", { configurable: true, value: window0.height });
  });

  test("pressed at the sidebar's foot, it stays open, above the trigger and on screen", async () => {
    render(
      <Router>
        <UserBlock viewer={ada} seatName="Founder" onSetToken={() => {}} />
      </Router>,
    );
    const trigger = screen.getByRole("button", { name: "Preferences" });
    fireEvent.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Preferences" });
    // Still there once placement has run, and the trigger says so.
    await new Promise((r) => setTimeout(r, 20));
    expect(dialog.isConnected).toBe(true);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    // Placed, not hidden: above the trigger, starting at its edge, inside the window.
    expect(dialog.style.visibility).not.toBe("hidden");
    const left = parseFloat(dialog.style.left);
    const top = parseFloat(dialog.style.top);
    expect(left).toBe(202);
    expect(left + 320).toBeLessThanOrEqual(1440);
    expect(top + 420).toBeLessThanOrEqual(860);
  });
});

describe("the preferences", () => {
  function open(tokenPresented = true) {
    const onSetToken = vi.fn();
    render(<Preferences onSetToken={onSetToken} tokenPresented={tokenPresented} />);
    return onSetToken;
  }

  // THE MODE IS THREE CHOICES, and the third is what makes the popover the
  // place to set it: the one-press flip beside the name can only say light or
  // dark, and "follow the system" is a choice a reader has to be able to get
  // back to. Each choice is read back from `lib/prefs.ts` and from the root
  // attribute the stylesheet keys on, and "system" REMOVES the attribute, so
  // the media query decides and keeps deciding when the OS setting changes.
  test("the mode is light, dark or the system's, and each is kept", () => {
    open();
    const theme = screen.getByRole("radiogroup", { name: "Mode" });
    const pick = (name: string) => fireEvent.click(within(theme).getByRole("radio", { name }));

    pick("Dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("dark");

    pick("Light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("light");

    pick("Follow the system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("system");
    expect(
      within(theme).getByRole("radio", { name: "Follow the system" }).getAttribute("aria-checked"),
    ).toBe("true");
  });

  // DENSITY IS THE OTHER SWITCHER, and "normal" is the absence of the
  // attribute for the same reason: the default is the stylesheet's own.
  test("the density is a control, and it writes the preference", () => {
    open();
    const density = screen.getByRole("radiogroup", { name: "Density" });
    fireEvent.click(within(density).getByRole("radio", { name: "Compact" }));
    expect(document.documentElement.getAttribute("data-density")).toBe("compact");
    expect(localStorage.getItem(STORAGE_KEYS.density)).toBe("compact");
    fireEvent.click(within(density).getByRole("radio", { name: "Normal" }));
    expect(document.documentElement.hasAttribute("data-density")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.density)).toBe("normal");
  });

  // THE ZONE AND THE DATE'S SHAPE ARE THE BROWSER'S, so the panel offers
  // neither: a control here would be a setting nothing reads.
  test("offers no time zone and no date format", () => {
    open();
    expect(screen.queryByText("Time zone")).toBeNull();
    expect(screen.queryByText("Dates")).toBeNull();
    // The one list here is the theme's.
    expect(screen.getAllByRole("combobox").map((box) => box.getAttribute("aria-label"))).toEqual([
      "Theme",
    ]);
  });

  // THE ROWS ARE MODE, DENSITY, THEME, in that order, and the two switchers
  // draw one box: the same width, three equal cells each.
  test("the mode, the density and the theme are one column of one width", () => {
    open();
    const rows = [...document.querySelectorAll(".preferences-row")].map(
      (row) => row.querySelector(".t-label")?.textContent,
    );
    expect(rows).toEqual(["Mode", "Density", "Theme"]);
    for (const name of ["Mode", "Density"]) {
      const group = screen.getByRole("radiogroup", { name });
      expect(group.classList.contains("crewlet-segmented--fill"), name).toBe(true);
      expect(group.parentElement?.className, name).toBe("preferences-switch");
    }
    // The theme's field sits in the same box, so the three draw one width.
    const theme = screen.getByRole("combobox", { name: "Theme" });
    expect(theme.closest(".preferences-switch")).toBeTruthy();
  });

  test("the theme lists every theme under its group, applies the pick and keeps it", () => {
    render(
      <ToastProvider>
        <Preferences onSetToken={() => {}} tokenPresented />
      </ToastProvider>,
    );
    const field = screen.getByRole("combobox", { name: "Theme" });
    expect(field.textContent).toContain("Crewlet");
    fireEvent.click(field);
    const list = screen.getByRole("listbox", { name: "Theme" });
    const names = within(list)
      .getAllByRole("option")
      .map((option) => option.querySelector(".crewlet-select__option-label")?.textContent);
    expect(names).toEqual(themeRegistry.map((theme) => theme.name));
    expect(within(list).getByRole("group", { name: "Purple" })).toBeTruthy();
    fireEvent.mouseDown(within(list).getByRole("option", { name: /Abyss/ }));
    expect(document.documentElement.getAttribute("data-palette")).toBe("abyss");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("abyss");
    expect(screen.getByRole("combobox", { name: "Theme" }).textContent).toContain("Abyss");
    // Said, with the way back, and the way back is the choice before.
    const undo = screen.getByRole("button", { name: "Undo" });
    // Drawn, and announced in the toaster's live region as well.
    expect(screen.getAllByText("Theme set to Abyss").length).toBeGreaterThan(0);
    fireEvent.click(undo);
    expect(document.documentElement.hasAttribute("data-palette")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
  });

  // A PREVIEW IS THE LIST'S, NEVER STORAGE'S: the frame takes the theme the
  // highlight rests on, and the list closing puts back the one in effect.
  test("the highlight previews the theme across the frame, and Escape puts back the one in effect", () => {
    vi.useFakeTimers();
    try {
      open();
      const field = screen.getByRole("combobox", { name: "Theme" });
      fireEvent.click(field);
      fireEvent.keyDown(field, { key: "ArrowDown" });
      expect(document.documentElement.getAttribute("data-palette")).toBe("violet");
      expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
      fireEvent.mouseEnter(screen.getByRole("option", { name: /Ledger/ }));
      act(() => vi.advanceTimersByTime(HIGHLIGHT_REST_MS));
      expect(document.documentElement.getAttribute("data-palette")).toBe("ledger");
      fireEvent.keyDown(field, { key: "Escape" });
      expect(document.documentElement.hasAttribute("data-palette")).toBe(false);
      expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  test("the token is set or changed from here, and says which", () => {
    const onSetToken = open(false);
    fireEvent.click(screen.getByRole("button", { name: "Set an API token" }));
    expect(onSetToken).toHaveBeenCalledOnce();
    cleanup();
    open(true);
    expect(screen.getByRole("button", { name: "Change the API token" })).toBeTruthy();
  });
});
