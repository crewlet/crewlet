/**
 * A model's test says each check in words, names a failure by the credential
 * the model actually signs in with, and refreshes the page behind it once the
 * engine has answered.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { CheckAnswer } from "~/protocol/models.ts";
import { apiKeyEntry, modelsData } from "~/routes/settings/models/fixtures.ts";
import { TestDialog } from "./TestDialog.tsx";

const checkModel = vi.hoisted(() => vi.fn());
vi.mock("../api.ts", () => ({ checkModel }));

afterEach(() => {
  cleanup();
  checkModel.mockReset();
});

const FAILED: CheckAnswer = {
  at: "2026-10-29T10:00:00Z",
  node: "node-1",
  ok: false,
  failure: "auth",
  lines: [
    { check: "binary", ok: true, detail: "/usr/local/bin/claude" },
    {
      check: "sign-in",
      ok: false,
      kind: "auth",
      detail: "401 invalid x-api-key",
      remedy: "Store a new key.",
    },
  ],
};

function open(route: "api-key" | "token") {
  const entry = apiKeyEntry({ key: "sonnet" });
  entry.sign_in = { ...entry.sign_in, route, mode: route === "token" ? "subscription" : "api-key" };
  const model = modelsData({ entries: [entry] }).models[0]!;
  const onDone = vi.fn();
  render(<TestDialog model={model} onClose={() => {}} onDone={onDone} />);
  return onDone;
}

test("a test runs a real call, says each check by its name, and refreshes the page once answered", async () => {
  checkModel.mockResolvedValue({ kind: "ok", status: 200, value: FAILED });
  const onDone = open("api-key");
  expect(checkModel).toHaveBeenCalledWith("sonnet", { smoke: true }, expect.any(AbortSignal));
  const lines = await screen.findByRole("list", { name: "Test results" });
  const items = within(lines).getAllByRole("listitem");
  expect(items[0]!.textContent).toContain("CLI installed");
  expect(items[1]!.textContent).toContain("Signs in");
  expect(items[1]!.textContent).toContain("The API key was refused. Store a new key.");
  expect(onDone).toHaveBeenCalledTimes(1);
});

// QA Minor 19: a key is a key and a token a token.
test("a refused token is called a token, never a key", async () => {
  checkModel.mockResolvedValue({ kind: "ok", status: 200, value: FAILED });
  open("token");
  expect(await screen.findAllByText(/The subscription token was refused/)).not.toHaveLength(0);
  expect(screen.queryByText(/API key/)).toBeNull();
});

test("a refused test says the engine's sentence and changes nothing behind it", async () => {
  checkModel.mockResolvedValue({
    kind: "refused",
    status: 404,
    code: "not_found",
    sentence: "There is no model called sonnet.",
    detail: "",
    hint: "",
    body: {},
  });
  const onDone = open("api-key");
  expect(await screen.findByText("There is no model called sonnet.")).toBeDefined();
  expect(onDone).not.toHaveBeenCalled();
});
