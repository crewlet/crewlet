/**
 * The frame's one session read: who this browser is, and whether it may read
 * the company — asked again on a clock only while it may not.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { NO_ACCESS_RECHECK_MS, SessionReading, useFrameSession } from "./frameSession.ts";

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
});

let seen: ReturnType<typeof useFrameSession> | null = null;

function Probe() {
  seen = useFrameSession();
  return null;
}

const reads = () => vi.mocked(fetch).mock.calls.length;

// NOTHING ELSE CAN TELL A TAB WITH NO ACCESS THAT IT HAS SOME NOW: the identity
// move naming its person is pushed over the socket it does not have. So it asks
// again on a clock, and stops once an answer holds the grant. The CONTROL is
// the clock after that answer, which asks nothing more. Mutation: drop the
// clock, or keep it running for a session with access, and a count is wrong.
test("a session without state:read is asked again on a clock, and one with it is not", async () => {
  render(
    <SessionReading>
      <Probe />
    </SessionReading>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(reads()).toBe(1);
  expect(seen?.noAccess).toBe(true);

  grants = ["state:read"];
  await act(async () => {
    await vi.advanceTimersByTimeAsync(NO_ACCESS_RECHECK_MS);
  });
  expect(reads()).toBe(2);
  expect(seen?.noAccess).toBe(false);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(NO_ACCESS_RECHECK_MS * 10);
  });
  expect(reads()).toBe(2);
});

test("outside the frame's reading it refuses, rather than asking for itself", () => {
  vi.spyOn(console, "error").mockImplementation(() => {});
  expect(() => render(<Probe />)).toThrow(/outside a SessionReading/);
});
