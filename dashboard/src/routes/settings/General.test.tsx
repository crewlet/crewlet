/**
 * Settings › General: the company's clock, the engine's row of `timezone`,
 * beside its default, with its key and its source, changed in place.
 */

import { act, cleanup, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { mountSettings, READER_VIEWER } from "~/test/settingsScreen.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { GeneralSettings } from "./General.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const CLOCK = {
  settings: [
    {
      key: "timezone",
      group: "company",
      kind: "zone",
      value: "Europe/Berlin",
      default: "UTC",
      set: true,
      off: false,
      has_off: false,
    },
  ],
};

async function show(viewer?: Record<string, unknown>) {
  mountSettings(
    <GeneralSettings />,
    { settings: CLOCK },
    {
      hash: "#/settings/general",
      org: { timezone: "Asia/Tokyo", roles: [] },
      ...(viewer ? { viewer } : {}),
    },
  );
  await act(afterMicrotasks);
}

// THE CLOCK KEEPS ITS KEY AND ITS SOURCE where it moved: the row is the
// engine's, a changed zone is marked, and its pencil and reset stand at the
// row's end. Mutation: draw the zone off the org projection for an admin, or
// drop the reset, and this fails.
test("the company clock is the engine's row, with its key, source and controls", async () => {
  await show();
  const clock = screen.getByRole("table", { name: "Company clock" });
  const zone = within(clock).getByRole("row", { name: /Time zone/ });
  expect(
    within(zone).getByRole("button", { name: "Copy the setting's key, timezone" }),
  ).toBeTruthy();
  const cells = within(zone).getAllByRole("cell");
  expect(cells.map((c) => c.textContent)).toEqual(["Europe/Berlin", "Changed", ""]);
  expect(
    within(cells[2]!)
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label")),
  ).toEqual(["Reset to default", "Change Time zone"]);
});

// A reader who may not read the document sees the zone the org projection
// carries, and the pencil held with its reason: a write control is never hidden.
test("a reader sees the company's zone and the pencil held with its reason", async () => {
  await show(READER_VIEWER);
  const zone = within(screen.getByRole("table", { name: "Company clock" })).getByRole("row", {
    name: /Time zone/,
  });
  expect(zone.textContent).toContain("Asia/Tokyo");
  const held = within(zone).getByRole("button", { name: "Change Time zone" });
  expect(held.getAttribute("aria-disabled")).toBe("true");
  const described = (held.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain(CONFIG_WRITE_REASONS.no_grant);
});
