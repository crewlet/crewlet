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
  const field = screen.getByRole("spinbutton");
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

const ROUNDS: LimitRow = {
  key: "turn_engine.max_iterations",
  group: "turn",
  unit: "count",
  value: 7,
  default: 3,
  off: false,
  default_off: false,
  settable: true,
  range: { min: 1, max: 10 },
};

const IDLE: LimitRow = {
  key: "sign_in.idle_timeout_seconds",
  group: "sign_in",
  unit: "seconds",
  value: 43_200,
  default: 43_200,
  off: false,
  default_off: false,
  settable: true,
  range: { min: 300, max: 2_592_000 },
};

async function mountCap(cap: LimitRow, label: string) {
  mountSettings(
    <SettingsTable aria-label="Limits">
      <LimitSetting cap={cap} label={label} area="Limits" reread="Reload." onApplied={() => {}} />
    </SettingsTable>,
    {},
  );
  await act(afterMicrotasks);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: `Change ${label}` }));
  });
}

const DOC = { name: "Acme" } as unknown as CompanyDocument;

function engine(doc: CompanyDocument = DOC) {
  return stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(doc, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
}

// A NUMBER NEEDS NO READING: its steppers hold to the range said under it, and
// its default is the placeholder and a chip, never a sentence. Mutation: drop
// the range from the field and Increase passes 10.
test("a count is stepped within its range, its default a placeholder and a chip", async () => {
  await mountCap(ROUNDS, "Review rounds per turn");
  const field = screen.getByRole("spinbutton");
  expect(field.getAttribute("aria-valuemin")).toBe("1");
  expect(field.getAttribute("aria-valuemax")).toBe("10");
  expect((field as HTMLInputElement).value).toBe("7");
  expect(field.getAttribute("placeholder")).toBe("3");
  expect(screen.getByText("1 to 10")).toBeTruthy();
  expect(screen.queryByText(/Empty is the default/)).toBeNull();
  for (let i = 0; i < 5; i++) fireEvent.click(screen.getByRole("button", { name: "Increase" }));
  expect((field as HTMLInputElement).value).toBe("10");
  fireEvent.click(screen.getByRole("radio", { name: "Default (3)" }));
  expect((field as HTMLInputElement).value).toBe("");
});

// A DURATION IS AN AMOUNT AND A UNIT, with common durations beside its
// default, and a chip fills both. Mutation: save the amount without its
// unit's seconds and the patch is wrong.
test("a duration is an amount and a unit, and a common duration fills both", async () => {
  const calls = engine();
  await mountCap(IDLE, "Signed out when unused for");
  expect(screen.getByRole("combobox", { name: "Unit" }).textContent).toContain("hours");
  expect(screen.getByRole("spinbutton").getAttribute("placeholder")).toBe("12");
  expect(screen.getByText("5 minutes to 30 days")).toBeTruthy();
  const chips = within(screen.getByRole("radiogroup", { name: "Common durations" }))
    .getAllByRole("radio")
    .map((r) => r.textContent);
  expect(chips[0]).toBe("Default (12 hours)");
  expect(chips).toContain("1 day");
  fireEvent.click(screen.getByRole("radio", { name: "1 day" }));
  expect((screen.getByRole("spinbutton") as HTMLInputElement).value).toBe("1");
  await act(async () => {
    fireEvent.submit(screen.getByRole("spinbutton").closest("form")!);
  });
  await act(afterMicrotasks);
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save!.body).toEqual({
    sign_in: { idle_timeout_seconds: 86_400 },
    _summary: "Limits: Signed out when unused for set to 1 day (sign_in.idle_timeout_seconds)",
  });
});

// THE RESET STANDS AT THE ROW'S END while the value differs from its default,
// asks inline, and saves the default by removing the key, on the same checked
// path every save takes. Mutation: reset without asking, or write the default's
// number, and this fails.
test("a changed value is reset to its default after an inline question, by removing its key", async () => {
  const calls = engine({
    name: "Acme",
    turn_engine: { max_iterations: 7 },
  } as unknown as CompanyDocument);
  mountSettings(
    <SettingsTable aria-label="Limits">
      <LimitSetting
        cap={ROUNDS}
        label="Review rounds per turn"
        area="Limits"
        reread="Reload."
        onApplied={() => {}}
      />
    </SettingsTable>,
    {},
  );
  await act(afterMicrotasks);
  const row = screen.getByRole("row", { name: /Review rounds per turn/ });
  const cells = within(row).getAllByRole("cell");
  expect(
    within(cells[cells.length - 1]!)
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label")),
  ).toEqual(["Reset to default", "Change Review rounds per turn"]);
  fireEvent.click(within(row).getByRole("button", { name: "Reset to default" }));
  expect(row.textContent).toContain("Reset to 3?");
  expect(calls).toEqual([]);
  await act(async () => {
    fireEvent.click(within(row).getByRole("button", { name: "Reset" }));
  });
  await act(afterMicrotasks);
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save!.body).toEqual({
    turn_engine: { max_iterations: null },
    _summary: "Limits: Review rounds per turn back to the default (turn_engine.max_iterations)",
  });
  // THE DEFAULT, MARKED AS APPLYING, and no reset while it is the default.
  expect(within(row).getByText("3")).toBeTruthy();
  expect(within(row).queryByRole("button", { name: "Reset to default" })).toBeNull();
});
