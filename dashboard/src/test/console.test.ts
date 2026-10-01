/**
 * The watcher every case runs under: what it records, what it lets a case
 * silence, and what it puts back.
 *
 * Against a console of its own rather than the real one, because the real one
 * is being watched for THIS case too, and a case about the watcher that wrote
 * to it would be failed by it.
 */

import { expect, test, vi } from "vitest";
import { watchConsole, watchFile } from "./console.ts";

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

// A FILE IS WATCHED OUTSIDE ITS CASES TOO. What it says while it loads, in a
// `beforeAll`, between two cases or in an `afterAll` is nobody's case — and a
// watch that ran only from each case's start to its end passed all of it over.
test("what a file says outside every case is the file's, and inside a case the case's", () => {
  const target = fakeConsole();
  const watch = watchFile(target);
  target.warn("while the file loads");
  watch.caseStarts();
  target.error("inside the first case");
  expect(watch.caseEnds()).toEqual(["console.error: inside the first case"]);
  target.warn("between the cases");
  watch.caseStarts();
  expect(watch.caseEnds()).toEqual([]);
  target.error("in the file's afterAll");
  expect(watch.fileEnds()).toEqual([
    "console.warn: while the file loads",
    "console.warn: between the cases",
    "console.error: in the file's afterAll",
  ]);
});

test("a file's watch ends with both methods put back, a case's silencing not outliving it", () => {
  const target = fakeConsole();
  const { error, warn } = target;
  const watch = watchFile(target);
  watch.caseStarts();
  // A case that silenced and never restored leaves nothing behind its span.
  vi.spyOn(target, "error").mockImplementation(() => {});
  watch.caseEnds();
  target.error("after the case, unsilenced");
  expect(error).toHaveBeenCalledWith("after the case, unsilenced");
  expect(watch.fileEnds()).toEqual(["console.error: after the case, unsilenced"]);
  expect(target.error).toBe(error);
  expect(target.warn).toBe(warn);
});
