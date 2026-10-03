/**
 * The three states a reader can be in, and the sentences that depend on them.
 *
 * A screen that folds two of these together is a screen that tells somebody to
 * go and find a credential they already have, or reports a fault where the
 * remedy is a line of company configuration.
 */

import { act, cleanup, renderHook, waitFor } from "~/test/inCase.ts";
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
  test("a bound principal names the seat, and is neither unbound nor anonymous", async () => {
    answering({
      login: "ops-1",
      grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
      handle: "ana",
      name: "Ana Diaz",
      kind: "human",
      owner: "ana",
      acts: ["create_work_item", "set_pins"],
      project: "ENG",
    });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.handle).toBe("ana"));
    expect(result.current.name).toBe("Ana Diaz");
    expect(result.current.kind).toBe("human");
    // WHOSE RECORD, WHAT MAY BE DONE, AND WHERE A CREATE LANDS are the
    // engine's answers, carried as given.
    expect(result.current.owner).toBe("ana");
    expect(result.current.acts).toEqual(["create_work_item", "set_pins"]);
    expect(result.current.project).toBe("ENG");
    expect(result.current.unbound).toBe(false);
    expect(result.current.anonymous).toBe(false);
  });

  // AN ORDINARY STATE. The remedy is a binding in the identity directory, and
  // a screen can only say what to bind if it has the login to name. And it is
  // not a reason to withhold a press: an unbound caller acts under its login.
  test("a credential no seat is bound to is UNBOUND and still carries its login", async () => {
    answering({
      login: "ops-7",
      grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
      handle: "",
      name: "",
      kind: "",
      owner: "ops-7",
      acts: ["set_pins"],
      project: "",
    });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.unbound).toBe(true));
    expect(result.current.login).toBe("ops-7");
    expect(result.current.owner).toBe("ops-7");
    expect(result.current.acts).toEqual(["set_pins"]);
    expect(result.current.anonymous).toBe(false);
  });

  test("nobody resolved at all is ANONYMOUS, which is a different sentence", async () => {
    answering({ login: "", grants: [], handle: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.anonymous).toBe(true));
    expect(result.current.unbound).toBe(false);
    expect(result.current.operatesFleet).toBe(false);
    // An answer that names no acts acts for nobody — never `undefined`.
    expect(result.current.acts).toEqual([]);
    expect(result.current.project).toBe("");
  });

  // THE ADMIN PATH OVER SOMEBODY'S WORK RECORD IS fleet:operate, AND ONLY IT.
  //
  // The field read `people:manage`, which is authority over person ROWS in the
  // identity directory and opens nobody's queue: the engine refuses a
  // colleague's inbox to a caller holding it alone and answers a caller
  // holding `fleet:operate`. A screen gating on the wrong one asked for what
  // it would be refused and hid what it would be given. Each grant is held
  // ALONE, so neither case passes on the other's back.
  test("people:manage alone does not open somebody's work record", async () => {
    answering({ login: "hr-1", grants: ["people:manage"], handle: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.login).toBe("hr-1"));
    expect(result.current.operatesFleet).toBe(false);
  });

  test("fleet:operate alone does", async () => {
    answering({ login: "sre-1", grants: ["fleet:operate"], handle: "", name: "", kind: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.login).toBe("sre-1"));
    expect(result.current.operatesFleet).toBe(true);
  });

  // A FAILED READ IS NOT AN ANSWER, and anonymity is the worst of the three
  // to guess: it locks the app rail, My work and the inbox for an operator
  // whose credential is valid and every one of whose other queries answered.
  // The engine reports anonymity as an EMPTY login with no error, so a throw
  // here means only that nothing came back.
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
  // screen that read the loading state as "no credential" would flash "no
  // credential is presented" at every reader on every navigation.
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
      grants: ["state:read"],
      handle: "ana",
      name: "Ana Diaz",
      kind: "human",
      owner: "ana",
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
