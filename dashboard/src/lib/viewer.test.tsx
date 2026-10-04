/**
 * The three states a reader can be in, and the sentences that depend on them.
 *
 * A screen that folds two of these together is a screen that tells somebody to
 * sign in when they are signed in, or reports a fault where the remedy is a
 * binding an administrator makes (`crewlet iam bind`).
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { ViewerProvider, useViewer } from "./viewer.ts";
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
  test("a bound person names the seat, and is neither unbound nor anonymous", async () => {
    answering({
      login: "ana.diaz",
      grants: ["work:write", "fleet:operate"],
      handle: "ana",
      owner: "ana",
      name: "Ana Diaz",
      kind: "human",
    });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.handle).toBe("ana"));
    expect(result.current.name).toBe("Ana Diaz");
    expect(result.current.kind).toBe("human");
    expect(result.current.owner).toBe("ana");
    expect(result.current.operatesFleet).toBe(true);
    expect(result.current.unbound).toBe(false);
    expect(result.current.anonymous).toBe(false);
  });

  // AN ORDINARY STATE. Somebody is signed in and the directory binds them to
  // no seat: their record is their login, which is what every personal read
  // asks by.
  test("a person no seat holds is UNBOUND, and their record is their login", async () => {
    answering({
      login: "ops.seven",
      grants: ["work:write"],
      handle: "",
      owner: "ops.seven",
      name: "",
      kind: "",
    });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.unbound).toBe(true));
    expect(result.current.login).toBe("ops.seven");
    expect(result.current.owner).toBe("ops.seven");
    expect(result.current.operatesFleet).toBe(false);
    expect(result.current.anonymous).toBe(false);
  });

  test("nobody signed in is ANONYMOUS, which is a different sentence", async () => {
    answering({ login: "", grants: [], handle: "", owner: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.anonymous).toBe(true));
    expect(result.current.unbound).toBe(false);
    expect(result.current.grants).toEqual([]);
  });

  // A FAILED READ IS NOT AN ANSWER, and anonymity is the worst of the three
  // to guess: it locks the app rail, My work and the inbox for a person whose
  // session is valid and every one of whose other queries answered. The
  // engine reports anonymity as an EMPTY login with no error, so a throw here
  // means only that nothing came back.
  test("a read that failed is nobody yet, not ANONYMOUS", async () => {
    // ONE rejected promise, so the test can wait on the very object the hook
    // awaited: our continuation is attached after the hook's, and microtasks
    // run in order, so by the time this resumes the hook's catch has already
    // set its state. Without that ordering the case would pass on a flush
    // that never happened, which is the failure mode it exists to rule out.
    const refused = Promise.reject(new Error("timeout"));
    const query = vi.fn(() => refused);
    vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
    vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await act(async () => {
      await refused.catch(() => {});
    });
    expect(query).toHaveBeenCalled();
    expect(result.current.anonymous).toBe(false);
    expect(result.current.unbound).toBe(false);
    expect(result.current.loading).toBe(true);
  });

  // NOT ANONYMOUS WHILE LOADING. Every first paint has no answer yet, and a
  // screen that read the loading state as "nobody" would flash "nobody is
  // signed in" at every reader on every navigation.
  test("an unanswered query is not yet anybody", () => {
    const query = vi.fn(() => new Promise(() => {}));
    vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
    vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    expect(result.current.loading).toBe(true);
    expect(result.current.anonymous).toBe(false);
    expect(result.current.unbound).toBe(false);
  });
});

// WHO THIS BROWSER IS IS ASKED ONCE, by the frame, however many surfaces ask
// the hook. A reader per caller was a standing query per caller: the frame,
// the sidebar and the Inbox count made three of the socket's four slots, and
// each polled on its own clock.
describe("one reading", () => {
  test("every reader under one provider shares one query", async () => {
    const query = vi.fn().mockResolvedValue({
      login: "ana.diaz",
      grants: [],
      handle: "ana",
      owner: "ana",
      name: "Ana Diaz",
      kind: "human",
    });
    vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
    vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
    const { result } = renderHook(() => [useViewer(), useViewer(), useViewer()], {
      wrapper: ViewerProvider,
    });
    await waitFor(() => expect(result.current[2]!.handle).toBe("ana"));
    expect(query).toHaveBeenCalledTimes(1);
  });

  // AND A CALLER OUTSIDE ONE IS TOLD, rather than answered by a read of its
  // own: that fallback is the per-caller read, back, and it would work.
  test("outside a provider the hook refuses rather than asking for itself", () => {
    const query = vi.fn();
    vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
    vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
    // React reports the throw on the console as well as throwing it.
    const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => renderHook(() => useViewer())).toThrow(/outside a ViewerProvider/);
    quiet.mockRestore();
    expect(query).not.toHaveBeenCalled();
  });
});
