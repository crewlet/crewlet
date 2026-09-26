/**
 * Who this browser is, and the reader's own preferences.
 *
 * THREE STATES, ONE OF THEM A PERSON, each a sentence rather than a blank
 * avatar — because "who does the engine think I am" is the question every
 * write this dashboard makes turns on. And the zone and the date format had
 * no control anywhere before this block: `lib/prefs.ts` kept both and nothing
 * could set them.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { Preferences, UserBlock, supportedZones, whoLine } from "./UserBlock.tsx";
import { Router } from "~/app/router.tsx";
import { dates, reloadForTest, zone, zoneIsChosen } from "~/lib/prefs.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { ViewerState } from "~/lib/viewer.ts";

const nobody: ViewerState = {
  operatorID: "",
  operator: false,
  handle: "",
  name: "",
  kind: "",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
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
});

describe("who the block says this is", () => {
  test("a bound token is the person, with their seat and never a bare handle", () => {
    expect(whoLine(ada, "Founder")).toEqual({
      name: "Ada Lovelace",
      detail: "Founder · human seat",
    });
    // A seat whose name IS the person's says so once.
    expect(whoLine(ada, "Ada Lovelace").detail).toBe("human seat");
    expect(whoLine(ada, "Founder").detail).not.toContain("@");
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

  // THE THEME IS THREE CHOICES, and the third is what makes the popover the
  // place to set it: the one-press flip beside the name can only say light or
  // dark, and "follow the system" is a choice a reader has to be able to get
  // back to. Each choice is read back from `lib/prefs.ts` and from the root
  // attribute the stylesheet keys on — and "system" REMOVES the attribute, so
  // the media query decides and keeps deciding when the OS setting changes.
  test("the theme is light, dark or the system's, and each is kept", () => {
    open();
    const theme = screen.getByRole("radiogroup", { name: "Theme" });
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

  // THE ZONE AND THE DATE FORMAT HAD NO CONTROL. Both were read by every
  // timestamp in the product, and a reader could set neither.
  test("the date format is a control, and it writes the preference", async () => {
    open();
    fireEvent.click(screen.getByRole("combobox", { name: "Dates" }));
    // The kit's listbox commits on the press, as a native select does.
    fireEvent.mouseDown(await screen.findByRole("option", { name: "2026-09-22" }));
    await waitFor(() => expect(dates()).toBe("iso"));
  });

  test("the zone is a control, empty is the browser's, and a choice is kept", async () => {
    open();
    expect(zoneIsChosen()).toBe(false);
    // A SEARCHABLE select: the trigger is a button, and the field inside the
    // list is the combobox.
    fireEvent.click(screen.getByRole("button", { name: "Time zone" }));
    fireEvent.change(await screen.findByPlaceholderText("Find a zone"), {
      target: { value: "UTC" },
    });
    const utc = await screen.findAllByRole("option", { name: "UTC" });
    fireEvent.mouseDown(utc[0]!);
    await waitFor(() => expect(zone()).toBe("UTC"));
    expect(zoneIsChosen()).toBe(true);
  });

  test("every zone offered is one this runtime can format in, UTC among them", () => {
    const zones = supportedZones();
    expect(zones.length).toBeGreaterThan(0);
    // The runtime's canonical list omits it; the reader reading logs wants it.
    expect(zones).toContain("UTC");
    for (const z of zones.slice(0, 50)) {
      expect(() => new Intl.DateTimeFormat("en", { timeZone: z }), z).not.toThrow();
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
