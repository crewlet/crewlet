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
import type { SelectOption } from "@crewlethq/ui";
import { HelperModel, helperModelPlan } from "./HelperModel.tsx";

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

test("a fleet is written as its reference, as a model's key is", () => {
  expect(helperModelPlan("advisor", "fleet:claude-opus")(doc())).toEqual({
    patch: { advisor: { model: "fleet:claude-opus" } },
    summary: "Advisor: model set to fleet:claude-opus (advisor.model)",
  });
});

const OPTIONS: SelectOption[] = [
  { value: "fleet:claude-opus", label: "Fleet · Claude Opus", group: "Fleets" },
  { value: "sonnet", label: "sonnet", group: "Models" },
  {
    value: "codex",
    label: "codex",
    group: "Models",
    disabled: true,
    description: "This model's tool runs with tools of its own that Crewlet cannot turn off.",
  },
];

test("a reader who may not choose reads the value in a picker that cannot open", () => {
  render(
    <HelperModel
      helper="assistant"
      current="sonnet"
      options={OPTIONS}
      wordOf={(v) => v}
      canAssign={false}
      onApplied={() => {}}
    />,
  );
  const picker = screen.getByRole("combobox", { name: "Ask Crewlet's model" });
  expect(picker.textContent).toContain("sonnet");
  expect(picker.getAttribute("aria-disabled") === "true" || picker.hasAttribute("disabled")).toBe(
    true,
  );
});

test("an administrator chooses among the page's fleets and models, and None", () => {
  render(
    <HelperModel
      helper="assistant"
      current={null}
      options={OPTIONS}
      wordOf={(v) => v}
      canAssign
      onApplied={() => {}}
    />,
  );
  fireEvent.click(screen.getByRole("combobox", { name: "Ask Crewlet's model" }));
  const refused = screen.getByRole("option", { name: /codex/ });
  expect(refused.getAttribute("aria-disabled")).toBe("true");
  expect(refused.textContent).toContain("cannot turn off");
  expect(screen.getByRole("option", { name: /Fleet · Claude Opus/ })).toBeDefined();
  expect(screen.getByRole("option", { name: /None/ })).toBeDefined();
});

test("a pick asks once in the move's words, saying what it ran on", () => {
  const pending: (string | null | undefined)[] = [];
  render(
    <HelperModel
      helper="advisor"
      current="sonnet"
      options={OPTIONS}
      wordOf={(v) => (v.startsWith("fleet:") ? "Fleet · Claude Opus" : v)}
      canAssign
      onApplied={() => {}}
      onPending={(v) => pending.push(v)}
    />,
  );
  fireEvent.click(screen.getByRole("combobox", { name: "Advisor's model" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: /Fleet · Claude Opus/ }));
  const dialog = screen.getByRole("dialog", { name: /Move Advisor to Fleet · Claude Opus/ });
  expect(dialog.textContent).toContain(
    "It moves at its next turn; a running turn finishes where it is.",
  );
  expect(dialog.textContent).toContain("sonnet");
  expect(pending).toEqual(["fleet:claude-opus"]);
});
