/**
 * The dictation socket is the page's own origin at `/ws/dictation`, over
 * `wss` where the page is https, as every socket here dials.
 */
import { afterEach, expect, test, vi } from "vitest";
import { canOpenDictationSocket, openDictationSocket } from "./dictation.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

test("dials the page's own origin at /ws/dictation", () => {
  const dialed: string[] = [];
  vi.stubGlobal(
    "WebSocket",
    class {
      constructor(url: string) {
        dialed.push(url);
      }
    },
  );
  expect(canOpenDictationSocket()).toBe(true);
  openDictationSocket();
  expect(dialed).toEqual([`ws://${location.host}/ws/dictation`]);
});
