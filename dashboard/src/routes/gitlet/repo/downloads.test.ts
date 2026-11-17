import { renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { archiveHref, forgetDownloadProbes, rawHref, useDownloadsServed } from "./downloads.ts";

const COMMIT = "0123456789abcdef0123456789abcdef01234567";

afterEach(() => {
  forgetDownloadProbes();
  vi.unstubAllGlobals();
});

// THE DOWNLOADS ARE RELATIVE, on the dashboard's own origin, at a whole commit.
test("a ZIP is asked of the dashboard's origin by the whole commit", () => {
  expect(archiveHref("web.site", COMMIT)).toBe(`/git/web.site/-/archive.zip?rev=${COMMIT}`);
});

// A PATH IS ONE QUERY VALUE: a space, `+`, `#`, `?` and a non-ASCII letter
// stay inside it. Mutation: put the path in the URL's path, and `#` ends it.
test("a raw file's path is one percent-encoded value", () => {
  const href = rawHref("web", COMMIT, "docs/a b+c#d?e é.md", true);
  expect(href.startsWith(`/git/web/-/raw?rev=${COMMIT}&path=`)).toBe(true);
  const query = new URLSearchParams(href.split("?").slice(1).join("?"));
  expect(query.get("path")).toBe("docs/a b+c#d?e é.md");
  expect(query.get("download")).toBe("1");
  expect(href).toContain("%2B");
  expect(href).not.toContain("#");
  expect(rawHref("web", COMMIT, "go.mod")).not.toContain("download");
});

function probing(answer: () => Promise<Response>) {
  const asked: { url: string; method: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      asked.push({ url: String(input), method: init?.method ?? "GET" });
      return answer();
    }),
  );
  return asked;
}

// ONE HEAD PER NAME: 405 is a build that serves downloads, 404 an older one,
// anything else cannot tell. Mutation: read 404 as unknown, and an older fleet
// draws a link that downloads an error.
test.each([
  ["405", () => Promise.resolve(new Response(null, { status: 405 })), "yes"],
  ["404", () => Promise.resolve(new Response("no route", { status: 404 })), "no"],
  ["500", () => Promise.resolve(new Response("", { status: 500 })), "unknown"],
  ["a failure", () => Promise.reject(new TypeError("offline")), "unknown"],
] as const)("a probe answering %s reads as %s", async (_, answer, served) => {
  const asked = probing(answer);
  const { result } = renderHook(() => useDownloadsServed("web", true));
  expect(result.current).toBe("checking");
  await waitFor(() => expect(result.current).toBe(served));
  const again = renderHook(() => useDownloadsServed("web", true));
  await waitFor(() => expect(again.result.current).toBe(served));
  expect(asked).toHaveLength(1);
  expect(asked[0]!.method).toBe("HEAD");
  expect(new URL(asked[0]!.url).pathname).toBe("/git/web/-/archive.zip");
});

test("nothing is probed until it is asked", () => {
  const asked = probing(() => Promise.resolve(new Response(null, { status: 405 })));
  const { result } = renderHook(() => useDownloadsServed("web", false));
  expect(result.current).toBe("checking");
  expect(asked).toHaveLength(0);
});
