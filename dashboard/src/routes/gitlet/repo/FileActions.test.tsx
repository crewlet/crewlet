import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { CodeBlobAnswer } from "~/protocol/index.ts";
import { forgetDownloadProbes } from "./downloads.ts";
import { COPY_MAX_BYTES, FileBytes, copyHeld, copyWhole } from "./FileActions.tsx";

afterEach(() => {
  cleanup();
  forgetDownloadProbes();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "clipboard");
});

const blob = (extra: Partial<CodeBlobAnswer> = {}) =>
  ({ path: "a.go", oid: "a".repeat(40), size: 10, lines: 1, ...extra }) as CodeBlobAnswer;

/** A read the case settles itself. */
function pending() {
  let settle!: (text: string) => void;
  const read = new Promise<string>((resolve) => (settle = resolve));
  return { read, settle };
}

// A BROWSER THAT TAKES A PROMISE IN ClipboardItem is written at the press, before the
// read settles. Mutation: await the read before the write, and this fails.
test("the write is made at the press where ClipboardItem takes a promise", async () => {
  const items: Record<string, Promise<Blob>>[] = [];
  class Item {
    static supports = (type: string) => type === "text/plain";
    constructor(readonly parts: Record<string, Promise<Blob>>) {
      items.push(parts);
    }
  }
  vi.stubGlobal("ClipboardItem", Item);
  const write = vi.fn(async (list: Item[]) => {
    await list[0]!.parts["text/plain"];
  });
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { write } });
  const { read, settle } = pending();
  const copied = copyWhole(() => read);
  expect(write).toHaveBeenCalledTimes(1);
  settle("whole text");
  expect(await copied).toBe(true);
  expect(await (await items[0]!["text/plain"]!).text()).toBe("whole text");
});

// ELSEWHERE THE TEXT IS WRITTEN ONCE READ, and a failed read is the read's failure,
// never the clipboard's. Mutation: swallow the read's error, and this fails.
test("without ClipboardItem the text is written once read, and a failed read throws", async () => {
  const writeText = vi.fn(async () => undefined);
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const { read, settle } = pending();
  const copied = copyWhole(() => read);
  expect(writeText).not.toHaveBeenCalled();
  settle("whole text");
  expect(await copied).toBe(true);
  expect(writeText).toHaveBeenCalledWith("whole text");
  await expect(copyWhole(() => Promise.reject(new Error("query_failed")))).rejects.toThrow();
});

// COPY IS HELD PAST ITS BOUND, for a binary file and an empty one. Mutation: hold at
// another bound, and this fails.
test("Copy is held past 1 MiB, for a binary file and for an empty one", () => {
  expect(copyHeld(blob({ size: COPY_MAX_BYTES }))).toBeUndefined();
  expect(copyHeld(blob({ size: COPY_MAX_BYTES + 1 }))).toBe(
    "This file is 1.0 MiB: copy one past 1 MiB from Raw.",
  );
  expect(copyHeld(blob({ binary: true }))).toMatch(/^A binary file/);
  expect(copyHeld(blob({ size: 0, lines: 0 }))).toBe("This file is empty.");
});

// RAW, DOWNLOAD AND A COPY THAT MUST READ wait for the commit. Mutation: link the
// branch while the commit is unknown, and this fails.
test("Raw, Download and Copy wait for the commit the file was read at", () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(null, { status: 405 })),
  );
  render(<FileBytes repo="web" path="a.go" commit="" file={blob()} loaded={null} />);
  for (const name of ["Raw", "Download a.go", "Copy the file's contents"]) {
    const held = screen.getByRole("button", { name });
    expect(held.getAttribute("aria-disabled"), name).toBe("true");
  }
  expect(screen.queryByRole("link")).toBeNull();
});
