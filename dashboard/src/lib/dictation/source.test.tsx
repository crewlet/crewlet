/**
 * The source every composer passes: the relay first while the engine can
 * dictate, the browser's own route after it, and the mic held with the
 * engine's reason only where neither can work: a member reads the reason, an
 * administrator is offered the guide beside it.
 */
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { DictationSource } from "@crewlethq/ui";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";

vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

function viewer(over: Partial<ViewerState>): ViewerState {
  return { admin: false, ...over } as ViewerState;
}

/** A fresh module per case: the browser's answer is asked once per page. */
async function source(state: ViewerState): Promise<DictationSource> {
  vi.mocked(useViewer).mockReturnValue(state);
  const { useDictation } = await import("./source.ts");
  const { result } = renderHook(() => useDictation());
  await waitFor(() => expect(result.current).toBeTruthy());
  return result.current;
}

const routes = (s: DictationSource) =>
  s.transport as readonly NonNullable<DictationSource["transport"]>[];
const relay = (s: DictationSource) => (routes(s)[0] as { available: () => boolean }).available();

/** A browser with on-device recognition and its pack; answers what it was asked. */
function onDeviceRecognition() {
  const asked = vi.fn(async () => "available");
  class Recognition {
    static available = asked;
  }
  Object.defineProperty(Recognition.prototype, "processLocally", { value: false, writable: true });
  vi.stubGlobal("SpeechRecognition", Recognition);
  return asked;
}

/** jsdom declares no `isSecureContext`; a browser on https or loopback says true. */
function secure(on: boolean) {
  Object.defineProperty(window, "isSecureContext", { value: on, configurable: true });
}

beforeEach(() => {
  vi.resetModules();
  vi.stubGlobal("AudioWorkletNode", class {});
  secure(true);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test("while the engine can dictate the relay goes first and nothing is held", async () => {
  const s = await source(viewer({ dictation: { available: true } }));
  expect(routes(s)).toHaveLength(2);
  await waitFor(() => expect(relay(s)).toBe(true));
  expect(s.unavailableReason).toBeUndefined();
  expect(s.setup).toBeUndefined();
});

test("before the viewer has said, the relay is tried and the socket decides", async () => {
  const s = await source(viewer({}));
  await waitFor(() => expect(relay(s)).toBe(true));
  expect(s.unavailableReason).toBeUndefined();
});

test("no relay on a page that is not a secure context or has no audio worklet", async () => {
  secure(false);
  const s = await source(viewer({ dictation: { available: true } }));
  await waitFor(() => expect(relay(s)).toBe(false));
});

test("an engine that cannot dictate holds a member's mic with its reason alone", async () => {
  vi.mocked(useViewer).mockReturnValue(
    viewer({ dictation: { available: false, reason: "disabled" } }),
  );
  const { useDictation } = await import("./source.ts");
  const { result } = renderHook(() => useDictation());
  await waitFor(() =>
    expect(result.current.unavailableReason).toBe("Dictation is turned off on this server"),
  );
  expect(result.current.setup).toBeUndefined();
  expect(relay(result.current)).toBe(false);
});

test("an administrator is offered the guide beside the reason", async () => {
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(useViewer).mockReturnValue(
    viewer({ admin: true, dictation: { available: false, reason: "model_unavailable" } }),
  );
  const { useDictation, DICTATION_GUIDE } = await import("./source.ts");
  const { result } = renderHook(() => useDictation());
  await waitFor(() =>
    expect(result.current.unavailableReason).toBe("This server could not load its speech model"),
  );
  expect(result.current.setup?.label).toBe("How to set up dictation");
  result.current.setup?.onSelect();
  expect(open).toHaveBeenCalledWith(DICTATION_GUIDE, "_blank", "noopener");
});

test("where the browser can recognise on the device it stands in, and nothing asks it before a press", async () => {
  const asked = onDeviceRecognition();
  vi.mocked(useViewer).mockReturnValue(
    viewer({ dictation: { available: false, reason: "failed" } }),
  );
  const { useDictation } = await import("./source.ts");
  const { result } = renderHook(() => useDictation());
  await waitFor(() => expect(relay(result.current)).toBe(false));
  expect(result.current.unavailableReason).toBeUndefined();
  expect(asked).not.toHaveBeenCalled();
});
