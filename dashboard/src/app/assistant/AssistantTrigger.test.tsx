/**
 * The Ask button: no keycap drawn on it, its shortcut kept as its own
 * property and in its tooltip, and the dot worn on the mark's corner with the
 * reason in the button's name and its tooltip.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { AssistantTrigger } from "./AssistantTrigger.tsx";
import { AssistantControlContext, type AssistantControl } from "./control.ts";
import { setAssistantActivity } from "./status.ts";
import { ariaKeysOf, keyRow, pressText } from "../keymap.ts";

afterEach(() => {
  cleanup();
  act(() => setAssistantActivity("idle"));
});

const ready: AssistantControl = { open: false, unavailable: null, toggle: () => {}, ask: () => {} };

function draw(control: AssistantControl = ready) {
  return render(
    <AssistantControlContext.Provider value={control}>
      <AssistantTrigger />
    </AssistantControlContext.Provider>,
  );
}

test("draws no keycap, and keeps its shortcut as its own property and in its tooltip", () => {
  const { container } = draw();
  const button = screen.getByRole("button", { name: "Ask" });
  expect(container.querySelector(".crewlet-kbd, kbd")).toBeNull();
  expect(button.getAttribute("aria-keyshortcuts")).toBe("Meta+. Control+.");
  const press = keyRow("assistant.open").presses[0]!;
  expect(button.getAttribute("title")).toBe(`Ask Crewlet (${pressText(press)})`);
});

test("wears the dot on the mark's corner, with the reason in its name and its tooltip", () => {
  const { container } = draw();
  expect(container.querySelector(".crewlet-indicator-anchor__indicator")).toBeNull();
  act(() => setAssistantActivity("waiting"));
  const button = screen.getByRole("button", { name: "Ask, A change waits on your approval" });
  const indicator = container.querySelector(".crewlet-indicator-anchor__indicator")!;
  // ON THE MARK, before the word, never after it.
  expect(indicator.closest(".crewlet-indicator-anchor")!.querySelector("svg")).not.toBeNull();
  expect(indicator.querySelector(".crewlet-status-dot--warning")).not.toBeNull();
  expect(button.textContent!.indexOf("Ask")).toBeGreaterThan(-1);
  expect(button.getAttribute("title")).toMatch(/\. A change waits on your approval\.$/);
});

test("an open panel says its state itself, so the button wears no dot", () => {
  act(() => setAssistantActivity("working"));
  const { container } = draw({ ...ready, open: true });
  expect(container.querySelector(".crewlet-indicator-anchor__indicator")).toBeNull();
  expect(screen.getByRole("button", { name: "Ask" })).toBeTruthy();
});

test("a command-key press is named for both platforms", () => {
  expect(ariaKeysOf({ key: "k", mod: true })).toBe("Meta+K Control+K");
  expect(ariaKeysOf({ key: "escape" })).toBe("Escape");
  expect(pressText({ key: ".", mod: true }, true)).toBe("⌘.");
  expect(pressText({ key: ".", mod: true }, false)).toBe("Ctrl+.");
});
