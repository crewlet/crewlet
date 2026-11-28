/**
 * A helper's model, chosen: the picker shows the chosen model at once, says it
 * is being saved, then tells a screen reader where it runs now, and falls back to the
 * stored model where nothing was saved. The write itself is the configuration
 * transport's, held here so each moment can be read.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ToastProvider, type SelectOption } from "@crewlethq/ui";
import { toastText } from "~/testing.tsx";
import { HelperModel } from "./HelperModel.tsx";

const transport = vi.hoisted(() => ({
  save: null as null | ((outcome: unknown) => void),
  refuse: false,
}));

vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: async () => ({
    kind: "document",
    document: { assistant: { model: "sonnet" } },
    etag: '"r1"',
  }),
  dryRunPatch: async () =>
    transport.refuse
      ? { kind: "problems", problems: [{ path: "assistant.model", message: "no such model" }] }
      : { kind: "valid", baseRevisionId: "r1", warnings: [] },
  savePatch: () =>
    new Promise((resolve) => {
      transport.save = resolve;
    }),
}));

const OPTIONS: SelectOption[] = [
  { value: "sonnet", label: "sonnet", group: "Models" },
  { value: "fast", label: "fast", group: "Models" },
];

beforeEach(() => {
  transport.save = null;
  transport.refuse = false;
});
afterEach(cleanup);

function mount() {
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <ToastProvider>
        <HelperModel
          helper="assistant"
          current="sonnet"
          options={OPTIONS}
          wordOf={(v) => v}
          canAssign
          onApplied={() => {}}
        />
      </ToastProvider>
    </ClientContext.Provider>,
  );
}

/** Chooses a model and confirms it: every model picker on its page asks once. */
async function choose(key: string) {
  fireEvent.click(screen.getByRole("combobox", { name: "Ask Crewlet's model" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: new RegExp(key) }));
  const confirm = screen.getByRole("dialog", { name: new RegExp(`Move Ask Crewlet to ${key}`) });
  await act(async () => {
    fireEvent.click(within(confirm).getByRole("button", { name: "Move" }));
  });
}

test("the chosen model shows at once while it is saved, and its toast says where it runs now", async () => {
  mount();
  await choose("fast");
  // SAVING: the choice is drawn, never the old model, and the wait is said.
  expect(screen.getByRole("combobox", { name: "Ask Crewlet's model" }).textContent).toContain(
    "fast",
  );
  expect(await screen.findByText("Saving…")).toBeDefined();
  await act(async () => {
    transport.save?.({ kind: "saved", revisionId: "r2", epoch: 2 });
  });
  expect(toastText()).toContain("Ask Crewlet now runs on fast");
  expect(screen.getByRole("combobox", { name: "Ask Crewlet's model" }).textContent).toContain(
    "fast",
  );
});

test("a choice the engine refuses falls back to the stored model", async () => {
  transport.refuse = true;
  mount();
  await choose("fast");
  expect(await screen.findByText("Nothing was saved")).toBeDefined();
  expect(screen.getByRole("combobox", { name: "Ask Crewlet's model" }).textContent).toContain(
    "sonnet",
  );
  expect(toastText()).not.toContain("now runs on");
});
