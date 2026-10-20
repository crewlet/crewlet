/**
 * The Artifacts gallery: its three sections and the earlier sections'
 * addresses, the Make row, the grid by default and the list remembered, who a
 * card and a row name, which columns a section draws, what a row in the trash
 * offers, and where the listing's controls stand.
 *
 * WHAT IS HELD HERE IS THE GALLERY: the engine's answers are stood in for by
 * `useQuery`, and every live preview draws its stand-in, since jsdom has no
 * IntersectionObserver and `GalleryPreview` has its own suite.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { ArtifactGallery, noMatch } from "./Gallery.tsx";
import { Router } from "~/app/router.tsx";
import type { ArtifactSection } from "~/app/routes.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ArtifactListItem } from "~/protocol/index.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

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
vi.mock("~/lib/artifacts/pinToggle.ts", () => ({
  usePinToggle: () => ({ label: "Pin to your sidebar", toggle: vi.fn(), pinned: false }),
}));

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

afterEach(() => {
  cleanup();
  localStorage.clear();
});

/** The list shape, as a reader who chose it on this browser before. */
function asList() {
  localStorage.setItem(STORAGE_KEYS.artifactsShape, "list");
}

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
  asList();
  mount("all");
  const grid = document.body;
  // It said "token:review", the credential's spelling, on every row.
  expect(within(grid).queryByText("token:review")).toBeNull();
  const own = within(grid).getByText("Architecture notes").closest(".grid-row") as HTMLElement;
  expect(within(own).getByText("You")).toBeTruthy();
  const theirs = within(grid).getByText("Vendor scorecard").closest(".grid-row") as HTMLElement;
  expect(within(theirs).getByText("maya")).toBeTruthy();
  expect(within(theirs).getByText("Can edit")).toBeTruthy();
});

test("Yours draws no owner and no role, which would say You and Owner on every row", () => {
  asList();
  mount("yours", "#/artifacts/yours");
  expect(headers()).toEqual(["Name", "Type", "Edited", ""]);
});

test("the trash says when each went in and when it goes for good, and offers a restore and no pin", () => {
  asList();
  items = [row({ status: "trashed", trashed_at: "2026-10-18T10:00:00Z", title: "Old draft" })];
  mount("yours", "#/artifacts/yours?status=trashed");
  expect(headers()).toEqual(["Name", "Type", "Trashed", "Deleted for good", ""]);
  expect(screen.getByText(/Only you see your trash/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "More for “Old draft”" }));
  expect(screen.queryByRole("menuitem", { name: "Pin to your sidebar" })).toBeNull();
  expect(screen.getByRole("menuitem", { name: "Restore" })).toBeTruthy();
  expect(screen.getByRole("menuitem", { name: "Copy link" })).toBeTruthy();
});

// THE TOGGLE SAYS IT IS ONE: the kit's Button takes `pressed`, and an
// `aria-pressed` passed beside it was overwritten with nothing.
test("the type, the order, the trash and the shape are the toolbar's, and the bar keeps the field and New", () => {
  mount("yours", "#/artifacts/yours");
  const toolbar = document.querySelector(".toolbar") as HTMLElement;
  expect(within(toolbar).getByRole("button", { name: "Trash" }).getAttribute("aria-pressed")).toBe(
    "false",
  );
  expect(within(toolbar).getByRole("radio", { name: "Grid" })).toBeTruthy();
  expect(within(toolbar).getByRole("radio", { name: "List" })).toBeTruthy();
  expect(within(toolbar).queryByRole("searchbox")).toBeNull();
  expect(screen.getByRole("searchbox", { name: "Search artifacts" })).toBeTruthy();
});

test("the trash is Yours alone", () => {
  mount("all");
  expect(screen.queryByRole("button", { name: "Trash" })).toBeNull();
  cleanup();
  mount("shared", "#/artifacts/shared");
  expect(screen.queryByRole("button", { name: "Trash" })).toBeNull();
});

// THE GRID IS THE DEFAULT, and the list a choice this browser remembers: the
// address does not carry it, so a pasted link opens in the reader's own shape.
test("the gallery opens as a grid, and a reader's choice of the list is remembered here", () => {
  mount("all");
  expect(screen.getByRole("radio", { name: "Grid" }).getAttribute("aria-checked")).toBe("true");
  expect(
    screen.getByRole("link", { name: "Architecture notes" }).closest(".artifacts-cards"),
  ).not.toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: "List" }));
  expect(localStorage.getItem(STORAGE_KEYS.artifactsShape)).toBe("list");
  expect(location.hash).not.toContain("shape");
  cleanup();
  mount("all");
  expect(screen.getByRole("radio", { name: "List" }).getAttribute("aria-checked")).toBe("true");
  expect(headers()[0]).toBe("Name");
  fireEvent.click(screen.getByRole("radio", { name: "Grid" }));
  expect(localStorage.getItem(STORAGE_KEYS.artifactsShape)).toBeNull();
});

test("the Make row offers every kind on All and Yours, each opening its kind's own page", () => {
  mount("all");
  const make = screen.getByRole("region", { name: "Make something new" });
  const tiles = within(make).getAllByRole("link");
  expect(tiles.map((t) => t.getAttribute("aria-label"))).toEqual([
    "Docs",
    "Slides",
    "Design",
    "Dashboard",
    "HTML",
    "Sheet",
  ]);
  // WHAT EACH IS FOR is read as the link's description, never run into its name.
  const described = (t: HTMLElement) =>
    document.getElementById(t.getAttribute("aria-describedby") ?? "")?.textContent;
  expect(described(tiles[3]!)).toBe("Live figures");
  expect(tiles[0]!.getAttribute("href")).toBe("#/artifacts/new/doc");
  expect(tiles[5]!.getAttribute("href")).toBe("#/artifacts/new/sheet");
  cleanup();
  mount("yours", "#/artifacts/yours");
  expect(screen.getByRole("region", { name: "Make something new" })).toBeTruthy();
});

