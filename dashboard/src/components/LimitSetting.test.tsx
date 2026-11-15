/**
 * What the one number editor writes for a setting whose zero is a choice of
 * its own: Chatlet's message horizon, 0 to keep messages for ever or 30 to
 * 3650 days. The hole between is said before anything is sent, and a saved 0
 * reads as the page calls it while it applies.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { SettingsTable } from "@crewlethq/ui";
import type { LimitRow } from "~/lib/limits.ts";
import type { CompanyDocument } from "~/protocol/index.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { LimitSetting } from "./LimitSetting.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const HORIZON: LimitRow = {
  key: "chat.chatlet.message_retention_days",
  group: "chat",
  unit: "days",
  value: 365,
  default: 365,
  off: false,
  default_off: false,
  settable: true,
  range: { min: 30, max: 3650, or_zero: true },
};

async function show() {
  mountSettings(
    <SettingsTable aria-label="Chatlet settings">
      <LimitSetting
        cap={HORIZON}
        label="Messages are kept for"
        area="Chatlet"
        reread="Reload."
        onApplied={() => {}}
        off="For ever"
        zero="0 to keep them for ever"
      />
    </SettingsTable>,
    {},
  );
  await act(afterMicrotasks);
}

// THE HOLE IS SAID BEFORE ANY REQUEST, and 0 is admitted and saved as 0, read
// For ever while it applies. Mutation: drop the zero carve-out and 0 is
// refused; drop `off` and a saved 0 reads "Off", which says nothing of what
// it keeps.
test("a horizon in the hole is refused beside the field, and 0 keeps messages for ever", async () => {
  const doc = { name: "Acme" } as unknown as CompanyDocument;
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(doc, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change Messages are kept for" }));
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value: "12" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  expect(calls).toEqual([]);
  expect(
    screen.getAllByText("From 30 days to 3,650 days, or 0 to keep them for ever.").length,
  ).toBeGreaterThan(0);

  fireEvent.change(field, { target: { value: "0" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save!.body).toEqual({
    chat: { chatlet: { message_retention_days: 0 } },
    _summary: "Chatlet: Messages are kept for set to 0 (chat.chatlet.message_retention_days)",
  });
  const row = screen.getByRole("row", { name: /Messages are kept for/ });
  expect(within(row).getByText("For ever")).toBeTruthy();
});
