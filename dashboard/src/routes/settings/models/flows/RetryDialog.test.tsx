/**
 * Retrying a limit acts on every model on the plan, says plainly why nothing
 * was cleared when nothing was (the limit holds, the probe failed for another
 * reason, or there was nothing to retry), and keeps the safe action under the
 * reader's hand.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ModelSittingOut } from "~/protocol/models.ts";
import { apiKeyEntry, modelsData } from "~/routes/settings/models/fixtures.ts";
import { RetryDialog } from "./RetryDialog.tsx";

const retryModel = vi.hoisted(() => vi.fn());
vi.mock("../api.ts", () => ({ retryModel }));

afterEach(() => {
  cleanup();
  retryModel.mockReset();
});

const BENCH: ModelSittingOut = {
  until: "2026-10-29T14:00:00Z",
  resets_at: null,
  window: "session",
  said: "You've hit your session limit",
  since: "2026-10-29T09:00:00Z",
  nodes: ["node-1"],
};

const ok = (value: unknown) => ({ kind: "ok", status: 200, value });
const refused = (code: string, sentence: string) => ({
  kind: "refused",
  status: 409,
  code,
  sentence,
  detail: "",
  hint: "",
  body: {},
});

/** default and aux share a plan and both sit out; solo is on another sign-in. */
function open() {
  const data = modelsData({
    entries: [
      apiKeyEntry({ key: "default", plan: "p1", health: "sitting_out", sitting_out: BENCH }),
      apiKeyEntry({ key: "aux", plan: "p1", health: "sitting_out", sitting_out: BENCH }),
      apiKeyEntry({ key: "solo", plan: "p2", health: "sitting_out", sitting_out: BENCH }),
    ],
  });
  const onClose = vi.fn();
  const onDone = vi.fn();
  render(<RetryDialog model={data.models[0]!} data={data} onClose={onClose} onDone={onDone} />);
  return { onClose, onDone };
}

const press = async (name: string) => {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
};

test("Retry now asks every model on the plan, the model first, and a cleared plan closes the dialog", async () => {
  retryModel
    .mockResolvedValueOnce(ok({ cleared: true, mode: "probe", node: "node-1", still: null }))
    .mockResolvedValueOnce(refused("not_sitting_out", "Nothing to retry; test it instead."));
  const { onClose, onDone } = open();
  expect(screen.getByText(/The models default and aux share one plan/)).toBeDefined();
  await press("Retry now");
  expect(retryModel.mock.calls.map((c) => [c[0], c[1]])).toEqual([
    ["default", "probe"],
    ["aux", "probe"],
  ]);
  expect(onClose).toHaveBeenCalled();
  expect(onDone).toHaveBeenCalled();
});

test("a limit that still holds says until when, and the dialog stays", async () => {
  retryModel.mockResolvedValueOnce(
    ok({ cleared: false, mode: "probe", node: "node-1", still: { ...BENCH } }),
  );
  const { onClose } = open();
  await press("Retry now");
  expect(screen.getByText("Still limited")).toBeDefined();
  expect(screen.getByText(/The vendor still limits default\. It sits out until/)).toBeDefined();
  expect(retryModel).toHaveBeenCalledTimes(1);
  expect(onClose).not.toHaveBeenCalled();
});

test("a probe that failed for another reason clears nothing and says why", async () => {
  retryModel.mockResolvedValueOnce(
    ok({
      cleared: false,
      mode: "probe",
      node: "node-1",
      still: null,
      failed: "the harness timed out",
    }),
  );
  const { onClose } = open();
  await press("Retry now");
  expect(screen.getByText("Nothing was cleared")).toBeDefined();
  expect(
    screen.getByText(/failed for a reason other than a limit: the harness timed out/),
  ).toBeDefined();
  expect(onClose).not.toHaveBeenCalled();
});

test("a model no longer sitting out has nothing to retry", async () => {
  retryModel.mockResolvedValueOnce(
    refused("not_sitting_out", "Nothing to retry; test it instead."),
  );
  open();
  await press("Clear sit-out");
  expect(retryModel).toHaveBeenCalledWith("default", "clear");
  expect(screen.getByText("Nothing to retry")).toBeDefined();
  expect(screen.getByText("Nothing to retry; test it instead.")).toBeDefined();
});

// QA Minor 10: the safe action takes the focus, and the title leads with a word.
test("the dialog opens on Retry now, under a title that names the model", () => {
  open();
  expect(screen.getByRole("dialog", { name: "Model default is sitting out" })).toBeDefined();
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Retry now" }));
});
