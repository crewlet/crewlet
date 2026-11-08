/**
 * A message body as a room draws it: a shortcode in the words is its emoji,
 * the same characters in code stay as written, and a fenced sample is the
 * kit's code block with its copy control.
 */
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";

import { SRC } from "~/test/source.ts";

import { MessageBody } from "./Body.tsx";

test("a shortcode in the words is drawn as its emoji, and in code as written", () => {
  render(
    <MessageBody
      message={{ body: "Shipped :tada: with `:tada:` in code" }}
      viewer="ada"
      nameOf={(handle) => handle}
    />,
  );
  expect(screen.getByText(/Shipped 🎉 with/)).toBeTruthy();
  expect(screen.getByText(":tada:", { selector: "code" })).toBeTruthy();
});

// A FENCED SAMPLE IN CHAT IS THE KIT'S CODE BLOCK, copied from its corner
// (a page keeps `md-code`). Mutation: drop `code: "copy"` from CHAT_WORDS and
// the copy control goes.
test("a fenced sample is the kit's code block, with a copy control", () => {
  const { container } = render(
    <MessageBody
      message={{ body: "Run this:\n\n```sh\nmake dashboard\n```" }}
      viewer="ada"
      nameOf={(handle) => handle}
    />,
  );
  expect(screen.getByRole("button", { name: "Copy code" })).toBeTruthy();
  expect(screen.getByText("make dashboard")).toBeTruthy();
  expect(container.querySelector("pre.md-code")).toBeNull();
});

// ONLY THE PARAGRAPH "(edited)" FOLLOWS RUNS INLINE. The rule once took the
// second-to-last child, so in any message of three paragraphs and no mark the
// middle one ran inline and lost its line. Mutation: match
// `p:nth-last-child(2)` again and the middle paragraph reads inline.
test("only the paragraph the edited mark follows runs inline", () => {
  const style = document.createElement("style");
  style.textContent = readFileSync(join(SRC, "styles", "chatlet-room.css"), "utf8");
  document.head.append(style);
  try {
    const words = "First paragraph.\n\nSecond paragraph.\n\nThird paragraph.";
    const { unmount } = render(
      <MessageBody message={{ body: words }} viewer="ada" nameOf={(handle) => handle} />,
    );
    for (const text of ["First paragraph.", "Second paragraph.", "Third paragraph."]) {
      expect(getComputedStyle(screen.getByText(text).closest("p")!).display).toBe("block");
    }
    unmount();
    render(
      <MessageBody
        message={{ body: words }}
        viewer="ada"
        nameOf={(handle) => handle}
        after={<span>(edited)</span>}
      />,
    );
    expect(getComputedStyle(screen.getByText("Second paragraph.").closest("p")!).display).toBe(
      "block",
    );
    expect(getComputedStyle(screen.getByText("Third paragraph.").closest("p")!).display).toBe(
      "inline",
    );
  } finally {
    style.remove();
  }
});
