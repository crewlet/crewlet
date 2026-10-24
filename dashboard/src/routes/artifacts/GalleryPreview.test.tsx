/**
 * A gallery card's preview: the kind's drawing stands in until the artifact's
 * own content has drawn, what it drew is kept as a still copy with the
 * content unmounted, nothing live is drawn for an artifact in the trash,
 * every content read is a preview that records no open, and a board's
 * preview reads its first row alone.
 */

import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { LivePreviewBudgetProvider } from "@crewlethq/ui";
import { GalleryPreview, previewWidgets, snapshotKey } from "./GalleryPreview.tsx";
import type { ArtifactListItem, DashboardWidget } from "~/protocol/index.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { mintFrame, readBoard, readOutline } from "~/lib/artifacts/api.ts";

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
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: () => ({ socket: { query: vi.fn() } }) };
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

test("on screen, a doc draws its opening once, kept as a still copy with nothing live behind it", async () => {
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
  expect(container.querySelector(".crewlet-live-preview__live")).toBeNull();
  const still = container.querySelector(".crewlet-live-preview__still") as HTMLElement;
  expect(still.hasAttribute("inert")).toBe(true);
  expect(still.closest("[aria-hidden='true']")).not.toBeNull();
  expect(still.querySelector(".artifacts-preview-doc")?.textContent).toContain("On call");
  expect(still.textContent).toContain("Who answers the pager.");
  expect(readOutline).toHaveBeenCalledTimes(1);
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

// A PREVIEW'S FRAME HOLDS AT MOST THE READ-ONLY HOST: a page that declares no
// datastore has nothing on its port; one that does is answered by the
// preview host, which refuses every write, so neither can change anything,
// learn a viewer or join a room.
test("a page's preview frame carries no host, or for a datastore the read-only one", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  const minted = {
    frame_url: "https://p2.artifacts.example.net/t/x/index.html#n=abc",
    origin: "https://p2.artifacts.example.net",
    mode: "per-artifact",
    sandbox: "allow-scripts",
    allow: "",
    nonce: "abc",
    capabilities: [],
  };
  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce(minted as never);
  render(<GalleryPreview item={item({ id: "p2", kind: "html" })} people={people} />);
  await waitFor(() => expect(framed.props).not.toBeNull());
  for (const host of ["onReady", "onMessage", "onOpenLink", "onFocusExit", "onReload"]) {
    expect(framed.props, host).not.toHaveProperty(host);
  }
  cleanup();

  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce({ ...minted, capabilities: ["db"] } as never);
  render(
    <GalleryPreview
      item={item({ id: "p4", kind: "html", capabilities: ["db"] })}
      people={people}
    />,
  );
  await waitFor(() => expect(framed.props).not.toBeNull());
  for (const host of ["onOpenLink", "onFocusExit", "onReload"]) {
    expect(framed.props, host).not.toHaveProperty(host);
  }
  // The port is answered by the preview host: a write is refused.
  const sent: Record<string, unknown>[] = [];
  const port = { postMessage: (m: Record<string, unknown>) => sent.push(m) } as never;
  const onMessage = framed.props!["onMessage"] as (data: unknown, port: MessagePort) => void;
  onMessage({ type: "bridge.call", id: 1, method: "data.write", params: { ops: [] } }, port);
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ ok: false, error: { code: "forbidden" } });
});

// THE FRAME'S GRANT IS WHAT THE PAGE IS TOLD, never the gallery item's
// declaration: the origin tells the page's runtime the grant as it loads, so
// a host telling it more at the handshake answered a page that asked before
// the handshake differently from one that asked after (a sheet said for good
// that its page declared no datastore).
test("the preview host tells the page the frame's grant, never the item's declaration", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  const minted = {
    frame_url: "https://p5.artifacts.example.net/t/x/index.html#n=abc",
    origin: "https://p5.artifacts.example.net",
    mode: "per-artifact",
    sandbox: "allow-scripts",
    allow: "",
    nonce: "abc",
  };
  // An engine that grants a preview no datastore: no host, whatever the item declares.
  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce({ ...minted, capabilities: ["assets"] } as never);
  render(
    <GalleryPreview
      item={item({ id: "p5", kind: "html", capabilities: ["db", "identity", "presence"] })}
      people={people}
    />,
  );
  await waitFor(() => expect(framed.props).not.toBeNull());
  expect(framed.props).not.toHaveProperty("onReady");
  expect(framed.props).not.toHaveProperty("onMessage");
  cleanup();

  // Granted a datastore: the handshake names exactly the grant this host answers.
  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce({
    ...minted,
    capabilities: ["assets", "db"],
  } as never);
  render(
    <GalleryPreview
      item={item({ id: "p6", kind: "html", capabilities: ["db", "identity", "presence"] })}
      people={people}
    />,
  );
  await waitFor(() => expect(framed.props).not.toBeNull());
  const sent: Record<string, unknown>[] = [];
  const port = { postMessage: (m: Record<string, unknown>) => sent.push(m) } as never;
  (framed.props!["onReady"] as (port: MessagePort) => void)(port);
  expect(sent[0]).toMatchObject({ type: "bridge.init", capabilities: ["assets", "db"] });
});

