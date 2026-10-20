/**
 * A gallery card's live preview: the kind's drawing stands in until the
 * artifact's own content has drawn, nothing live is drawn for an artifact in
 * the trash, every content read is a preview that records no open, and a
 * board's preview reads its first row alone.
 */

import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { GalleryPreview, previewWidgets, previewable } from "./GalleryPreview.tsx";
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
/** The props the last preview frame was mounted with. */
const framed: { props: Record<string, unknown> | null } = { props: null };
vi.mock("@crewlethq/ui", async () => {
  const actual = await vi.importActual<typeof import("@crewlethq/ui")>("@crewlethq/ui");
  return {
    ...actual,
    SandboxedFrame: (props: Record<string, unknown>) => {
      framed.props = props;
      return null;
    },
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
    capabilities: [],
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

// A PREVIEW'S FRAME RUNS WITH NO HOST: nothing is attached to its port, so a
// page in it can call no datastore, learn no viewer and join no room; it is
// mounted only to draw, and only its drawn state is heard.
test("a page's preview frame is mounted with no host on its port", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce({
    frame_url: "https://p2.artifacts.example.net/t/x/index.html#n=abc",
    origin: "https://p2.artifacts.example.net",
    mode: "per-artifact",
    sandbox: "allow-scripts",
    allow: "",
    nonce: "abc",
  } as never);
  render(<GalleryPreview item={item({ id: "p2", kind: "html" })} people={people} />);
  await waitFor(() => expect(framed.props).not.toBeNull());
  for (const host of ["onReady", "onMessage", "onOpenLink", "onFocusExit", "onReload"]) {
    expect(framed.props, host).not.toHaveProperty(host);
  }
  expect(framed.props).toHaveProperty("onStateChange");
});

test("a page that is an app over its people's data shows the kind's drawing, and reads nothing", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  expect(previewable({ kind: "html", capabilities: ["db", "identity", "presence"] })).toBe(false);
  expect(previewable({ kind: "html", capabilities: ["state", "assets"] })).toBe(false);
  expect(previewable({ kind: "html", capabilities: ["db"] })).toBe(true);
  expect(previewable({ kind: "html", capabilities: ["assets"] })).toBe(true);
  expect(previewable({ kind: "sheet", capabilities: [] })).toBe(true);
  const { container } = render(
    <GalleryPreview
      item={item({ id: "p3", kind: "html", capabilities: ["db", "presence"] })}
      people={people}
    />,
  );
  await act(async () => {});
  expect(container.querySelector(".crewlet-kind-ground.crewlet-kind--html")).not.toBeNull();
  expect(container.querySelector(".crewlet-live-preview__live")).toBeNull();
  expect(mintFrame).not.toHaveBeenCalled();
});

const widget = (id: string, w: number, more: Partial<DashboardWidget> = {}): DashboardWidget => ({
  id,
  type: "stat",
  w,
  ...more,
});

const ids = (drawn: { widget: DashboardWidget }[]) => drawn.map((d) => d.widget.id);

// THE BOARD FILLS ITS CARD: rows from the top until they reach the box's
// height, a group's children in its place, within the dataset budget.
test("a board's preview draws rows from the top until they fill the card", () => {
  const stats = ["a", "b", "c", "d"].map((id) => widget(id, 3, { h: 6, dataset: id }));
  // Four stats (96px) and a chart (192px) fill the box; the table under them does not fit.
  const chart = widget("chart", 12, { type: "timeseries", h: 12, dataset: "series" });
  const table = widget("table", 12, { type: "table", h: 10, dataset: "rows" });
  expect(ids(previewWidgets([...stats, chart, table]))).toEqual(["a", "b", "c", "d", "chart"]);
  expect(ids(previewWidgets([widget("g", 12, { type: "group", children: stats }), chart]))).toEqual(
    ["a", "b", "c", "d", "chart"],
  );
});

test("a board's preview reads at most six datasets", () => {
  const many = Array.from({ length: 12 }, (_, i) => widget(`w${i}`, 2, { h: 2, dataset: `d${i}` }));
  const drawn = previewWidgets(many, 10_000);
  expect(new Set(drawn.map((d) => d.widget.dataset)).size).toBe(6);
  // A widget on a dataset already read costs nothing more.
  const shared = Array.from({ length: 8 }, (_, i) => widget(`s${i}`, 3, { h: 2, dataset: "one" }));
  expect(previewWidgets(shared, 10_000)).toHaveLength(8);
});

test("a widget too narrow to read at the preview's width is widened", () => {
  const drawn = previewWidgets([widget("tiny", 1, { h: 4 }), widget("wide", 6, { h: 4 })]);
  expect(drawn[0]!.span).toBeGreaterThan(1);
  expect(drawn[1]!.span).toBe(6);
});
