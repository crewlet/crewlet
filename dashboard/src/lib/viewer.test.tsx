/**
 * The three states a reader can be in, and the sentences that depend on them.
 *
 * A screen that folds two of these together is a screen that tells somebody to
 * go and find a credential they already have, or reports a fault where the
 * remedy is a line of company configuration.
 */

import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { useViewer } from "./viewer.ts";
import { useClient, useConnection } from "./store-hooks.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

/** answering wires the socket to give one `viewer` answer. */
function answering(answer: unknown) {
  const query = vi.fn().mockResolvedValue(answer);
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("who the dashboard thinks you are", () => {
  test("a bound token names the seat, and is neither unbound nor anonymous", async () => {
    answering({
      operator_id: "ops-1",
      operator: true,
      handle: "ana",
      name: "Ana Diaz",
      kind: "human",
    });
    const { result } = renderHook(() => useViewer());
    await waitFor(() => expect(result.current.handle).toBe("ana"));
    expect(result.current.name).toBe("Ana Diaz");
    expect(result.current.kind).toBe("human");
    expect(result.current.unbound).toBe(false);
    expect(result.current.anonymous).toBe(false);
  });

  // AN ORDINARY STATE. The remedy is a line of company configuration, and a
  // screen can only say which line if it has the id to put in it.
  test("a token no seat claims is UNBOUND and still carries its operator id", async () => {
    answering({ operator_id: "ops-7", operator: true, handle: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer());
    await waitFor(() => expect(result.current.unbound).toBe(true));
    expect(result.current.operatorID).toBe("ops-7");
    expect(result.current.anonymous).toBe(false);
  });

  test("no token at all is ANONYMOUS, which is a different sentence", async () => {
    answering({ operator_id: "", operator: false, handle: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer());
    await waitFor(() => expect(result.current.anonymous).toBe(true));
    expect(result.current.unbound).toBe(false);
    expect(result.current.operator).toBe(false);
  });

  // NOT ANONYMOUS WHILE LOADING. Every first paint has no answer yet, and a
  // screen that read the loading state as "no credential" would flash "no
  // credential is presented" at every reader on every navigation.
  test("an unanswered query is not yet anybody", () => {
    const query = vi.fn(() => new Promise(() => {}));
    vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
    vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
    const { result } = renderHook(() => useViewer());
    expect(result.current.loading).toBe(true);
    expect(result.current.anonymous).toBe(false);
    expect(result.current.unbound).toBe(false);
  });
});