// A PAGE THAT KEEPS EACH VIEWER'S OWN STATE IS DRAWN, as a newcomer sees it:
// it used to stand as its kind's drawing for good, so a dashboard that
// remembered its branch filter (Pipeline health) never had a preview. Its
// frame's host answers the state the page itself set while it lives, and
// never asks the engine for the reader's.
test("a page that keeps each viewer's own state is drawn with a newcomer's state", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  framed.props = null;
  vi.mocked(mintFrame).mockResolvedValueOnce({
    frame_url: "https://p3.artifacts.example.net/t/x/index.html#n=abc",
    origin: "https://p3.artifacts.example.net",
    mode: "per-artifact",
    sandbox: "allow-scripts",
    allow: "",
    nonce: "abc",
    capabilities: ["state"],
  } as never);
  render(
    <GalleryPreview
      item={item({ id: "p3", kind: "html", capabilities: ["db", "identity", "state"] })}
      people={people}
    />,
  );
  await waitFor(() =>
    expect(mintFrame).toHaveBeenCalledWith("p3", 3, undefined, { preview: true }),
  );
  await waitFor(() => expect(framed.props).not.toBeNull());
  const sent: Record<string, unknown>[] = [];
  const port = { postMessage: (m: Record<string, unknown>) => sent.push(m) } as never;
  (framed.props!["onReady"] as (port: MessagePort) => void)(port);
  expect(sent[0]).toMatchObject({ type: "bridge.init", capabilities: ["state"], viewer: null });
  const onMessage = framed.props!["onMessage"] as (data: unknown, port: MessagePort) => void;
  onMessage({ type: "bridge.call", id: 1, method: "state.get", params: { key: "branch" } }, port);
  onMessage(
    { type: "bridge.call", id: 2, method: "state.set", params: { key: "branch", value: "main" } },
    port,
  );
  onMessage({ type: "bridge.call", id: 3, method: "state.get", params: { key: "branch" } }, port);
  await waitFor(() => expect(sent).toHaveLength(4));
  expect(sent[1]).toMatchObject({ id: 1, ok: true, value: null });
  expect(sent[2]).toMatchObject({ id: 2, ok: true });
  expect(sent[3]).toMatchObject({ id: 3, ok: true, value: "main" });
});

test("a card mounted again on the same visit shows its snapshot and reads nothing", async () => {
  vi.stubGlobal("IntersectionObserver", SeenObserver);
  vi.mocked(readBoard).mockClear();
  vi.mocked(readBoard).mockResolvedValue({
    board: {
      title: "Weekly",
      widgets: [{ id: "n", type: "note", w: 12, h: 4, text: "Shipped on time." }],
    },
    datasets: [],
  } as never);
  const board = item({ id: "b1", kind: "dashboard", head_number: 2 });
  const gallery = (shown: ArtifactListItem | null) => (
    <LivePreviewBudgetProvider>
      {shown && <GalleryPreview item={shown} people={people} />}
    </LivePreviewBudgetProvider>
  );
  const { container, rerender } = render(gallery(board));
  await waitFor(() =>
    expect(container.querySelector(".crewlet-live-preview")?.getAttribute("data-state")).toBe(
      "ready",
    ),
  );
  expect(container.querySelector(".crewlet-live-preview__still")?.textContent).toContain(
    "Shipped on time.",
  );
  rerender(gallery(null));
  rerender(gallery(board));
  expect(container.querySelector(".crewlet-live-preview")?.getAttribute("data-state")).toBe(
    "ready",
  );
  expect(container.querySelector(".crewlet-live-preview__still")?.textContent).toContain(
    "Shipped on time.",
  );
  expect(readBoard).toHaveBeenCalledTimes(1);
  expect(snapshotKey(board)).not.toBe(snapshotKey({ ...board, head_number: 3 }));
  rerender(gallery({ ...board, head_number: 3 }));
  await waitFor(() => expect(readBoard).toHaveBeenCalledTimes(2));
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
