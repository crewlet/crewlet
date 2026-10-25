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
import { LogosBody } from "./BrandCards.tsx";

const { createObjectURL, revokeObjectURL } = URL;

afterEach(() => {
  cleanup();
  Object.assign(URL, { createObjectURL, revokeObjectURL });
  vi.restoreAllMocks();
});

const DOC: BrandDocument = {
  v: 1,
  name: "FairSplit",
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
  expect(saved).toEqual([
    { href: "blob:dashboard/download", download: "fairsplit-primary-logo.svg" },
  ]);
  // The press's own link is gone with it.
  expect(container.ownerDocument.querySelector('a[href^="blob:"]')).toBeNull();
});

test("a slot with no logo offers no download", () => {
  render(<LogosBody document={DOC} assets={ASSETS} />);
  expect(screen.queryByRole("button", { name: "Download the mark logo" })).toBeNull();
});
