/**
 * A proposed file on its card: an image is drawn through an `<img>` from the
 * bytes the owner's route answers, typed from the reference; nothing from the
 * file is ever put into the page as markup.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { DraftFile } from "./DraftFile.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const REF = {
  kind: "file" as const,
  file_id: "0b7f1c3e-8a4d-4e2b-9c1a-3f5e6d7a8b9c",
  conversation_id: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
  name: "logo.svg",
  media_type: "image/svg+xml",
};

test("an image is drawn through an img from the owner's route, never inlined", async () => {
  const fetched: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      fetched.push(String(input instanceof Request ? input.url : input));
      return new Response('<svg xmlns="http://www.w3.org/2000/svg"><script>x()</script></svg>');
    }),
  );
  const made: Blob[] = [];
  vi.stubGlobal(
    "URL",
    Object.assign(URL, {
      createObjectURL: (blob: Blob) => {
        made.push(blob);
        return "blob:test/logo";
      },
      revokeObjectURL: () => {},
    }),
  );
  const { container } = render(<DraftFile file={REF} />);
  await waitFor(() =>
    expect(screen.getByRole("img", { name: "logo.svg (image/svg+xml)" }).getAttribute("src")).toBe(
      "blob:test/logo",
    ),
  );
  expect(fetched[0]).toMatch(
    /\/assistant\/conversations\/1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f\/files\/0b7f1c3e-8a4d-4e2b-9c1a-3f5e6d7a8b9c$/,
  );
  expect(made[0]?.type).toBe("image/svg+xml");
  expect(container.querySelector("svg script")).toBeNull();
});

test("a file that is not an image is its name and type, and nothing is fetched", () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  render(<DraftFile file={{ ...REF, name: "brief.pdf", media_type: "application/pdf" }} />);
  expect(screen.getByText("brief.pdf (application/pdf)")).toBeTruthy();
  expect(fetch).not.toHaveBeenCalled();
});

test("a file proposed for a dark-ground field is drawn on that ground, in the brand's colour", () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("<svg/>")),
  );
  const { container } = render(
    <DraftFile file={REF} ground={{ tone: "dark", color: "#0d1717" }} />,
  );
  const stage = container.querySelector(".crewlet-logo-slot__stage--dark") as HTMLElement | null;
  expect(stage).not.toBeNull();
  expect(stage?.style.getPropertyValue("--crewlet-logo-slot-ground")).toBe("#0d1717");
  expect(container.querySelector(".crewlet-logo-slot__stage--light")).toBeNull();
});

test("a file with no declared ground is drawn on the light ground", () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("<svg/>")),
  );
  const { container } = render(<DraftFile file={REF} />);
  expect(container.querySelector(".crewlet-logo-slot__stage--light")).not.toBeNull();
});
