import { expect, test } from "vitest";
import { archiveHref, rawHref } from "./downloads.ts";

const COMMIT = "0123456789abcdef0123456789abcdef01234567";

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
