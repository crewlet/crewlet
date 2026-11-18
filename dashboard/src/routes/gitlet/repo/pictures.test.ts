import { expect, test } from "vitest";
import { picturePath, pictureType } from "./pictures.tsx";

// A DOCUMENT'S PICTURE IS A PATH FROM ITS DIRECTORY, or from the root with a
// leading slash; an address elsewhere is not the repository's. Mutation: keep
// `..` or a query in the path, or take a URL for a path, and this fails.
test.each([
  ["assets/logo.png", "", "assets/logo.png"],
  ["./assets/logo.png", "docs", "docs/assets/logo.png"],
  ["../img/a.png", "docs/guide", "docs/img/a.png"],
  ["/img/a.png", "docs", "img/a.png"],
  ["a%20b.png?raw=true#top", "", "a b.png"],
  ["../../a.png", "docs", null],
  ["https://example.com/a.png", "", null],
  ["//example.com/a.png", "", null],
  ["data:image/png;base64,AAAA", "", null],
  ["#anchor", "", null],
])("%s from %s is %s", (src, dir, path) => {
  expect(picturePath(src, dir)).toBe(path);
});

test("a picture's type is read off its extension, and any other file is none", () => {
  expect(pictureType("a/LOGO.PNG")).toBe("image/png");
  expect(pictureType("icon.svg")).toBe("image/svg+xml");
  expect(pictureType("photo.jpeg")).toBe("image/jpeg");
  expect(pictureType("main.go")).toBeNull();
  expect(pictureType("Makefile")).toBeNull();
});
