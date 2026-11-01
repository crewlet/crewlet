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

const polled = vi.hoisted(() => ({
  data: null as unknown,
  error: null as string | null,
  detail: undefined as string | undefined,
  calls: [] as unknown[][],
}));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (...args: unknown[]) => {
    polled.calls.push(args);
    return {
      data: polled.data,
      loading: false,
      error: polled.error,
      detail: polled.detail,
      refusal: null,
      refetch: () => {},
    };
  },
}));

afterEach(() => {
  cleanup();
  api.startDeviceSignIn.mockReset();
  api.cancelDeviceSignIn.mockReset();
  polled.data = null;
  polled.error = null;
  polled.detail = undefined;
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
      apiKeyEntry({ key: "codex", harness: "codex", health: "rejected", device_sign_in: running }),
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

/** A deferred answer: the promise, and the hand that settles it. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

test("a cancel that has started finishes after the dialog closed, and acts for nothing on screen", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  const answer = deferred<unknown>();
  let sent: unknown = "unset";
  api.cancelDeviceSignIn.mockImplementation((_id: string, signal?: AbortSignal) => {
    sent = signal;
    return answer.promise;
  });
  const { onClose, onDone, view } = open();
  await screen.findByText("QXJM-7KPD");
  fireEvent.click(screen.getByRole("button", { name: "Cancel sign-in" }));
  view.unmount();
  // NO SIGNAL: closing the dialog cannot stop the cancel part way.
  expect(sent).toBeUndefined();
  await act(async () => answer.resolve({ kind: "ok", status: 200, value: SESSION }));
  expect(onDone).not.toHaveBeenCalled();
  expect(onClose).not.toHaveBeenCalled();
});

test("a refused cancel says so in its own words, and the sign-in stays on screen", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  api.cancelDeviceSignIn.mockResolvedValue({
    kind: "refused",
    status: 409,
    code: "conflict",
    sentence: "The sign-in already ended.",
    detail: "",
    hint: "",
    body: {},
  });
  const { onClose } = open();
  await screen.findByText("QXJM-7KPD");
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Cancel sign-in" }));
  });
  expect(screen.getByText("The sign-in could not be cancelled")).toBeDefined();
  expect(screen.queryByText("The sign-in could not start")).toBeNull();
  expect(screen.getByText("QXJM-7KPD")).toBeDefined();
  expect(onClose).not.toHaveBeenCalled();
});

test("a start that failed offers Start again, which asks again", async () => {
  api.startDeviceSignIn.mockResolvedValueOnce({
    kind: "refused",
    status: 502,
    code: "device_sign_in_failed",
    sentence: "Codex did not print a device code.",
    detail: "",
    hint: "",
    body: {},
  });
  api.startDeviceSignIn.mockResolvedValueOnce({ kind: "ok", status: 201, value: SESSION });
  open();
  expect(await screen.findByText("The sign-in could not start")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Start again" }));
  expect(await screen.findByText("QXJM-7KPD")).toBeDefined();
  expect(api.startDeviceSignIn).toHaveBeenCalledTimes(2);
});

test("a session the engine no longer answers for is said, never left waiting, and can start again", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  const { view, model, onDone } = open();
  await screen.findByText("QXJM-7KPD");
  polled.error = "not_found";
  polled.detail = "No device sign-in dev-1.";
  view.rerender(<DeviceDialog model={model} onClose={() => {}} onDone={onDone} />);
  expect(screen.getByText("This sign-in can no longer be followed")).toBeDefined();
  expect(screen.getByText(/No device sign-in dev-1\./)).toBeDefined();
  expect(screen.queryByText("Waiting for you to approve the code")).toBeNull();
  polled.error = null;
  polled.detail = undefined;
  fireEvent.click(screen.getByRole("button", { name: "Start again" }));
  await waitFor(() => expect(api.startDeviceSignIn).toHaveBeenCalledTimes(2));
});

test("a session gone from this reader's sight is lost too, as one that no longer exists is", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  const { view, model, onDone } = open();
  await screen.findByText("QXJM-7KPD");
  polled.error = "unauthorized";
  view.rerender(<DeviceDialog model={model} onClose={() => {}} onDone={onDone} />);
  expect(screen.getByText("This sign-in can no longer be followed")).toBeDefined();
  expect(screen.getByRole("button", { name: "Start again" })).toBeDefined();
});

test("a poll that failed for a moment keeps the status and asks again", async () => {
  api.startDeviceSignIn.mockResolvedValue({ kind: "ok", status: 201, value: SESSION });
  const { view, model, onDone } = open();
  await screen.findByText("QXJM-7KPD");
  for (const code of ["unavailable", "timeout", "closed"]) {
    polled.error = code;
    polled.detail = "The node is still starting.";
    view.rerender(<DeviceDialog model={model} onClose={() => {}} onDone={onDone} />);
    expect(screen.queryByText("This sign-in can no longer be followed"), code).toBeNull();
    expect(screen.getByText("Waiting for you to approve the code"), code).toBeDefined();
    expect(screen.queryByRole("button", { name: "Start again" }), code).toBeNull();
    // STILL ASKING: the poll stays on while the session is live.
    expect(polled.calls.at(-1)![2], code).toEqual({ enabled: true, pollMs: DEVICE_POLL_MS });
  }
});

test("only an https address is offered to open", async () => {
  api.startDeviceSignIn.mockResolvedValue({
    kind: "ok",
    status: 201,
    value: { ...SESSION, url: "http://auth.openai.com/codex/device" },
  });
  open();
  await screen.findByText("QXJM-7KPD");
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText(/not a secure \(https\) address/)).toBeDefined();
});
