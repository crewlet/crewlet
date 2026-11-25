/**
 * Vendor text through the kit: a linked vendor UI name is one box, the link
 * itself, with its new-tab mark inside the frame; linked plain words stay a link
 * in the sentence.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { VendorWords } from "./VendorWords.tsx";

afterEach(cleanup);

test("a linked vendor UI name is the link, its new-tab mark inside the chip", () => {
  render(
    <p>
      <VendorWords
        text={[
          { text: "A " },
          { ui: ["Web application"] },
          { text: " client from " },
          {
            ui: ["Google Cloud", "Credentials"],
            url: "https://console.cloud.google.com/apis/credentials",
          },
          { text: ", in a project you control." },
        ]}
      />
    </p>,
  );
  const link = screen.getByRole("link", {
    name: /^Google Cloud then Credentials\s\(opens in a new tab\)$/,
  });
  expect(link.classList).toContain("crewlet-vendor-ui");
  expect(link.getAttribute("target")).toBe("_blank");
  expect(link.querySelector(".crewlet-vendor-ui__external")).toBeTruthy();
  expect(link.parentElement?.closest("a")).toBeNull();
  expect(link.nextSibling?.textContent).toBe(", in a project you control.");
});

test("a link that waits on a form value draws the name without a link", () => {
  render(
    <VendorWords
      text={[{ ui: ["Console", "Keys"], url: "https://{region}.example.com/keys" }]}
      resolveUrl={() => ""}
    />,
  );
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText("Keys").closest(".crewlet-vendor-ui")).toBeTruthy();
});
