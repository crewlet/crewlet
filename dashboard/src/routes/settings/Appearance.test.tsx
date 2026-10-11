/**
 * Settings › Appearance: the mode, the density and the theme, for this
 * browser alone.
 *
 * What it has to get right is that pointing at a theme shows it in the pane
 * and nowhere else, that choosing one applies it everywhere and keeps it, and
 * that the page is the reader's own: an address under Personal that no
 * credential guards.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { themeRegistry } from "@crewlethq/tokens";
import { HIGHLIGHT_REST_MS, ToastProvider } from "@crewlethq/ui";

import { Appearance } from "./Appearance.tsx";
import { crumbsFor } from "~/app/crumbs.ts";
import { sectionOf } from "~/app/nav.ts";
import { Router } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

const root = () => document.documentElement;

beforeEach(() => {
  localStorage.clear();
  reloadForTest();
  location.hash = "#/settings/appearance";
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  reloadForTest();
  for (const name of ["data-theme", "data-density", "data-palette"]) root().removeAttribute(name);
  location.hash = "#/";
});

function mount() {
  render(
    <ToastProvider>
      <Router>
        <Appearance />
      </Router>
    </ToastProvider>,
  );
}

const pane = () => screen.getByRole("complementary", { name: "Theme preview" });

describe("the address", () => {
  test("is a section of Settings under Personal, guarded by nothing", () => {
    const where = resolve(["settings", "appearance"]);
    expect(where.resolved && where.screen).toBe("appearance");
    expect(resolve(["settings", "appearance", "more"]).resolved).toBe(false);
    const section = sectionOf(["settings", "appearance"]);
    expect(section?.key).toBe("appearance");
    expect(section?.group).toBe("Personal");
    expect(section?.guarded).toBeUndefined();
    expect(crumbsFor(["settings", "appearance"]).map((crumb) => crumb.label)).toEqual([
      "Settings",
      "Appearance",
    ]);
  });
});

describe("the page", () => {
  test("offers the mode and the density as words, and every theme under its group", () => {
    mount();
    const mode = screen.getByRole("radiogroup", { name: "Mode" });
    expect(
      within(mode)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["System", "Light", "Dark"]);
    const density = screen.getByRole("radiogroup", { name: "Density" });
    expect(
      within(density)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["Compact", "Normal", "Comfortable"]);
    const gallery = screen.getByRole("radiogroup", { name: "Theme" });
    expect(within(gallery).getAllByRole("radio")).toHaveLength(themeRegistry.length);
    expect(
      within(gallery).getByRole("radio", { name: "Crewlet, Default" }).getAttribute("aria-checked"),
    ).toBe("true");
  });

  test("a mode and a density chosen here are the browser's", () => {
    mount();
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    expect(root().getAttribute("data-theme")).toBe("light");
    fireEvent.click(screen.getByRole("radio", { name: "Comfortable" }));
    expect(root().getAttribute("data-density")).toBe("comfortable");
    expect(localStorage.getItem(STORAGE_KEYS.density)).toBe("comfortable");
  });

  test("pointing at a theme shows it in the pane and nowhere else", () => {
    vi.useFakeTimers();
    try {
      mount();
      expect(within(pane()).getByText("Crewlet")).toBeTruthy();
      fireEvent.mouseEnter(screen.getByRole("radio", { name: "Dusk, Soft lavender" }));
      act(() => vi.advanceTimersByTime(HIGHLIGHT_REST_MS));
      expect(within(pane()).getByText("Dusk")).toBeTruthy();
      // The window stays on the theme in effect, and nothing is written.
      expect(root().hasAttribute("data-palette")).toBe(false);
      expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
      fireEvent.mouseLeave(document.querySelector(".crewlet-theme-gallery")!);
      expect(within(pane()).getByText("Crewlet")).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  test("choosing a theme applies it everywhere, keeps it, and offers the way back", () => {
    mount();
    fireEvent.click(screen.getByRole("radio", { name: "Ledger, Cobalt on stone" }));
    expect(root().getAttribute("data-palette")).toBe("ledger");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("ledger");
    expect(
      screen.getByRole("radio", { name: "Ledger, Cobalt on stone" }).getAttribute("aria-checked"),
    ).toBe("true");
    expect(within(pane()).getByText("Ledger")).toBeTruthy();
    // The arrows choose as they move, as a setting's do.
    const ledger = screen.getByRole("radio", { name: "Ledger, Cobalt on stone" });
    ledger.focus();
    fireEvent.keyDown(ledger, { key: "ArrowRight" });
    expect(root().getAttribute("data-palette")).toBe("high-contrast");
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    expect(root().getAttribute("data-palette")).toBe("ledger");
  });

  test("the pane draws the theme in the mode the console is in", () => {
    mount();
    fireEvent.click(screen.getByRole("radio", { name: "Light" }));
    fireEvent.click(screen.getByRole("radio", { name: "Abyss, Vivid violet" }));
    const abyss = themeRegistry.find((theme) => theme.id === "abyss")!;
    const half = pane().querySelector<HTMLElement>(".crewlet-theme-swatch__half");
    expect(pane().querySelectorAll(".crewlet-theme-swatch__half")).toHaveLength(1);
    expect(half?.style.getPropertyValue("--crewlet-swatch-frame")).toBe(abyss.swatch.light.frame);
  });
});
