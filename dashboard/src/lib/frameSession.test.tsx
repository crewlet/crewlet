/**
 * The frame's one session read: who this browser is — asked once, whatever it
 * holds, because every session is dialled and the socket says the rest.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { SessionReading, useFrameSession } from "./frameSession.ts";
import { currentReader } from "./reader.ts";
import { page } from "./session.ts";
import { STORAGE_KEYS } from "./storage.ts";

let grants: string[];

beforeEach(() => {
  vi.useFakeTimers();
  grants = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            person: "p-1",
            login: "erin.ng",
            kind: "person",
            grants,
            status: "signed_in",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionStorage.clear();
});

let seen: ReturnType<typeof useFrameSession> | null = null;

function Probe() {
  seen = useFrameSession();
  return null;
}

const reads = () => vi.mocked(fetch).mock.calls.length;

// A SESSION WITH NO APP IS READ ONCE, LIKE ANY OTHER: the socket admits every
// principal the engine resolves and serves them the overview, so access given
// later reaches the tab by the engine closing its socket to reconnect — never
// by the frame asking again on a clock. Mutation: put a clock back for a
// session holding nothing and the count moves.
test("a session holding no app is read once, and never again on a clock", async () => {
  render(
    <SessionReading>
      <Probe />
    </SessionReading>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(reads()).toBe(1);
  expect(seen?.answer?.grants).toEqual([]);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(10 * 60_000);
  });
  expect(reads()).toBe(1);
});

test("outside the frame's reading it refuses, rather than asking for itself", () => {
  vi.spyOn(console, "error").mockImplementation(() => {});
  expect(() => render(<Probe />)).toThrow(/outside a SessionReading/);
});

// A SESSION SOMEBODY ELSE'S SIGN-IN LEFT IN THE BROWSER HANDS THE TAB TO THEM.
//
// Signing in as somebody else in another tab ends this tab's session and moves
// the cookie the two share, so the frame's read answers for them — and the
// frame adopted nobody over the tab's reader and served them the last
// reader's tab: the builder's kept draft as their own, their stars under the
// last reader's key. The tab is handed over as a sign-in by them would hand
// it: emptied, its new reader noted, reloaded where it was. The CONTROLS are a
// tab read by the same person and a tab nobody has recorded, which carry on
// and adopt. Mutation: adopt rather than take the session and nothing reloads.
test("a session somebody else's sign-in left hands the tab to them", async () => {
  grants = [];
  for (const [reader, reloaded] of [
    ["p-0", true],
    ["p-1", false],
    [null, false],
  ] as const) {
    sessionStorage.clear();
    if (reader !== null) sessionStorage.setItem(STORAGE_KEYS.reader, reader);
    sessionStorage.setItem(STORAGE_KEYS.orgDraft, "the last reader's draft");
    const reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
    render(
      <SessionReading>
        <Probe />
      </SessionReading>,
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(reloads.mock.calls.length > 0).toBe(reloaded);
    expect(currentReader()).toBe("p-1");
    expect(sessionStorage.getItem(STORAGE_KEYS.orgDraft) === null).toBe(reloaded);
    cleanup();
    reloads.mockRestore();
  }
});
