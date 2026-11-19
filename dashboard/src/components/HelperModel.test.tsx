/**
 * A helper's one setting, its model: offered to an administrator as a choice
 * among the company's models, each one the engine says cannot run a helper
 * listed disabled with its reason, and written as `<helper>.model`.
 */

import type { ReactNode } from "react";
import { cleanup, fireEvent, render as draw, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { HelperModel, helperModelPlan, type HelperChoice } from "./HelperModel.tsx";

afterEach(cleanup);

function render(node: ReactNode) {
  const store = new Store();
  return draw(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      {node}
    </ClientContext.Provider>,
  );
}

const doc = (assistant?: unknown, advisor?: unknown) =>
  ({ ...(assistant ? { assistant } : {}), ...(advisor ? { advisor } : {}) }) as CompanyDocument;

test("a choice writes the helper's one model key, and None removes it", () => {
  const ask = helperModelPlan("assistant", "sonnet");
  expect(ask(doc())).toEqual({
    patch: { assistant: { model: "sonnet" } },
    summary: "Ask Crewlet: model set to sonnet (assistant.model)",
  });
  expect(ask(doc({ model: "sonnet" }))).toBeNull();
  expect(helperModelPlan("advisor", null)(doc(undefined, { model: "opus" }))).toEqual({
    patch: { advisor: { model: null } },
    summary: "Advisor: model removed (advisor.model)",
  });
  expect(helperModelPlan("advisor", null)(doc())).toBeNull();
});

const CHOICES: HelperChoice[] = [
  { key: "sonnet", model: "claude-sonnet-5-5", problem: null },
  {
    key: "codex",
    model: "gpt-6",
    problem: "This model's tool runs with tools of its own that Crewlet cannot turn off.",
  },
];

test("a reader who may not choose reads the model's name, with no control", () => {
  render(
    <HelperModel
      helper="assistant"
      current="sonnet"
      choices={CHOICES}
      canAssign={false}
      onApplied={() => {}}
    />,
  );
  expect(screen.getByText("Runs on sonnet")).toBeDefined();
  expect(screen.queryByRole("combobox")).toBeNull();
});

test("an administrator chooses among the company's models, and None", () => {
  render(
    <HelperModel
      helper="assistant"
      current={null}
      choices={CHOICES}
      canAssign
      onApplied={() => {}}
    />,
  );
  const picker = screen.getByRole("combobox", { name: "Ask Crewlet's model" });
  fireEvent.click(picker);
  const refused = screen.getByRole("option", { name: /codex/ });
  expect(refused.getAttribute("aria-disabled")).toBe("true");
  expect(refused.textContent).toContain("cannot turn off");
  expect(screen.getByRole("option", { name: /sonnet/ }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
  expect(screen.getByRole("option", { name: /None/ })).toBeDefined();
});
