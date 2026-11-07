/**
 * A message body as a room draws it: a shortcode in the words is its emoji,
 * the same characters in code stay as written, and a fenced sample is the
 * kit's code block with its copy control.
 */
import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";

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
