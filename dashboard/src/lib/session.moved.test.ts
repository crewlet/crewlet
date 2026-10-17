/**
 * A tab the engine refused for its reader is handed to whoever holds the
 * browser now — once, however many of its requests were refused together.
 *
 * `reader_changed` says only that the session is not the tab's reader's; who
 * it IS comes from the session read, and the hand-over is the one a sign-in
 * by them here would make: the tab's storage emptied, the new reader recorded
 * and the page reloaded where it was.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { currentReader } from "./reader.ts";
import { page, sessionMoved } from "./session.ts";
import { STORAGE_KEYS } from "./storage.ts";

let answer: () => Promise<Response>;

beforeEach(() => {
  answer = async () =>
    new Response(JSON.stringify({ person: "p-1", login: "erin.ng", status: "signed_in" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => answer()),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionStorage.clear();
});

/** A tab read by `reader`, holding a draft its reader left in it. */
function tab(reader: string): void {
  sessionStorage.setItem(STORAGE_KEYS.reader, reader);
  sessionStorage.setItem(STORAGE_KEYS.orgDraft, "the last reader's draft");
}

// Mutation: read the session and adopt it rather than take it, and nothing
// is reloaded — the last person's draft is the new person's.
test("a tab read by somebody else is handed to whoever the session is, once", async () => {
  tab("p-0");
  const reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
  await Promise.all([sessionMoved(), sessionMoved(), sessionMoved()]);
  expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);
  expect(reloads).toHaveBeenCalledTimes(1);
  expect(currentReader()).toBe("p-1");
  expect(sessionStorage.getItem(STORAGE_KEYS.orgDraft)).toBeNull();
});

// THE CONTROL: the cookie moved back to this tab's reader between the refusal
// and the read — their tab, and their draft, stay theirs.
test("a session that is the tab's own reader's again keeps the tab", async () => {
  tab("p-1");
  const reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
  await sessionMoved();
  expect(reloads).not.toHaveBeenCalled();
  expect(sessionStorage.getItem(STORAGE_KEYS.orgDraft)).toBe("the last reader's draft");
});

// A READ THAT FAILED says nothing about whose the browser is, so nothing is
// handed over on it — and the next refusal reads again rather than joining a
// read that is over.
test("a session read that failed hands nothing over, and the next one reads again", async () => {
  tab("p-0");
  const reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
  answer = async () => {
    throw new TypeError("network down");
  };
  await sessionMoved();
  expect(reloads).not.toHaveBeenCalled();
  expect(currentReader()).toBe("p-0");

  answer = async () =>
    new Response(JSON.stringify({ person: "p-1", status: "signed_in" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  await sessionMoved();
  expect(vi.mocked(fetch)).toHaveBeenCalledTimes(2);
  expect(reloads).toHaveBeenCalledTimes(1);
});
