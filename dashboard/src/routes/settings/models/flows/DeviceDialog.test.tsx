/**
 * A device sign-in shows the address and the code, follows the session every
 * three seconds until it ends, resumes a session already running rather than
 * starting a second, and refreshes the page the moment the code is approved,
 * so the page behind never keeps saying the sign-in was refused (QA M4).
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { ModelDevice } from "~/protocol/models.ts";
import { apiKeyEntry, modelsData } from "~/routes/settings/models/fixtures.ts";
import { DEVICE_POLL_MS, DeviceDialog } from "./DeviceDialog.tsx";

const api = vi.hoisted(() => ({ startDeviceSignIn: vi.fn(), cancelDeviceSignIn: vi.fn() }));
vi.mock("../api.ts", () => api);

const polled = vi.hoisted(() => ({ data: null as unknown, calls: [] as unknown[][] }));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (...args: unknown[]) => {
    polled.calls.push(args);
    return { data: polled.data, loading: false, error: null, refusal: null, refetch: () => {} };
  },
}));

afterEach(() => {
  cleanup();
  api.startDeviceSignIn.mockReset();
  api.cancelDeviceSignIn.mockReset();
  polled.data = null;
  polled.calls = [];
});

const SESSION: ModelDevice = {
  id: "dev-1",
  entry: "codex",
  node: "node-1",
  url: "https://auth.openai.com/codex/device",
  code: "QXJM-7KPD",
  status: "pending",
  started_at: "2026-10-29T10:00:00Z",
  expires_at: "2026-10-29T10:15:00Z",
};

function open(running: ModelDevice | null = null) {
  const data = modelsData({
    entries: [
      apiKeyEntry({ key: "codex", agent: "codex", health: "rejected", device_sign_in: running }),
    ],
  });
  const onDone = vi.fn();
  const onClose = vi.fn();
  const view = render(<DeviceDialog model={data.models[0]!} onClose={onClose} onDone={onDone} />);
  return { onDone, onClose, view, model: data.models[0]! };
}

test("a started sign-in shows the address and the code, and follows the session every three seconds", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  open();
  const link = await screen.findByRole("link", { name: /Open auth\.openai\.com/ });
  expect(link.getAttribute("href")).toBe(SESSION.url);
  // QA Minor 11: the address is the next thing to do, so it takes the focus,
  // in the same commit that draws it: read once, the moment it is there.
  expect(document.activeElement).toBe(link);
  expect(screen.getByText("QXJM-7KPD")).toBeDefined();
  await waitFor(() => {
    const last = polled.calls.at(-1)!;
    expect(last[0]).toBe("llm_device_sign_in");
    expect(last[1]).toEqual({ id: "dev-1" });
    expect(last[2]).toEqual({ enabled: true, pollMs: DEVICE_POLL_MS });
  });
  expect(DEVICE_POLL_MS).toBe(3000);
});

test("a sign-in already running for the model is resumed, never started twice", async () => {
  api.startDeviceSignIn.mockResolvedValue({
    kind: "refused",
    status: 409,
    code: "sign_in_in_progress",
    sentence: "A sign-in is already running.",
    detail: "",
    hint: "",
    body: { running: { ...SESSION, code: "RUNS-0001" } },
  });
  open();
  expect(await screen.findByText("RUNS-0001")).toBeDefined();
  expect(screen.queryByText("A sign-in is already running.")).toBeNull();
});

test("a session the reading already carries is followed without a start", async () => {
  open(SESSION);
  expect(await screen.findByText("QXJM-7KPD")).toBeDefined();
  expect(api.startDeviceSignIn).not.toHaveBeenCalled();
});

test("approval refreshes the page once and stops asking", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  const { onDone, view, model } = open();
  await screen.findByText("QXJM-7KPD");
  polled.data = { ...SESSION, status: "approved" };
  view.rerender(<DeviceDialog model={model} onClose={() => {}} onDone={onDone} />);
  expect(await screen.findByText("Signed in")).toBeDefined();
  view.rerender(<DeviceDialog model={model} onClose={() => {}} onDone={onDone} />);
  expect(onDone).toHaveBeenCalledTimes(1);
  expect(polled.calls.at(-1)![2]).toEqual({ enabled: false, pollMs: DEVICE_POLL_MS });
});

test("Cancel sign-in cancels the running session", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  api.cancelDeviceSignIn.mockResolvedValue({
    kind: "ok",
    status: 200,
    value: { ...SESSION, status: "cancelled" },
  });
  const { onClose } = open();
  await screen.findByText("QXJM-7KPD");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Cancel sign-in" }));
  });
  expect(api.cancelDeviceSignIn).toHaveBeenCalledWith("dev-1");
  expect(onClose).toHaveBeenCalled();
});
