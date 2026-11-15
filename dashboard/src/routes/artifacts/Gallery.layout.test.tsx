/**
 * THE GALLERY, LAID OUT: what the trash says sits whole under the toolbar.
 *
 * The toolbar is sticky, opaque and over the content, and it takes back its
 * host's gap so what follows it starts flush at its foot (`.toolbar` in
 * styles/frame.css). A line that pulled itself up on top of that slid under
 * the band: the trash's "Only you see your trash" showed the lower half of
 * its letters. Measured in a real browser, since jsdom lays nothing out.
 */
import "~/test/layout.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { eventually } from "~/test/settle.ts";
import { ArtifactGallery } from "./Gallery.tsx";

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
vi.mock("~/lib/useRest.ts", () => ({ useRest: () => ({ data: null }) }));
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: () => ({ socket: { query: vi.fn() } }), useOrg: () => null };
});
vi.mock("~/lib/viewer.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/viewer.ts")>("~/lib/viewer.ts");
  return {
    ...actual,
    useViewer: () => ({ owner: "jane", admin: false, handle: "jane" }),
    useHeldReading: () => ({ login: "jane", grants: ["artifacts"] }),
    useHeldGrants: () => ["artifacts"],
  };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "jane", acts: [] }) };
});

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.mocked(useQuery).mockImplementation((() => ({
    data: { contents: [], total: 0 },
    loading: false,
    error: null,
    refusal: null,
    refetch: vi.fn(),
  })) as never);
  location.hash = "#/artifacts/yours?status=trashed";
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  location.hash = "";
});

async function mount(width: number) {
  await page.viewport(width, 900);
  host = document.createElement("div");
  // THE CONTENT COLUMN'S RHYTHM: a column of the page's parts a gap apart,
  // which the toolbar takes back under itself.
  host.style.cssText = `width: ${width}px; display: flex; flex-direction: column; gap: var(--spacing-4); --toolbar-host-gap: var(--spacing-4);`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <LayerHost>
          <ToastProvider>
            <ArtifactGallery section="yours" />
          </ToastProvider>
        </LayerHost>
      </Router>,
    ),
  );
  await eventually(() => expect(note()).not.toBeNull());
  await document.fonts.ready;
}

const note = () =>
  [...document.querySelectorAll<HTMLElement>("p")].find((p) =>
    p.textContent?.startsWith("Only you see your trash"),
  ) ?? null;

test.each([1440, 390])(
  "the trash's own line sits whole under the toolbar at %ipx",
  async (width) => {
    await mount(width);
    const bar = document.querySelector<HTMLElement>(".toolbar")!;
    const line = note()!;
    expect(line.getBoundingClientRect().top).toBeGreaterThanOrEqual(
      bar.getBoundingClientRect().bottom - 0.5,
    );
    // AND NOTHING IS PAINTED OVER ITS FIRST LINE: the band is opaque.
    const r = line.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + 4, r.top + 2);
    expect(hit === line || line.contains(hit)).toBe(true);
  },
);
