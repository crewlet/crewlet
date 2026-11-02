/**
 * The Review step on its own: what holds a finish is said under the part it
 * is about, only once a finish was tried, and the first of it is where focus
 * goes.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { BrandDocument } from "~/protocol/index.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import { ReviewStep, partStatus, warningKey, setupWarnings } from "./Review.tsx";

afterEach(cleanup);

const ASSETS: BrandAssets = {
  logo: () => null,
  logoFile: () => null,
  face: () => undefined,
} as unknown as BrandAssets;

function doc(over: Partial<BrandDocument> = {}): BrandDocument {
  return {
    v: 1,
    name: "FairSplit",
    logos: {},
    colors: { primary: "#0f7b5f" },
    type: { heading: { kit: "geist" }, body: { kit: "geist" } },
    voice: {},
    ...over,
  };
}

function review(document: BrandDocument, showErrors: boolean, acknowledged = new Set<string>()) {
  return render(
    <ReviewStep
      edit={{ document, files: new Map() }}
      assets={ASSETS}
      admin
      onEditStep={() => {}}
      acknowledged={acknowledged}
      onAcknowledge={() => {}}
      showErrors={showErrors}
    />,
  );
}

const row = (name: string) =>
  screen.getAllByRole("listitem").find((item) => within(item).queryByRole("heading", { name }))!;

test("a part missing what an adoption requires says so under its values once a finish was tried", () => {
  const { rerender } = review(doc(), false);
  expect(within(row("Logos")).getByText("Missing required")).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
  rerender(
    <ReviewStep
      edit={{ document: doc(), files: new Map() }}
      assets={ASSETS}
      admin
      onEditStep={() => {}}
      acknowledged={new Set()}
      onAcknowledge={() => {}}
      showErrors
    />,
  );
  const alert = within(row("Logos")).getByRole("alert");
  expect(alert.textContent).toBe("Add the primary logo, for light grounds.");
  // FOCUS GOES TO THE WAY BACK TO THAT PART, the first thing that holds.
  const held = document.querySelectorAll("[data-hold]");
  expect(held).toHaveLength(1);
  expect(held[0]).toBe(within(row("Logos")).getByRole("button", { name: "Edit Logos" }));
});

test("with every part in place, the first unconfirmed pair is what holds", () => {
  const document = doc({ logos: { primary: "logos/primary.svg" }, colors: { primary: "#00baba" } });
  const pairs = setupWarnings(document);
  expect(pairs.length).toBeGreaterThan(0);
  review(document, true);
  const group = screen.getByRole("group", { name: "Confirm low contrast" });
  const boxes = within(group).getAllByRole("checkbox");
  expect(boxes.every((box) => box.getAttribute("aria-invalid") === "true")).toBe(true);
  const held = globalThis.document.querySelectorAll("[data-hold]");
  expect(held).toHaveLength(1);
  expect(held[0]).toBe(boxes[0]);
  expect(within(group).getByRole("alert").textContent).toBe(
    pairs.length === 1 ? "Confirm the pair to publish." : "Confirm each pair to publish.",
  );
});

test("the colours need attention until every low pair is confirmed", () => {
  const document = doc({ logos: { primary: "logos/primary.svg" }, colors: { primary: "#00baba" } });
  expect(partStatus("colours", document)).toBe("attention");
  const all = new Set(setupWarnings(document).map(warningKey));
  expect(partStatus("colours", document, all)).toBe("defaults");
  expect(partStatus("colours", doc({ colors: {} }), all)).toBe("missing");
});
