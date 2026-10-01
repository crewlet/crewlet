/**
 * The watcher every case runs under: what it records, what it lets a case
 * silence, and what it puts back.
 *
 * Against a console of its own rather than the real one, because the real one
 * is being watched for THIS case too, and a case about the watcher that wrote
 * to it would be failed by it.
 */

import { expect, test, vi } from "vitest";
import { watchConsole } from "./console.ts";

function fakeConsole() {
  return { error: vi.fn(), warn: vi.fn() };
}

test("what a case writes to error or warn is recorded, formatted, and still printed", () => {
  const target = fakeConsole();
  const { error, warn } = target;
  const stop = watchConsole(target);
  target.error("Each child in a list should have a unique %s prop.", '"key"');
  target.warn("[crewlet] announce() was called with no <Announcer> mounted:", "Added SRE");
  expect(stop()).toEqual([
    'console.error: Each child in a list should have a unique "key" prop.',
    "console.warn: [crewlet] announce() was called with no <Announcer> mounted: Added SRE",
  ]);
  // PASSED THROUGH, so the run's log still shows it where it happened.
  expect(error).toHaveBeenCalledTimes(1);
  expect(warn).toHaveBeenCalledTimes(1);
});

test("a case silences what it expects, and only that", () => {
  const target = fakeConsole();
  const stop = watchConsole(target);
  // What a case holding a throw does: replace the method for the call it
  // expects. The watcher underneath is not reached, so nothing is recorded.
  const quiet = vi.spyOn(target, "error").mockImplementation(() => {});
  target.error("The above error occurred in the <Reader> component");
  quiet.mockRestore();
  // And what it did not expect still is.
  target.warn("something else");
  expect(stop()).toEqual(["console.warn: something else"]);
});

test("stopping puts both methods back as they were, a case's own silencing included", () => {
  const target = fakeConsole();
  const { error, warn } = target;
  const stop = watchConsole(target);
  // A case that silenced and never restored: the next case must not inherit
  // its silence.
  vi.spyOn(target, "error").mockImplementation(() => {});
  stop();
  expect(target.error).toBe(error);
  expect(target.warn).toBe(warn);
});
