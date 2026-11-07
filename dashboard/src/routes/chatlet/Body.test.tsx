/**
 * A message body as a room draws it: a shortcode in the words is its emoji,
 * and the same characters in code stay as written.
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
