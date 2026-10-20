/**
 * A gallery card's live preview: the kind's drawing stands in until the
 * artifact's own content has drawn, nothing live is drawn for an artifact in
 * the trash, every content read is a preview that records no open, and a
 * board's preview reads its first row alone.
 */

import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { GalleryPreview, firstRow } from "./GalleryPreview.tsx";
import type { ArtifactListItem, DashboardWidget } from "~/protocol/index.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { mintFrame, readOutline } from "~/lib/artifacts/api.ts";

vi.mock("~/lib/artifacts/api.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/artifacts/api.ts")>("~/lib/artifacts/api.ts");
  return {
    ...actual,
    readOutline: vi.fn(),
    mintFrame: vi.fn(() => new Promise(() => {})),
    readBoard: vi.fn(() => new Promise(() => {})),
  };
});
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({ data: null, loading: true, error: null, refusal: null, refetch: () => {} }),
}));

const people = { sheetPeople: [] } as unknown as ArtifactPeople;

function item(over: Partial<ArtifactListItem>): ArtifactListItem {
  return {
    id: "a1",
    kind: "doc",
    title: "Runbook",
    head_number: 3,
    status: "live",
    ...over,
  } as ArtifactListItem;
}

/** An observer that reports every box it watches as on screen, at once. */
class SeenObserver {
  constructor(private readonly report: IntersectionObserverCallback) {}
  observe(target: Element) {
    this.report([{ isIntersecting: true, target } as IntersectionObserverEntry], this as never);
  }
  disconnect() {}
  unobserve() {}
  takeRecords() {
    return [];
  }
}

beforeEach(() => {
  vi.mocked(readOutline).mockReset();
  vi.mocked(mintFrame).mockClear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("with no observer, every kind draws its own stand-in and reads nothing", () => {
  const drawings = new Set<string>();
  for (const kind of ["doc", "slides", "sheet", "dashboard", "design", "html"] as const) {
    const { container, unmount } = render(
      <GalleryPreview item={item({ id: kind, kind })} people={people} />,
    );
    const ground = container.querySelector(`.crewlet-kind-ground.crewlet-kind--${kind} svg`);
    expect(ground, kind).not.toBeNull();
    drawings.add(ground!.innerHTML);
    expect(container.querySelector(".crewlet-live-preview__live")).toBeNull();
    unmount();
  }
  expect(drawings.size).toBe(6);
  expect(readOutline).not.toHaveBeenCalled();
});

test("on screen, a doc draws its opening and the stand-in gives way to it", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  vi.mocked(readOutline).mockResolvedValue({
    artifact: "d1",
    kind: "doc",
    version: 3,
    doc: { tabs: [], cover: { tab: "index.md", markdown: "# On call\n\nWho answers the pager." } },
  });
  const { container } = render(<GalleryPreview item={item({ id: "d1" })} people={people} />);
  await waitFor(() =>
    expect(container.querySelector(".crewlet-live-preview")?.getAttribute("data-state")).toBe(
      "ready",
    ),
  );
  const live = container.querySelector(".crewlet-live-preview__live") as HTMLElement;
  expect(live.hasAttribute("inert")).toBe(true);
  expect(live.closest("[aria-hidden='true']")).not.toBeNull();
  expect(live.querySelector(".artifacts-preview-doc")?.textContent).toContain("On call");
  expect(live.textContent).toContain("Who answers the pager.");
});

test("a doc with nothing to draw keeps its stand-in", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  vi.mocked(readOutline).mockResolvedValue({
    artifact: "d2",
    kind: "doc",
    version: 3,
    doc: { tabs: [], cover: { tab: "index.md", markdown: "" } },
  });
  const { container } = render(<GalleryPreview item={item({ id: "d2" })} people={people} />);
  await waitFor(() =>
    expect(container.querySelector(".crewlet-live-preview")?.getAttribute("data-state")).toBe(
      "failed",
    ),
  );
  expect(container.querySelector(".crewlet-live-preview__live")).toBeNull();
});

test("a page is framed as a preview, which records no open", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  render(<GalleryPreview item={item({ id: "p1", kind: "html" })} people={people} />);
  await waitFor(() =>
    expect(mintFrame).toHaveBeenCalledWith("p1", 3, undefined, { preview: true }),
  );
});

test("nothing live is drawn for an artifact in the trash", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  const { container } = render(
    <GalleryPreview item={item({ id: "t1", kind: "html", status: "trashed" })} people={people} />,
  );
  await act(async () => {});
  expect(container.querySelector(".crewlet-live-preview__live")).toBeNull();
  expect(mintFrame).not.toHaveBeenCalled();
});

const widget = (id: string, w: number, more: Partial<DashboardWidget> = {}): DashboardWidget => ({
  id,
  type: "stat",
  w,
  ...more,
});

test("a board's first row fills its twelve columns from the top, a leading group's children standing in", () => {
  expect(
    firstRow([widget("a", 3), widget("b", 3), widget("c", 6), widget("d", 4)]).map((w) => w.id),
  ).toEqual(["a", "b", "c"]);
  expect(firstRow([widget("a", 8), widget("b", 6)]).map((w) => w.id)).toEqual(["a"]);
  expect(firstRow([widget("a", 12), widget("b", 3)]).map((w) => w.id)).toEqual(["a"]);
  expect(
    firstRow([
      widget("g", 12, { type: "group", children: [widget("x", 6), widget("y", 6)] }),
      widget("z", 3),
    ]).map((w) => w.id),
  ).toEqual(["x", "y"]);
  expect(firstRow([widget("a", 4), widget("g", 12, { type: "group" })]).map((w) => w.id)).toEqual([
    "a",
  ]);
});
