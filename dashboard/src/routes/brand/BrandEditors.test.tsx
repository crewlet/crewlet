/**
 * The brand's card editors, which are also the setup's steps: what a picked
 * logo becomes, and when a field says what is wrong with it.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { LogoFields } from "./BrandEditors.tsx";
import type { BrandEdit } from "~/lib/brand/draft.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import type { BrandDocument } from "~/protocol/index.ts";

afterEach(cleanup);

const DOC: BrandDocument = {
  v: 1,
  name: "Northwind",
  logos: {},
  colors: {},
  type: { heading: { kit: "geist" }, body: { kit: "geist" } },
  voice: {},
};

const EDIT: BrandEdit = { document: DOC, files: new Map() };

const ASSETS: BrandAssets = {
  logo: () => null,
  logoFile: () => null,
  face: () => undefined,
};

// A DRAWING WITH ONLY A VIEWBOX IS KEPT WITH ITS SIZE, the usual export from a
// design tool, which otherwise uploaded and then drew at 0 by 0. Mutation: keep
// the file as it was picked.
test("a picked SVG that states no size is kept with its viewBox's", async () => {
  const onEdit = vi.fn();
  const { container } = render(<LogoFields edit={EDIT} onEdit={onEdit} assets={ASSETS} />);
  const picker = container.querySelector<HTMLInputElement>('input[type="file"]')!;
  const drawn = new File(
    [`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 300 75"></svg>`],
    "logo.svg",
    { type: "image/svg+xml" },
  );
  fireEvent.change(picker, { target: { files: [drawn] } });
  await waitFor(() => expect(onEdit).toHaveBeenCalled());
  const next = onEdit.mock.calls[0]![0] as BrandEdit;
  expect(next.document.logos.primary).toBe("logos/primary.svg");
  const kept = next.files.get("logos/primary.svg")!;
  expect(await kept.text()).toContain(`width="300" height="75"`);
  expect(screen.queryByRole("alert")).toBeNull();
});
