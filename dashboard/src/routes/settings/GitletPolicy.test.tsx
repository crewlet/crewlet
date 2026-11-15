/**
 * What Settings › Gitlet's code policy card WRITES: each scratch setting is
 * the engine's own row, changed in place by the field Settings › Limits
 * changes a cap with, checked by the engine first and saved as a patch of
 * its one key; and the identity domain is the engine's row of it
 * (`settings`), changed in place by its text field on the same path.
 *
 * Its own file because `Gitlet.test.tsx` renders with a socket of its own,
 * and this suite answers the socket as the settings frame does.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { LimitsAnswer } from "~/contract/limits.ts";
import type { SettingsAnswer } from "~/contract/settings.ts";
import type { CompanyDocument } from "~/protocol/index.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { CodePolicy } from "./Gitlet.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const DOC = { name: "Acme", code: { backend: "gitlet" } } as unknown as CompanyDocument;

const LIMITS: LimitsAnswer = {
  limits: [
    {
      key: "code.scratch.idle_days",
      group: "code",
      unit: "days",
      value: 7,
      default: 7,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 1, max: 365 },
    },
    {
      key: "code.scratch.max_per_seat",
      group: "code",
      unit: "count",
      value: 20,
      default: 50,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 0, max: 1000 },
    },
    // ANOTHER PAGE'S ROW, which this card never draws.
    {
      key: "turn_engine.max_iterations",
      group: "turn",
      unit: "count",
      value: 3,
      default: 3,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 1 },
    },
  ],
};

const SETTINGS: SettingsAnswer = {
  settings: [
    {
      key: "code.identity_domain",
      group: "code",
      kind: "text",
      value: "seats.invalid",
      default: "seats.invalid",
      set: false,
      off: false,
      has_off: false,
    },
    // ANOTHER PAGE'S ROW, which this card never draws.
    {
      key: "timezone",
      group: "company",
      kind: "zone",
      value: "UTC",
      default: "UTC",
      set: false,
      off: false,
      has_off: false,
    },
  ],
};

async function show(viewer?: Record<string, unknown>, settings: SettingsAnswer = SETTINGS) {
  const mounted = mountSettings(
    <CodePolicy />,
    { limits: LIMITS, settings },
    {
      hash: "#/settings/gitlet",
      viewer,
    },
  );
  await act(afterMicrotasks);
  return mounted;
}

test("the scratch policy is the engine's rows, each beside its default, and nothing else", async () => {
  await show();
  const table = screen.getByRole("table", { name: "Scratch repositories and commit identity" });
  const idle = within(table).getByRole("row", {
    name: /Idle scratch repositories are deleted after/,
  });
  expect(within(idle).getByText("7 days")).toBeTruthy();
  expect(within(idle).getByText("Default")).toBeTruthy();
  const most = within(table).getByRole("row", { name: /Scratch repositories a seat may hold/ });
  expect(within(most).getByText("Changed from 50")).toBeTruthy();
  // THE IDENTITY DOMAIN IS THE ENGINE'S ROW: its default, said as one, and no
  // other page's setting beside it.
  const domain = within(table).getByRole("row", { name: /Identity domain/ });
  expect(within(domain).getByText("seats.invalid")).toBeTruthy();
  expect(within(domain).getByText("Default")).toBeTruthy();
  expect(within(table).queryByRole("row", { name: /Review rounds/ })).toBeNull();
  expect(within(table).queryByRole("row", { name: /Time zone/ })).toBeNull();
});

// THE IDENTITY DOMAIN IS CHANGED IN PLACE, through the same check and save as
// the numbers, as a patch of its one key; and going back to the default
// removes the key. Mutation: send the whole code block, or write the default's
// text rather than removing the key, and this fails.
test("the identity domain is changed in place as a patch of its one key", async () => {
  const written = { ...DOC, code: { backend: "gitlet", identity_domain: "old.example" } };
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(written, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show(undefined, {
    settings: [{ ...SETTINGS.settings[0]!, value: "old.example", set: true }],
  });
  expect(screen.getByText("Changed from seats.invalid")).toBeTruthy();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change Identity domain" }));
  });
  const field = screen.getByRole("textbox");
  expect((field as HTMLInputElement).value).toBe("old.example");
  fireEvent.change(field, { target: { value: " commits.example.com " } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save!.body).toEqual({
    code: { identity_domain: "commits.example.com" },
    _summary: "Gitlet: Identity domain set to commits.example.com (code.identity_domain)",
  });

  // BACK TO THE DEFAULT: an empty field removes the key.
  calls.length = 0;
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change Identity domain" }));
  });
  const again = screen.getByRole("textbox");
  fireEvent.change(again, { target: { value: "" } });
  await act(async () => {
    fireEvent.submit(again.closest("form")!);
  });
  await act(afterMicrotasks);
  const cleared = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(cleared!.body).toEqual({
    code: { identity_domain: null },
    _summary: "Gitlet: Identity domain back to the default (code.identity_domain)",
  });
});

// THE ENGINE'S REFUSAL IS SAID IN ITS OWN WORDS FOR A PERSON, from the check,
// and nothing is saved. Mutation: show the operator's message, or save after a
// refused check, and this fails.
test("a domain the engine refuses is said in the engine's words for a person, and not saved", async () => {
  const said = "That is not a domain name. Use lowercase letters, digits and hyphens.";
  const message = 'code.identity_domain: shape: "Not_A_Domain" is not a lowercase DNS name';
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer(
      {
        error: "invalid_patch",
        detail: message,
        problems: [{ path: "code.identity_domain", segments: null, kind: "shape", message, said }],
      },
      400,
    );
  });
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Change Identity domain" }));
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value: "Not_A_Domain" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
  expect(screen.getByText(said)).toBeTruthy();
  expect(screen.queryByText(message)).toBeNull();
  expect(calls.some((c) => c.method === "PATCH" && !c.path.includes("dry_run"))).toBe(false);
});

test("a scratch setting is changed in place as a patch of its one key", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show();
  await act(async () => {
    fireEvent.click(
      screen.getByRole("button", { name: "Change Idle scratch repositories are deleted after" }),
    );
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value: "14" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
  const save = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));
  expect(save!.headers.get("If-Match")).toBe('"r1"');
  expect(save!.body).toEqual({
    code: { scratch: { idle_days: 14 } },
    _summary:
      "Gitlet: Idle scratch repositories are deleted after set to 14 (code.scratch.idle_days)",
  });
});

test("a value of days that is not a whole number is said beside the field before any request", async () => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show();
  await act(async () => {
    fireEvent.click(
      screen.getByRole("button", { name: "Change Idle scratch repositories are deleted after" }),
    );
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value: "1.5" } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  expect(calls).toEqual([]);
  expect(screen.getAllByText("“1.5” is not a whole number of days.").length).toBeGreaterThan(0);
});

test("a reader without the grant sees the pencils disabled with the reason", async () => {
  await show(READER_VIEWER);
  const pencil = screen.getByRole("button", {
    name: "Change Scratch repositories a seat may hold",
  });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  expect(pencil.getAttribute("title")).toBe(CONFIG_WRITE_REASONS.no_grant);
});
