/**
 * The three states a reader can be in, and the sentences that depend on them —
 * and the role beside them, which is a different fact.
 *
 * A screen that folds two of these together is a screen that tells somebody to
 * go and find a credential they already have, or reports a fault where the
 * remedy is a line of company configuration.
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { ViewerProvider, adminToAsk, readsPersonOf, useViewer } from "./viewer.ts";
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
      token_id: "ops-1",
      role: "member",
      reach: "member",
      linked: true,
      handle: "ana",
      name: "Ana Diaz",
      kind: "human",
      line: ["bo"],
      admins: [{ handle: "jane", name: "Jane Founder" }],
    });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.handle).toBe("ana"));
    expect(result.current.name).toBe("Ana Diaz");
    expect(result.current.kind).toBe("human");
    expect(result.current.linked).toBe(true);
    expect(result.current.unbound).toBe(false);
    expect(result.current.anonymous).toBe(false);
    expect(result.current.line).toEqual(["bo"]);
    expect(result.current.admins).toEqual([{ handle: "jane", name: "Jane Founder" }]);
  });

  // AN ORDINARY STATE. The remedy is a line of company configuration, and a
  // screen can only say which line if it has the id to put in it.
  test("a token no seat claims is UNBOUND and still carries its id", async () => {
    answering({ token_id: "ops-7", role: "admin", reach: "admin", linked: false, handle: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.unbound).toBe(true));
    expect(result.current.tokenID).toBe("ops-7");
    expect(result.current.anonymous).toBe(false);
  });

  test("no token at all is ANONYMOUS, which is a different sentence", async () => {
    answering({ token_id: "", role: "", reach: "public", linked: false, handle: "" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.anonymous).toBe(true));
    expect(result.current.unbound).toBe(false);
    expect(result.current.admin).toBe(false);
    expect(result.current.role).toBe("");
    expect(result.current.reach).toBe("public");
  });

  // THE ROLE IS NOT THE LINK. An admin is whoever the engine says reaches the
  // admin surfaces — the REACH, never the presence of a key — so a linked
  // teammate on a member's key is no admin, and an unlinked pipeline key that
  // is an admin's is one.
  test.each([
    ["a linked member", { role: "member", reach: "member", linked: true, handle: "ana" }, false],
    ["an unlinked admin", { role: "admin", reach: "admin", linked: false, handle: "" }, true],
    ["a linked admin", { role: "admin", reach: "admin", linked: true, handle: "ana" }, true],
  ] as const)("%s is an admin: %s", async (_who, answer, admin) => {
    answering({ token_id: "k", ...answer });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.admin).toBe(admin);
    expect(result.current.role).toBe(answer.role);
    expect(result.current.linked).toBe(answer.linked);
  });

  // A DISABLED GUARD ANSWERS EVERY CALLER AS AN ADMIN under its reserved id:
  // the reach decides, so the guarded sections open rather than lock.
  test("whoever the engine reaches as an admin is one, whatever the key", async () => {
    answering({ token_id: "anonymous", role: "admin", reach: "admin", linked: false });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.admin).toBe(true));
    expect(result.current.anonymous).toBe(false);
  });

  // AN ANSWER WITH NO LINE OR ADMINS IS NOBODY'S LINE AND NO ADMIN, never an
  // absence a screen has to guard against at every read.
  test("a line and the admins are always lists", async () => {
    answering({ token_id: "", reach: "public" });
    const { result } = renderHook(() => useViewer(), { wrapper: ViewerProvider });
    await waitFor(() => expect(result.current.anonymous).toBe(true));
    expect(result.current.line).toEqual([]);
    expect(result.current.admins).toEqual([]);
  });

  // A FAILED READ IS NOT AN ANSWER, and anonymity is the worst of the three
  // to guess: it locks the app rail, My work and the inbox for a person whose
  // key is valid and every one of whose other queries answered. The engine
  // reports anonymity as an EMPTY token id with no error, so a throw here
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
      token_id: "ops-1",
      role: "member",
      reach: "member",
      linked: true,
      handle: "ana",
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

// WHO A PERSON ASKS for what their key does not reach: the first admin the
// engine named, by name, else their handle — and "an admin" when nobody is
// named, which is every anonymous reader.
test("the admin to ask is the first named, or an admin", () => {
  expect(
    adminToAsk([
      { handle: "jane", name: "Jane Founder" },
      { handle: "rui", name: "Rui Santos" },
    ]),
  ).toBe("Jane Founder");
  expect(adminToAsk([{ handle: "jane", name: "" }])).toBe("jane");
  expect(adminToAsk([])).toBe("an admin");
});

// A PERSON'S RECORDS ARE THEIRS AND THEIR LEADS', as the engine answers them
// (`viewerParty`): their own seat, a seat in their line, and nobody else —
// an admin's key adds nothing here, which is why the rule takes no role.
test("a person's records are read by them and the leads in their line, and nobody else", () => {
  const ana = { handle: "ana", line: ["bo", "cy"] };
  expect(readsPersonOf(ana, "ana")).toBe(true);
  expect(readsPersonOf(ana, "bo")).toBe(true);
  expect(readsPersonOf(ana, "dee")).toBe(false);
  // A KEY NO SEAT NAMES has no line, and no record of its own.
  expect(readsPersonOf({ handle: "", line: [] }, "ana")).toBe(false);
  expect(readsPersonOf({ handle: "", line: [] }, "")).toBe(false);
});
