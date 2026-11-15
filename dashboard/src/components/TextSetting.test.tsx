/**
 * What a setting that is not a number WRITES, kind by kind: each is the
 * engine's row, changed in place through the one check and save
 * (`lib/useSettingWrite.ts`), as a merge patch of its one key; the default is
 * the key removed, a setting's own Off is a value written, and the engine's
 * refusals and warnings are said in its words before anything is stored.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { SettingsTable } from "@crewlethq/ui";
import { AppAnnouncer } from "~/app/announcer.tsx";
import type { SettingRow } from "~/contract/settings.ts";
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
  type EngineCall,
} from "~/test/settingsScreen.tsx";
import { TextSetting } from "./TextSetting.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

function row(over: Partial<SettingRow> & Pick<SettingRow, "key" | "kind">): SettingRow {
  return {
    group: "knowledge",
    value: "",
    default: "",
    set: false,
    off: false,
    has_off: false,
    ...over,
  };
}

const SKILLS = row({
  key: "knowledge.skills_container",
  kind: "container",
  value: "TS",
  default: "TS",
  has_off: true,
});
const SCOPE = row({ key: "knowledge.scope", kind: "containers", value: [], default: [] });
const VECTORS = row({ key: "knowledge.vectors", kind: "switch", value: true, default: true });
const ZONE = row({ key: "timezone", group: "company", kind: "zone", value: "UTC", default: "UTC" });

async function show(
  setting: SettingRow,
  label: string,
  props: Partial<Parameters<typeof TextSetting>[0]> = {},
  viewer?: Record<string, unknown>,
) {
  // THE FRAME'S LIVE REGION, as every screen has it: a list field says what
  // it added aloud.
  const mounted = mountSettings(
    <>
      <AppAnnouncer />
      <SettingsTable aria-label="Settings">
        <TextSetting
          row={setting}
          label={label}
          area="Doclet"
          reread="Reload."
          onApplied={() => {}}
          helper="Helper."
          {...props}
        />
      </SettingsTable>
    </>,
    {},
    { viewer },
  );
  await act(afterMicrotasks);
  return mounted;
}

/** An engine holding `doc`, which checks every change valid (with `warnings`) and saves it. */
function engine(doc: CompanyDocument, warnings: unknown[] = []): EngineCall[] {
  return stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(doc, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) {
      // THE BASELINE CHECK (the empty patch) carries none of the change's warnings.
      const empty = JSON.stringify(call.body) === "{}";
      return validAnswer(empty ? [] : warnings);
    }
    return savedAnswer();
  });
}

const saved = (calls: EngineCall[]) =>
  calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"));

