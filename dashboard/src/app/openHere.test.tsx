/**
 * A frame popover is open only on the page it was opened on: Back, Forward or a
 * link closes it, as the palette and the drawer close.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Popover } from "@crewlethq/ui";
import { Router, useOpenHere } from "./router.tsx";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function Prefs() {
  const [open, setOpen] = useOpenHere();
  return (
    <Popover
      label="Preferences"
      open={open}
      onOpenChange={setOpen}
      trigger={(_open, toggle) => (
        <button type="button" onClick={toggle}>
          Preferences
        </button>
      )}
    >
      <p>Mode</p>
    </Popover>
  );
}

/* It stayed open over the next screen. Mutation: hold the state in a plain useState. */
test("a frame popover closes when the route moves, and opens again on the new one", () => {
  location.hash = "#/home";
  render(
    <Router>
      <Prefs />
    </Router>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Preferences" }));
  expect(screen.getByRole("dialog", { name: "Preferences" })).toBeTruthy();
  act(() => {
    location.hash = "#/inbox";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  expect(screen.queryByRole("dialog", { name: "Preferences" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Preferences" }));
  expect(screen.getByRole("dialog", { name: "Preferences" })).toBeTruthy();
});
