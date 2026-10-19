/**
 * The Artifacts gallery: who a row names, which columns a section draws, what
 * a row in the trash offers, and where the listing's controls stand.
 *
 * WHAT IS HELD HERE IS THE GALLERY: the engine's answers are stood in for by
 * `useQuery`, and every kind's cover by nothing, since each has its own suite.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ArtifactGallery } from "./Gallery.tsx";
import { Router } from "~/app/router.tsx";
import type { ArtifactSection } from "~/app/routes.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ArtifactListItem } from "~/protocol/index.ts";

vi.mock("~/lib/useQuery.ts", () => ({ useQuery: vi.fn() }));
vi.mock("~/lib/useRest.ts", () => ({ useRest: () => ({ data: null }) }));
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: () => ({ socket: { query: vi.fn() } }), useOrg: () => null };
});
vi.mock("~/lib/viewer.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/viewer.ts")>("~/lib/viewer.ts");
  return { ...actual, useViewer: () => ({ owner: "token:review", admin: false, handle: "" }) };
});
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "token:review", acts: [] }) };
});
vi.mock("~/lib/artifacts/pins.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/artifacts/pins.ts")>("~/lib/artifacts/pins.ts");
  return {
    ...actual,
    usePinToggle: () => ({ label: "Pin to your sidebar", toggle: vi.fn(), pinned: false }),
  };
});

function row(over: Partial<ArtifactListItem>): ArtifactListItem {
  return {
    id: "a1",
    kind: "doc",
    title: "Architecture notes",
    owner: "token:review",
    owner_kind: "operator",
    head_number: 1,
    role: "owner",
    status: "live",
    capabilities: [],
    share_count: 0,
    created_by: { name: "token:review", kind: "operator" },
    updated_by: { name: "token:review", kind: "operator" },
    created_at: "2026-10-18T10:00:00Z",
    updated_at: "2026-10-18T10:00:00Z",
    trashed_at: "0001-01-01T00:00:00Z",
    opened_at: "0001-01-01T00:00:00Z",
    ...over,
  } as ArtifactListItem;
}

let items: ArtifactListItem[];

beforeEach(() => {
  items = [
    row({}),
    row({ id: "a2", title: "Vendor scorecard", kind: "sheet", owner: "maya", role: "editor" }),
  ];
  vi.mocked(useQuery).mockImplementation(((what: string, params?: { pinned?: boolean }) => {
    const data =
      what === "artifacts" ? { items: params?.pinned ? [] : items, total: items.length } : null;
    return { data, loading: false, error: null, refusal: null, refetch: vi.fn() };
  }) as never);
});

afterEach(cleanup);

function mount(section: ArtifactSection, hash = "#/artifacts") {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <ArtifactGallery section={section} />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

function headers(): string[] {
  return screen.getAllByRole("columnheader").map((h) => h.textContent ?? "");
}

test("the reader's own artifact is owned by You, and anybody else's by their name", () => {
  mount("for-you");
  const grid = document.body;
  // It said "token:review", the credential's spelling, on every row.
  expect(within(grid).queryByText("token:review")).toBeNull();
  const own = within(grid).getByText("Architecture notes").closest(".grid-row") as HTMLElement;
  expect(within(own).getByText("You")).toBeTruthy();
  const theirs = within(grid).getByText("Vendor scorecard").closest(".grid-row") as HTMLElement;
  expect(within(theirs).getByText("maya")).toBeTruthy();
  expect(within(theirs).getByText("Can edit")).toBeTruthy();
});

test("Mine draws no owner and no role, which would say You and Owner on every row", () => {
  mount("mine", "#/artifacts/mine");
  expect(headers()).toEqual(["Name", "Type", "Updated", ""]);
});

test("the trash says when each went in and when it goes for good, and offers no pin", () => {
  items = [row({ status: "trashed", trashed_at: "2026-10-18T10:00:00Z", title: "Old draft" })];
  mount("mine", "#/artifacts/mine?status=trashed");
  expect(headers()).toEqual(["Name", "Type", "Trashed", "Deleted for good", ""]);
  fireEvent.click(screen.getByRole("button", { name: "More for “Old draft”" }));
  expect(screen.queryByRole("menuitem", { name: "Pin to your sidebar" })).toBeNull();
  expect(screen.getByRole("menuitem", { name: "Copy link" })).toBeTruthy();
});

// THE TOGGLE SAYS IT IS ONE: the kit's Button takes `pressed`, and an
// `aria-pressed` passed beside it was overwritten with nothing.
test("the type, the order, the trash and the shape are the toolbar's, and the bar keeps the field and New", () => {
  mount("mine", "#/artifacts/mine");
  const toolbar = document.querySelector(".toolbar") as HTMLElement;
  expect(within(toolbar).getByRole("button", { name: "Trash" }).getAttribute("aria-pressed")).toBe(
    "false",
  );
  expect(within(toolbar).getByRole("radio", { name: "List" })).toBeTruthy();
  expect(within(toolbar).getByRole("radio", { name: "Cards" })).toBeTruthy();
  expect(within(toolbar).queryByRole("searchbox")).toBeNull();
  expect(screen.getByRole("searchbox", { name: "Search artifacts" })).toBeTruthy();
});

test("a card names no owner on the reader's own, and its changer only when that is somebody else", () => {
  items = [
    row({}),
    row({
      id: "a2",
      title: "Vendor scorecard",
      owner: "maya",
      updated_by: { name: "token:review", kind: "operator" },
    }),
  ];
  mount("for-you", "#/artifacts?shape=cards");
  const own = screen.getByRole("link", { name: "Architecture notes" });
  expect(own.textContent).not.toContain("token:review");
  expect(own.textContent).not.toContain(" by ");
  const theirs = screen.getByRole("link", { name: "Vendor scorecard" });
  expect(theirs.textContent).toContain("maya");
  expect(theirs.textContent).toContain("by you");
});
