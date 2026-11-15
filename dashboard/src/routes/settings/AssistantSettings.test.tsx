import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { AppAnnouncer } from "~/app/announcer.tsx";
import type { LimitsAnswer } from "~/contract/limits.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import {
  AssistantSettings,
  chainOf,
  hostProblem,
  hostsOf,
  patchAt,
  toggled,
} from "./AssistantSettings.tsx";
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
  assistant: {
    enabled: true,
    llm: ["ask-sonnet", "ask-haiku"],
    research: { hosts: ["docs.example.com"] },
  },
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
    <>
      <AppAnnouncer />
      <AssistantSettings />
    </>,
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
  expect(
    within(screen.getByRole("row", { name: /Hosts it may read/ })).getByText("docs.example.com"),
  ).toBeTruthy();
  expect(screen.getByText(/Quick answers read only the hosts under Web research/)).toBeTruthy();
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

async function openHosts() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Change Hosts it may read/ }));
  });
  return screen.getByRole("textbox", { name: "New host" });
}

test("a host typed and not yet added is saved with the list, in lower case", async () => {
  const calls = engine();
  await show();
  const box = await openHosts();
  fireEvent.change(box, { target: { value: "API.example.com" } });
  await save(box);
  const write = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"))!;
  expect(write.body).toMatchObject({
    assistant: { research: { hosts: ["docs.example.com", "api.example.com"] } },
  });
});

test("removing every host removes the research block, which turns web research off", async () => {
  const calls = engine();
  await show();
  const box = await openHosts();
  fireEvent.click(screen.getByRole("button", { name: /Remove host/ }));
  await save(box);
  const write = calls.find((c) => c.method === "PATCH" && !c.path.includes("dry_run"))!;
  expect(write.body).toMatchObject({ assistant: { research: null } });
});

test("an address in place of a host is refused before anything is sent", async () => {
  const calls = engine();
  await show();
  const box = await openHosts();
  fireEvent.change(box, { target: { value: "https://docs.example.com/en" } });
  await save(box);
  expect(calls.filter((c) => c.method === "PATCH")).toEqual([]);
  expect(
    screen.getAllByText(/https:\/\/docs\.example\.com\/en is not a host name/).length,
  ).toBeGreaterThan(0);
});

test("a host is checked as the engine checks it", () => {
  const ok = ["docs.example.com", "api.example.com"];
  expect(ok.map((_, i) => hostProblem(ok, i))).toEqual([null, null]);
  for (const bad of [
    "https://docs.example.com",
    "docs.example.com:443",
    "*.example.com",
    "localhost",
  ]) {
    expect(hostProblem([bad], 0)).toBe(
      `${bad} is not a host name. Write the host alone, with no scheme, port, path or wildcard, such as docs.example.com.`,
    );
  }
  expect(hostProblem(["docs.example.com", "docs.example.com"], 1)).toMatch(/listed twice/);
  expect(hostsOf({ hosts: [" Docs.Example.com "], draft: "" })).toEqual(["docs.example.com"]);
});
