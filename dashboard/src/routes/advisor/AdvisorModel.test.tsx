/**
 * The Advisor's model picker says what it picks; a reader who may not choose
 * reads the model in a sentence, with no caption.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

const helper = vi.hoisted(() => ({ canAssign: true }));

vi.mock("~/components/HelperModel.tsx", async (actual) => ({
  ...(await actual<typeof import("~/components/HelperModel.tsx")>()),
  useHelperModel: () => ({
    current: null,
    choices: [{ key: "fast", model: "small-model", problem: null }],
    canAssign: helper.canAssign,
    reload: () => {},
  }),
}));

const { AdvisorModel } = await import("./AdvisorModel.tsx");

afterEach(cleanup);

function mount() {
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <AdvisorModel />
    </ClientContext.Provider>,
  );
}

test("an administrator's picker is captioned with what it picks", () => {
  helper.canAssign = true;
  mount();
  expect(screen.getByText("Explains with")).toBeDefined();
  expect(screen.getByRole("combobox", { name: "Advisor's model" })).toBeDefined();
});

test("a reader who may not choose reads the model in a sentence, uncaptioned", () => {
  helper.canAssign = false;
  mount();
  expect(screen.getByText("No model chosen")).toBeDefined();
  expect(screen.queryByText("Explains with")).toBeNull();
});
