import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { LimitsAnswer } from "~/contract/limits.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { AssistantSettings, chainOf, patchAt, toggled } from "./AssistantSettings.tsx";
import { modelEntry } from "./models/fixtures.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const denied = { harness: "claude-code", overrides: { web_tools: "denied" } };
const DOC = {
  name: "Acme",
  providers: {
    llm: {
      "ask-sonnet": { ...denied, model: "sonnet" },
      "ask-haiku": { ...denied, model: "haiku" },
      "ask-opus": { ...denied, model: "opus" },
      coder: { harness: "claude-code", model: "sonnet" },
    },
  },
  assistant: { enabled: true, llm: ["ask-sonnet", "ask-haiku"] },
};

const LIMITS: LimitsAnswer = {
  limits: [
    {
      key: "assistant.max_steps",
      group: "assistant",
      unit: "count",
      value: 30,
      default: 30,
      off: false,
      default_off: false,
      settable: true,
      range: { min: 1, max: 40, or_zero: true },
    },
  ],
};

const ENTRIES = {
  reading: "admin",
  node: "node-1",
  nodes: [{ id: "node-1", reported: true }],
  entries: ["ask-sonnet", "ask-haiku", "ask-opus", "coder"].map((key) => modelEntry({ key })),
  alarms: [],
};

async function show() {
  mountSettings(
    <AssistantSettings />,
    { config: DOC, limits: LIMITS, llm_entries: ENTRIES },
    { hash: "#/settings/ask-crewlet" },
  );
  await act(afterMicrotasks);
}

function engine() {
  return stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
}

async function save(form: HTMLElement) {
  await act(async () => {
    fireEvent.submit(form.closest("form")!);
  });
  await act(afterMicrotasks);
}

test("the page draws the assistant block as the document writes it", async () => {
  await show();
  expect(within(screen.getByRole("row", { name: /Turned on/ })).getByText("On")).toBeTruthy();
  expect(
    within(screen.getByRole("row", { name: /^Models/ })).getByText("ask-sonnet, then ask-haiku"),
  ).toBeTruthy();
  expect(screen.getByRole("row", { name: /Model rounds per turn/ })).toBeTruthy();
  expect(
    within(screen.getByRole("row", { name: /Turned off for everybody/ })).getByText("None"),
  ).toBeTruthy();
  expect(screen.getByText(/can reach any web host/)).toBeTruthy();
});

test("a model added to the chain is saved as the chain, behind the models already in it", async () => {
  const calls = engine();
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Change Models/ }));
  });
  // Only models that deny web tools are offered; the coding model is not.
  expect(screen.queryByRole("checkbox", { name: /coder/ })).toBeNull();
  const opus = screen.getByRole("checkbox", { name: /ask-opus/ });
  fireEvent.click(opus);
  await save(opus);
  const write = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"))!;
  expect(write.body).toMatchObject({ assistant: { llm: ["ask-sonnet", "ask-haiku", "ask-opus"] } });
});

test("a tool family turned off is saved as the deny list", async () => {
  const calls = engine();
  await show();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Change Turned off for everybody/ }));
  });
  const writes = screen.getByRole("checkbox", { name: /Every change/ });
  fireEvent.click(writes);
  await save(writes);
  const write = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"))!;
  expect(write.body).toMatchObject({ assistant: { tools: { deny: ["writes"] } } });
});

test("a chain is read, toggled and written as the document's merge patch", () => {
  expect(chainOf("a")).toEqual(["a"]);
  expect(chainOf(["a", 2, "b"])).toEqual(["a", "b"]);
  expect(toggled(["a", "b"], "c", true)).toEqual(["a", "b", "c"]);
  expect(toggled(["a", "b"], "a", false)).toEqual(["b"]);
  expect(patchAt("assistant.deliberate.llm", null)).toEqual({
    assistant: { deliberate: { llm: null } },
  });
});
