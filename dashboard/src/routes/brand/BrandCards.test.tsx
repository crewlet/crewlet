/**
 * The Logos card's downloads: each logo is saved at the press from the bytes
 * the page already holds, under a name made safe by the kit, and the card
 * never draws a lasting link to a logo's object URL (an SVG behind such a link
 * could be opened as a page on the dashboard's origin).
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { BrandDocument } from "~/protocol/index.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import { HeadingLevelProvider } from "@crewlethq/ui";
import { LogosBody } from "./BrandCards.tsx";
import { BrandBoard } from "./BrandBoard.tsx";

const { createObjectURL, revokeObjectURL } = URL;

afterEach(() => {
  cleanup();
  Object.assign(URL, { createObjectURL, revokeObjectURL });
  vi.restoreAllMocks();
});

const DOC: BrandDocument = {
  v: 1,
  name: "Acme",
  logos: { primary: "logos/primary.svg" },
  colors: { primary: "#0f7b5f" },
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};

const SVG = new Blob(["<svg/>"], { type: "image/svg+xml" });

const ASSETS: BrandAssets = {
  logo: (slot) => (slot === "primary" ? "blob:dashboard/primary" : null),
  logoFile: (slot) => (slot === "primary" ? SVG : null),
  face: () => undefined,
};

test("a logo is saved at the press from the bytes the page holds, and no link to its object URL is drawn", () => {
  const made = vi.fn(() => "blob:dashboard/download");
  Object.assign(URL, { createObjectURL: made, revokeObjectURL: vi.fn() });
  const saved: { href: string; download: string }[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    saved.push({ href: this.href, download: this.download });
  });

  const { container } = render(<LogosBody document={DOC} assets={ASSETS} />);
  expect(container.querySelector('a[href^="blob:"]')).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Download the primary logo" }));
  expect(made).toHaveBeenCalledWith(SVG);
  expect(saved).toEqual([{ href: "blob:dashboard/download", download: "acme-primary-logo.svg" }]);
  // The press's own link is gone with it.
  expect(container.ownerDocument.querySelector('a[href^="blob:"]')).toBeNull();
});

test("a slot with no logo offers no download", () => {
  render(<LogosBody document={DOC} assets={ASSETS} />);
  expect(screen.queryByRole("button", { name: "Download the mark logo" })).toBeNull();
});

// THE SLOTS LINE UP ROW BY ROW. In a plain grid a description that wrapped moved
// its own preview down alone, 18px off its neighbours at most widths. The kit's
// group gives every slot one set of rows. Mutation: lay the slots out in a div.
test("the three logos stand in one aligned group", () => {
  render(<LogosBody document={DOC} assets={ASSETS} />);
  const group = screen.getByRole("group", { name: "Logos" });
  expect(group.querySelectorAll(".crewlet-logo-slot")).toHaveLength(3);
});

// THE BOARD'S HEADINGS STEP BY ONE from the surface it stands on: its cards
// were a fixed h3, straight under the h1 on an open brand kit, and the voice
// lists a fixed h4 under the brand page's h2 cards. Mutation: fix either
// level again and a case goes red.
test("the board's cards and the voice's lists take the levels under the surface", () => {
  const voiced: BrandDocument = {
    ...DOC,
    voice: { summary: "Plain.", write_like: ["Short sentences"], avoid: ["Jargon"] },
  };
  const levels = () =>
    [...document.querySelectorAll("h1, h2, h3, h4, h5, h6")].map(
      (h) => `${h.tagName} ${h.textContent}`,
    );
  render(<BrandBoard document={voiced} assets={ASSETS} drawn="brand" />);
  const top = levels();
  expect(top.filter((l) => l.startsWith("H2 "))).toContain("H2 Voice and tone");
  expect(top).toContain("H3 Write like this");
  expect(top).toContain("H3 Avoid");
  expect(top.some((l) => /^H[4-6] /.test(l))).toBe(false);
  cleanup();
  render(
    <HeadingLevelProvider level={3}>
      <BrandBoard document={voiced} assets={ASSETS} drawn="brand" />
    </HeadingLevelProvider>,
  );
  expect(levels()).toContain("H3 Voice and tone");
  expect(levels()).toContain("H4 Write like this");
});
