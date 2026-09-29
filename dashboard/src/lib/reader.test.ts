/**
 * Who a tab is read by: what a sign-in decides the tab's hand-over against.
 */

import { afterEach, describe, expect, test } from "vitest";
import { adoptReader, currentReader, noteReader, onReader } from "./reader.ts";

afterEach(() => {
  sessionStorage.clear();
});

describe("the tab's reader", () => {
  test("is nobody until something records one", () => {
    expect(currentReader()).toBeNull();
  });

  // A SIGN-IN IS THE ENGINE'S OWN ANSWER about who the tab is about to be
  // read by, so it records whatever was there.
  test("a sign-in records its person, over anybody recorded before", () => {
    noteReader("p-1");
    expect(currentReader()).toBe("p-1");
    noteReader("p-2");
    expect(currentReader()).toBe("p-2");
  });

  // THE FRAME ADOPTS a session only on a tab nobody has recorded: a cookie
  // that moved under an open tab — a sign-in as somebody else in another tab
  // — does not make what this tab holds theirs.
  test("the frame adopts the session it finds only where nobody is recorded", () => {
    adoptReader("p-1");
    expect(currentReader()).toBe("p-1");
    adoptReader("p-2");
    expect(currentReader()).toBe("p-1");
  });

  // IT DESCRIBES THE TAB, so it goes with the tab's storage — which is what a
  // sign-out empties — and survives the reload a hand-over is.
  test("lives in the tab's own storage, and goes when it is emptied", () => {
    noteReader("p-1");
    expect(sessionStorage.getItem("crewlet_reader")).toBe("p-1");
    sessionStorage.clear();
    expect(currentReader()).toBeNull();
  });

  test("says so when it changes, and not when it does not", () => {
    let heard = 0;
    const stop = onReader(() => heard++);
    noteReader("p-1");
    noteReader("p-1");
    adoptReader("p-2");
    expect(heard).toBe(1);
    stop();
    noteReader("p-3");
    expect(heard).toBe(1);
  });

  test("an empty person is nobody, and records nothing", () => {
    noteReader("");
    adoptReader("");
    expect(currentReader()).toBeNull();
  });
});