// NEW IS A MENU OF THE KINDS, in the Make row's order, each row opening that
// kind's page: there is no generic page that asks for the type first.
test("New opens a menu of every kind, each going to its own create page", () => {
  mount("all");
  fireEvent.click(screen.getByRole("button", { name: "New artifact" }));
  const rows = screen.getAllByRole("menuitem");
  expect(rows.map((r) => r.textContent)).toEqual([
    "Doc",
    "Slides",
    "Design",
    "Dashboard",
    "HTML",
    "Sheet",
  ]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Slides" }));
  expect(location.hash).toBe("#/artifacts/new/slides");
});

test("the Make row steps aside on Shared with you, in the trash and for a search", () => {
  mount("shared", "#/artifacts/shared");
  expect(screen.queryByRole("region", { name: "Make something new" })).toBeNull();
  cleanup();
  mount("yours", "#/artifacts/yours?status=trashed");
  expect(screen.queryByRole("region", { name: "Make something new" })).toBeNull();
  cleanup();
  mount("all", "#/artifacts?q=vendor");
  expect(screen.queryByRole("region", { name: "Make something new" })).toBeNull();
});

test("an earlier gallery's address is rewritten in place to the section it became", () => {
  mount("yours", "#/artifacts/mine?kind=sheet");
  expect(location.hash).toBe("#/artifacts/yours?kind=sheet");
  cleanup();
  mount("all", "#/artifacts/company");
  expect(location.hash).toBe("#/artifacts");
});

test("pins stand above the rest on All, as a group of their own", () => {
  vi.mocked(useQuery).mockImplementation(((what: string, params?: { pinned?: boolean }) => {
    const data =
      what === "artifacts"
        ? { items: params?.pinned ? [items[1]!] : items, total: items.length }
        : null;
    return { data, loading: false, error: null, refusal: null, refetch: vi.fn() };
  }) as never);
  mount("all");
  const pinned = screen.getByRole("region", { name: "Pinned" });
  expect(within(pinned).getByRole("link", { name: "Vendor scorecard" })).toBeTruthy();
  const recent = screen.getByRole("region", { name: "Recent" });
  expect(within(recent).queryByRole("link", { name: "Vendor scorecard" })).toBeNull();
  expect(within(recent).getByRole("link", { name: "Architecture notes" })).toBeTruthy();
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
  mount("all");
  const own = screen.getByRole("link", { name: "Architecture notes" });
  expect(own.textContent).not.toContain("token:review");
  expect(own.textContent).not.toContain(" by ");
  const theirs = screen.getByRole("link", { name: "Vendor scorecard" });
  expect(theirs.textContent).toContain("maya");
  expect(theirs.textContent).toContain("by you");
});

test("a card says who can open it, and its menu stands beside its link", () => {
  items = [
    row({}),
    row({
      id: "a2",
      title: "Company handbook",
      share_count: 1,
      shares: [
        {
          audience: { kind: "company" },
          role: "viewer",
          granted_by: { name: "token:review", kind: "operator" },
          granted_at: "2026-10-18T10:00:00Z",
        },
      ],
    }),
    row({ id: "a3", title: "Team plan", share_count: 2 }),
  ];
  mount("all");
  expect(screen.getByRole("link", { name: "Architecture notes" }).textContent).toContain("Private");
  expect(screen.getByRole("link", { name: "Company handbook" }).textContent).toContain(
    "Shared with the company",
  );
  expect(screen.getByRole("link", { name: "Team plan" }).textContent).toContain("Shared");
  const link = screen.getByRole("link", { name: "Architecture notes" });
  const more = screen.getByRole("button", { name: "More for “Architecture notes”" });
  expect(link.contains(more)).toBe(false);
  fireEvent.click(more);
  for (const name of ["Pin to your sidebar", "Copy link", "Rename", "Move to the trash"]) {
    expect(screen.getByRole("menuitem", { name })).toBeTruthy();
  }
});

test("the grid's placeholder is the card's own shape while the first page loads", () => {
  vi.mocked(useQuery).mockImplementation((() => ({
    data: null,
    loading: true,
    error: null,
    refusal: null,
    refetch: vi.fn(),
  })) as never);
  mount("all");
  expect(document.querySelectorAll(".crewlet-artifact-card--skeleton").length).toBeGreaterThan(0);
  expect(
    screen.getAllByRole("status").filter((s) => s.textContent === "Loading artifacts"),
  ).toHaveLength(1);
});

test("an empty section says what fills it, and a search that found nothing says what it looked for", () => {
  items = [];
  mount("shared", "#/artifacts/shared");
  expect(screen.getByText("Nothing is shared with you")).toBeTruthy();
  cleanup();
  mount("all", "#/artifacts?q=zzz");
  expect(screen.getByText("Nothing matches “zzz”")).toBeTruthy();
});

// IT SAID "Change the phrase or the type" with no type chosen.
test("a search that found nothing names the phrase, and only the change that could find more", () => {
  expect(noMatch("zzz", "")).toEqual({
    title: "Nothing matches “zzz”",
    hint: "Try another phrase.",
  });
  expect(noMatch("zzz", "sheet").hint).toBe(
    "Only Sheet artifacts were searched. Try another phrase, or choose Every type.",
  );
  expect(noMatch("", "design")).toEqual({
    title: "No Design artifacts here",
    hint: "Choose Every type to see the rest.",
  });
});