async function press(name: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

async function submit(from: HTMLElement) {
  await act(async () => {
    fireEvent.submit(from.closest("form")!);
  });
  await act(afterMicrotasks);
}

const DOC = { name: "Acme" } as unknown as CompanyDocument;

// A SETTING'S OWN OFF IS A VALUE WRITTEN, never the default: turning tool
// skills off writes the empty key, and the default removes it. Mutation: write
// null for Off, or "" for the default, and this fails.
test("a space with an Off of its own writes the empty key for Off, and removes it for the default", async () => {
  const calls = engine(DOC);
  await show(SKILLS, "Tool skills");
  const drawn = screen.getByRole("row", { name: /Tool skills/ });
  expect(within(drawn).getByText("TS")).toBeTruthy();
  expect(within(drawn).getByText("Default")).toBeTruthy();
  await press("Change Tool skills");
  fireEvent.click(screen.getByRole("radio", { name: "Off" }));
  await submit(screen.getByRole("radio", { name: "Off" }));
  expect(saved(calls)!.body).toEqual({
    knowledge: { skills_container: "" },
    _summary: "Doclet: Tool skills set to Off (knowledge.skills_container)",
  });
});

test("a space typed for a setting with an Off is written trimmed", async () => {
  const calls = engine(DOC);
  await show({ ...SKILLS, value: "", off: true, set: true }, "Tool skills");
  expect(within(screen.getByRole("row", { name: /Tool skills/ })).getByText("Off")).toBeTruthy();
  await press("Change Tool skills");
  fireEvent.click(screen.getByRole("radio", { name: "Another space" }));
  const key = screen.getByRole("textbox", { name: "Tool skills: space key" });
  fireEvent.change(key, { target: { value: " SKL " } });
  await submit(key);
  expect(saved(calls)!.body).toMatchObject({ knowledge: { skills_container: "SKL" } });
});

// AN EMPTY REACH IS EVERY SPACE, the default: the list is the whole value
// (a merge patch replaces a list), and emptied it removes the key.
test("a list of spaces is written whole, and an empty one is the default", async () => {
  const calls = engine(DOC);
  await show(SCOPE, "Search reaches", {
    words: (v) => (Array.isArray(v) && v.length > 0 ? v.join(", ") : "Every space"),
    choices: ["ENG", "PROD"],
  });
  expect(
    within(screen.getByRole("row", { name: /Search reaches/ })).getByText("Every space"),
  ).toBeTruthy();
  await press("Change Search reaches");
  const field = screen.getByRole("combobox");
  fireEvent.change(field, { target: { value: "ENG" } });
  fireEvent.keyDown(field, { key: "Enter" });
  fireEvent.change(field, { target: { value: "OPS" } });
  fireEvent.keyDown(field, { key: "Enter" });
  await submit(field);
  expect(saved(calls)!.body).toEqual({
    knowledge: { scope: ["ENG", "OPS"] },
    _summary: "Doclet: Search reaches set to ENG, OPS (knowledge.scope)",
  });
});

// A SWITCH HAS THREE SETTINGS: the default the engine derives (said with its
// value), On and Off. Mutation: write false for the default and a company
// that adds an embeddings provider later never gets search by meaning.
test("a switch writes On and Off, and removes the key for the default it derives", async () => {
  const calls = engine({ ...DOC, knowledge: { vectors: false } } as unknown as CompanyDocument);
  await show({ ...VECTORS, value: false, set: true }, "Search by meaning");
  const drawn = screen.getByRole("row", { name: /Search by meaning/ });
  expect(within(drawn).getByText("Off")).toBeTruthy();
  // The source says provenance alone; the editor names the default.
  expect(within(drawn).getByText("Changed")).toBeTruthy();
  await press("Change Search by meaning");
  const back = screen.getByRole("radio", { name: "Default (On)" });
  fireEvent.click(back);
  await submit(back);
  expect(saved(calls)!.body).toEqual({
    knowledge: { vectors: null },
    _summary: "Doclet: Search by meaning back to the default (knowledge.vectors)",
  });
});

// MOVING THE CLOCK IS CONFIRMED: the engine's check warns of it in its own
// words, the save waits for a second press, and only then is it stored.
// Mutation: save on a warned check, and this fails.
test("a zone the engine warns about is saved only after Save anyway", async () => {
  const warning =
    "This moves the company's clock from UTC to Europe/Berlin. Every budget's day, week and month will start at midnight in Europe/Berlin.";
  const calls = engine(DOC, [
    {
      kind: "advisory",
      ref: "",
      path: "timezone",
      segments: ["timezone"],
      seat: "",
      unit: "",
      from: "",
      to: "",
      message: warning,
      introduced: true,
    },
  ]);
  await show(ZONE, "Time zone");
  await press("Change Time zone");
  // A SEARCHABLE SELECT IS OPENED BY ITS BUTTON; its search field is the combobox.
  fireEvent.click(screen.getByRole("button", { name: "Time zone" }));
  fireEvent.change(await screen.findByRole("combobox"), { target: { value: "Berlin" } });
  fireEvent.mouseDown(await screen.findByRole("option", { name: "Europe/Berlin" }));
  await submit(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByText(warning)).toBeTruthy();
  expect(saved(calls)).toBeUndefined();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
  });
  await act(afterMicrotasks);
  expect(saved(calls)!.body).toEqual({
    timezone: "Europe/Berlin",
    _summary: "Doclet: Time zone set to Europe/Berlin (timezone)",
  });
});

// WHAT IS ALREADY WRITTEN IS NOT SENT: Save is held on it, with the reason.
test("Save is held on the value the document already writes", async () => {
  const calls = engine(DOC);
  await show({ ...SKILLS, value: "SKL", set: true }, "Tool skills");
  await press("Change Tool skills");
  const save = screen.getByRole("button", { name: "Save" });
  expect(save.getAttribute("title")).toBe("This is the tool skills already.");
  await submit(save);
  expect(calls).toEqual([]);
});

// The held reason is the kit tooltip's, so keyboard focus shows it, not hover alone.
test("a reader without the grant sees the pencil held, and its reason on keyboard focus", async () => {
  await show(SKILLS, "Tool skills", {}, READER_VIEWER);
  const pencil = screen.getByRole("button", { name: "Change Tool skills" });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  // Focus as Tab gives it: the key first, so the focus is the keyboard's.
  fireEvent.keyDown(document.body, { key: "Tab" });
  act(() => pencil.focus());
  expect(screen.getByRole("tooltip").textContent).toBe(
    `Change Tool skills. ${CONFIG_WRITE_REASONS.no_grant}`,
  );
});
