/**
 * A gallery card's preview: the artifact's own content, drawn live and small
 * by the same renderer its page uses, never a screenshot and never a generic
 * cover.
 *
 * # What each kind draws
 *
 * - doc: the opening of its first tab (the version's outline holds it), set
 *   by the design system's `Document` as the doc's page sets it.
 * - slides: its cover slide on the design system's `SlideStage`.
 * - sheet: its first rows and columns in the `SheetGrid`, read only, read as
 *   the reader through the datastore's own questions.
 * - dashboard: its board's first row of widgets with their real figures,
 *   each dataset read as the reader; the rest of the board is not read.
 * - design: its cover board, framed at the board's own size and filling the
 *   card edge to edge (`PreviewScale`'s `fill`): a laptop's board loses a
 *   sliver of its foot rather than leaving an empty strip down each side, and
 *   a phone's board, which filling would cut to its top, is shown whole.
 * - html: its page, framed at a desktop width.
 * - brand: a sample doc drawn in the kit's colours (the version's outline
 *   holds its `brand.json`), as the kit's own page and Company › Brand
 *   preview it, with no logo or face read, as a doc's cover reads no image.
 *
 * # What it costs
 *
 * Every preview is a design system `LivePreview`, and it is a SNAPSHOT: the
 * content is rendered once, out of sight, when the card comes near the
 * viewport and the gallery's budget has a render free (a few at once); once
 * it has drawn, what it drew is kept as a still copy and the content is
 * unmounted, so a card holds no query, subscription, poll or timer after it,
 * and scrolling past it again reads and draws nothing. The kind's drawing
 * stands in until then (never yet near, waiting for a render, still loading,
 * failed or empty). The copy is kept by the VERSION it drew
 * (`snapshotKey`), so a card mounted again on this visit (a filter, the
 * list's shape, a page of results) shows it at once, a new version is drawn
 * afresh, and a page loaded afresh takes new snapshots. Every read is made
 * once and never again on a reconnect; a version's outline never changes, so
 * each is read once per tab (`outlines`). Every read is a PREVIEW
 * (`ReadOptions`): the engine records no open, so browsing a gallery never
 * fills an artifact's Seen by list or reorders the reader's Recently opened.
 *
 * # A frame is kept, not copied
 *
 * A page and a design's board run in a frame (`FRAMED`): another document,
 * which nothing in this one can copy, and which a copy of the frame would
 * load again from the start. So a framed preview keeps its frame once it has
 * loaded, and the budget caps the frames it keeps. What it may still do is
 * bounded instead: its ticket is minted once (never again on a reconnect,
 * which would reload the page), and the read-only preview host answers each
 * distinct read once and every repeat from that answer, so a page that polls
 * its datastore is drawn as it stood and asks the engine nothing more. A
 * picture of the page posted by the page itself was weighed and set aside:
 * rasterising a document inside the frame is a renderer of its own that
 * misses cross-origin images and fonts, and handing its markup out to draw
 * here would put the artifact's own HTML in the dashboard's document.
 *
 * # Never a control
 *
 * The live layer is inert and hidden from assistive technology: a frame is
 * never focused and takes no pointer, and the card's link names the artifact.
 */

import { useEffect, useId, useMemo, useState } from "react";
import {
  ArrangeBoard,
  ArrangeFrame,
  ArrangeGrid,
  ArtifactKindGround,
  Document,
  LivePreview,
  PreviewScale,
  SandboxedFrame,
  SheetGrid,
  SlideStage,
  useLivePreview,
} from "@crewlethq/ui";
import type { ThemeId } from "@crewlethq/tokens";
import type {
  ArtifactFrame,
  ArtifactKind,
  ArtifactListItem,
  ArtifactOutline,
  DashboardBoardAnswer,
  DashboardDatasetAnswer,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";
import { DASHBOARD_COLUMNS } from "~/contract/artifacts.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { useDensity, usePalette, useTheme } from "~/lib/prefs.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { renderSlide } from "~/lib/slides.tsx";
import { mintFrame, readBoard, readDataset, readOutline } from "~/lib/artifacts/api.ts";
import { NO_BRAND_ASSETS } from "~/lib/brand/useBrand.ts";
import { BrandSampleOf } from "~/routes/brand/BrandPreview.tsx";
import { boardSpan, datasetStatus } from "~/lib/artifacts/dashboard.ts";
import { sheetColumns, sheetRows } from "~/lib/artifacts/sheet.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { DashboardWidget } from "./DashboardWidget.tsx";
import { PreviewHost, hostAnswers } from "./previewHost.ts";
import { sheetCollections } from "./SheetView.tsx";

/** The width a board's preview is laid out at: a laptop's half, so its widgets fill a card's box. */
const BOARD_WIDTH = 680;

/** The board's own margin in the preview, and the gap between its widgets, in pixels. */
const BOARD_INSET = 24;
const BOARD_GAP = 12;

/**
 * The most datasets a board's preview reads: a gallery card is a glimpse, and
 * six reads cover the stats and the chart a board opens on without a gallery
 * of boards spending the engine's read budget.
 */
const BOARD_DATASETS = 6;

/**
 * The width each kind is laid out at before it is scaled to the card, chosen
 * so what it says reads at a card's size: a doc at a narrow page with its
 * margins, as the top of a page reads across a room; a deck's cover at half
 * its authored stage, which the stage fits whole; a sheet at a desktop
 * width; a board at [BOARD_WIDTH]; a page at a small laptop's, where a page
 * laid out for a wide screen still wraps into lines a card can show. A
 * design's board is laid out at its own size. A width is also what lets a
 * still copy follow its card when the card changes size: it is scaled again
 * rather than drawn again.
 */
const LAYOUT_WIDTH: Readonly<Record<ArtifactKind, number | undefined>> = {
  doc: 600,
  slides: 640,
  sheet: 880,
  dashboard: BOARD_WIDTH,
  design: undefined,
  html: 880,
  brand: 600,
};

/** The kinds whose preview runs the artifact's page in a frame. */
const FRAMED: ReadonlySet<ArtifactKind> = new Set(["html", "design"]);

/** How many rows of a sheet are read and drawn: more than the card shows at any width. */
const SHEET_ROWS = 14;

/** How many columns of a sheet are drawn. */
const SHEET_COLUMNS = 8;

/**
 * A preview's reads are made once: it is a snapshot, so an answer is not
 * read again when the socket comes back.
 */
const ONCE = { refetchOnReconnect: false } as const;

/** The board's row height, in pixels: the dashboard page's own (`DashboardView`). */
const BOARD_ROW = 16;

export function GalleryPreview({
  item,
  people,
}: {
  item: ArtifactListItem;
  people: ArtifactPeople;
}) {
  const fallback = <ArtifactKindGround kind={item.kind} />;
  // NOTHING LIVE IN THE TRASH: a trashed artifact's page refuses a frame, and
  // its card is a way to restore it rather than to read it.
  const still = item.status === "trashed" || (item.kind !== "sheet" && item.head_number <= 0);
  return (
    <LivePreview
      fallback={fallback}
      width={LAYOUT_WIDTH[item.kind]}
      disabled={still}
      frame={FRAMED.has(item.kind)}
      snapshot={snapshotKey(item)}
    >
      <KindPreview item={item} people={people} />
    </LivePreview>
  );
}

/**
 * What a preview's snapshot is of: the artifact at its version. A sheet's
 * rows move without a version, so its snapshot is of the rows as this visit
 * to the gallery first read them.
 */
export function snapshotKey(item: Pick<ArtifactListItem, "id" | "head_number">): string {
  return `${item.id}:${item.head_number}`;
}

function KindPreview({ item, people }: { item: ArtifactListItem; people: ArtifactPeople }) {
  switch (item.kind) {
    case "doc":
      return <DocPreview item={item} />;
    case "slides":
      return <SlidesPreview item={item} />;
    case "sheet":
      return <SheetPreview item={item} people={people} />;
    case "dashboard":
      return <DashboardPreview item={item} />;
    case "design":
      return <DesignPreview item={item} />;
    case "html":
      return <PagePreview item={item} />;
    case "brand":
      return <BrandKitPreview item={item} />;
  }
}

/**
 * A version's outline, read once per tab: a version never changes, so a card
 * drawn again (the gallery opened again in this tab) draws from what was
 * read. Bounded, the oldest
 * forgotten first, so a long session holds a gallery's worth and no more.
 */
const outlines = new Map<string, Promise<ArtifactOutline>>();
const OUTLINES_KEPT = 300;

function outlineOf(id: string, version: number): Promise<ArtifactOutline> {
  const key = `${id}:${version}`;
  const held = outlines.get(key);
  if (held) return held;
  // THE READ OUTLIVES THE CARD that asked for it, so a card unmounted
  // mid-read leaves the next mount a read to reuse rather than an aborted
  // one; a failed read is forgotten, so it is asked again.
  const read = readOutline(id, version, new AbortController().signal);
  read.catch(() => outlines.delete(key));
  outlines.set(key, read);
  if (outlines.size > OUTLINES_KEPT) outlines.delete(outlines.keys().next().value!);
  return read;
}

function useOutline(item: Pick<ArtifactListItem, "id" | "head_number">) {
  return useRest<ArtifactOutline>(
    `gallery-outline:${item.id}:${item.head_number}`,
    () => outlineOf(item.id, item.head_number),
    ONCE,
  );
}

/**
 * Tells the preview around it once the content has drawn, or that it has
 * nothing to draw: `drawn` true is ready, false is the stand-in for good,
 * and undefined is still loading.
 */
function useSettled(drawn: boolean | undefined) {
  const preview = useLivePreview();
  useEffect(() => {
    if (drawn === true) preview.ready();
    else if (drawn === false) preview.fail();
  }, [drawn, preview]);
}

const noImages = () => null;

function DocPreview({ item }: { item: ArtifactListItem }) {
  const outline = useOutline(item);
  const markdown = outline.data?.doc?.cover?.markdown ?? "";
  useSettled(outline.error ? false : outline.data ? markdown.trim() !== "" : undefined);
  const body = useMemo(
    () => (markdown ? renderMarkdown(markdown, { image: noImages, links: "text" }) : null),
    [markdown],
  );
  if (!body) return null;
  return (
    <div className="artifacts-preview-doc">
      <Document measure="none">{body}</Document>
    </div>
  );
}

function SlidesPreview({ item }: { item: ArtifactListItem }) {
  const outline = useOutline(item);
  const prefix = useId().replace(/:/g, "");
  const slides = outline.data?.slides;
  useSettled(outline.error ? false : outline.data ? slides !== undefined : undefined);
  if (!slides) return null;
  return (
    <div className="artifacts-preview-slide">
      <SlideStage label={`${item.title}, cover`} fit="contain">
        {renderSlide(slides.cover_slide, { prefix, image: noImages, links: "text" })}
      </SlideStage>
    </div>
  );
}

/** A brand kit's cover: the sample doc its own page previews, in light, in the kit's colours. */
function BrandKitPreview({ item }: { item: ArtifactListItem }) {
  const outline = useOutline(item);
  const doc = outline.data?.brand?.document;
  useSettled(outline.error ? false : outline.data ? doc !== undefined : undefined);
  if (!doc) return null;
  return (
    <div className="artifacts-preview-brand">
      <BrandSampleOf document={doc} assets={NO_BRAND_ASSETS} kind="doc" mode="light" />
    </div>
  );
}

function SheetPreview({ item, people }: { item: ArtifactListItem; people: ArtifactPeople }) {
  const schema = useQuery("artifact_data_schema", { id: item.id }, ONCE);
  const collection = schema.data ? sheetCollections(schema.data.collections)[0] : undefined;
  const docs = useQuery(
    "artifact_data",
    { id: item.id, collection: collection?.name ?? "", limit: SHEET_ROWS },
    { ...ONCE, enabled: collection !== undefined },
  );
  const drawn = useMemo(() => {
    if (!collection || !schema.data || !docs.data) return null;
    // A HIDDEN COLUMN IS LEFT OUT by [sheetColumns], as the sheet leaves it out.
    const columns = sheetColumns(collection, schema.data.role).slice(0, SHEET_COLUMNS);
    const rows = sheetRows(
      docs.data.docs
        .filter((d) => !d.deleted && d.fields)
        .map((d) => ({ doc: d, fields: d.fields ?? {} })),
    ).slice(0, SHEET_ROWS);
    return { columns, rows };
  }, [collection, schema.data, docs.data]);
  const settled =
    schema.error || docs.error
      ? false
      : schema.data && !collection
        ? false
        : drawn
          ? drawn.columns.length > 0
          : undefined;
  useSettled(settled);
  if (!drawn || drawn.columns.length === 0 || !collection) return null;
  return (
    <div className="artifacts-preview-sheet">
      <SheetGrid
        label={collection.title || item.title}
        fit="content"
        mode="read"
        columns={drawn.columns}
        rows={drawn.rows}
        people={people.sheetPeople}
      />
    </div>
  );
}

/** A widget the preview draws, with the columns it spans at the preview's width. */
export interface PreviewWidget {
  widget: Widget;
  span: number;
}

/**
 * The widgets a board's preview draws: the board from its top, a group's
 * children in its place, each spanning what it spans at [BOARD_WIDTH]
 * ([boardSpan] widens one too narrow to read), row after row until the rows
 * fill the card's box or the next widget would read a dataset past
 * [BOARD_DATASETS]. A board that opens on a row of four stats and a chart
 * draws both, rather than the stats alone over an empty ground.
 */
export function previewWidgets(
  widgets: readonly Widget[],
  height = (BOARD_WIDTH * 9) / 16 - 2 * BOARD_INSET,
): PreviewWidget[] {
  const flat = widgets.flatMap((w) => (w.type === "group" ? (w.children ?? []) : [w]));
  const out: PreviewWidget[] = [];
  const datasets = new Set<string>();
  let used = 0;
  let rowHeight = 0;
  let filled = 0;
  for (const widget of flat) {
    if (widget.type === "group") continue;
    if (widget.dataset && !datasets.has(widget.dataset) && datasets.size >= BOARD_DATASETS) break;
    const span = boardSpan(widget.w, BOARD_WIDTH - 2 * BOARD_INSET, BOARD_GAP, DASHBOARD_COLUMNS);
    if (used + span > DASHBOARD_COLUMNS) {
      filled += rowHeight + BOARD_GAP;
      used = 0;
      rowHeight = 0;
      // A ROW THE BOX WOULD SHOW ONLY A SLIVER OF is not read: its
      // datasets would cost reads for a strip of chrome at the card's foot.
      if (filled + ROW_SHOWN > height) break;
    }
    out.push({ widget, span });
    if (widget.dataset) datasets.add(widget.dataset);
    used += span;
    rowHeight = Math.max(rowHeight, (widget.h ?? DEFAULT_ROWS) * BOARD_ROW);
  }
  return out;
}

/** The least of a row the box must show for the preview to start it, in pixels. */
const ROW_SHOWN = 64;

/** The rows a widget with no height of its own is drawn at in a preview. */
const DEFAULT_ROWS = 6;

function DashboardPreview({ item }: { item: ArtifactListItem }) {
  const board = useRest<DashboardBoardAnswer>(
    `gallery-board:${item.id}:${item.head_number}`,
    (signal) => readBoard(item.id, item.head_number, signal, { preview: true }),
    ONCE,
  );
  const answer = board.data;
  const drawn = useMemo(() => (answer ? previewWidgets(answer.board.widgets) : []), [answer]);
  const row = useMemo(() => drawn.map((d) => d.widget), [drawn]);
  const [figures, setFigures] = useState<ReadonlyMap<string, DashboardDatasetAnswer | null>>(
    new Map(),
  );
  // EACH DATASET OF THE ROW, read once as the reader with the board's
  // filters at their defaults. A failed read is drawn as the widget's own
  // failure, as the board draws it.
  useEffect(() => {
    if (!answer) return undefined;
    const params: Record<string, string> = {};
    for (const p of answer.board.params ?? []) params[p.key] = p.default;
    const wanted = [...new Set(row.flatMap((w) => (w.dataset ? [w.dataset] : [])))];
    const controller = new AbortController();
    for (const dataset of wanted) {
      readDataset(item.id, item.head_number, dataset, params, controller.signal).then(
        (got) => setFigures((was) => new Map(was).set(dataset, got)),
        () => {
          if (!controller.signal.aborted) setFigures((was) => new Map(was).set(dataset, null));
        },
      );
    }
    return () => controller.abort();
  }, [answer, row, item.id, item.head_number]);
  const waiting = row.some((w) => w.dataset && !figures.has(w.dataset));
  useSettled(
    board.error
      ? false
      : answer
        ? row.length === 0
          ? false
          : waiting
            ? undefined
            : true
        : undefined,
  );
  if (!answer || row.length === 0) return null;
  const byId = new Map(answer.datasets.map((d) => [d.id, d]));
  const now = Date.now();
  return (
    <div className="artifacts-preview-board">
      <ArrangeBoard editing={false} columns={DASHBOARD_COLUMNS}>
        <ArrangeGrid>
          {drawn.map(({ widget: w, span }) => {
            const d = w.dataset ? byId.get(w.dataset) : undefined;
            const got = w.dataset ? (figures.get(w.dataset) ?? null) : null;
            const failed = w.dataset !== undefined && figures.get(w.dataset) === null;
            const state = d
              ? {
                  answer: got,
                  loading: !figures.has(d.id),
                  failure: failed ? "This dataset could not be read right now." : null,
                  received: now,
                }
              : undefined;
            return (
              <ArrangeFrame
                key={w.id}
                name={w.title ?? w.id}
                editing={false}
                bare={w.type === "note"}
                columns={span}
                rows={w.h ?? null}
                rowHeight={BOARD_ROW}
                className="dashboard-cell"
              >
                <DashboardWidget
                  boardTitle={answer.board.title ?? item.title}
                  widget={w}
                  dataset={d}
                  answer={got}
                  status={d ? datasetStatus(d, state, now) : "loading"}
                  detail={state?.failure ?? got?.detail}
                  asOf={got?.as_of}
                  onCite={undefined}
                  onFocus={() => {}}
                  inRow
                />
              </ArrangeFrame>
            );
          })}
        </ArrangeGrid>
      </ArrangeBoard>
    </div>
  );
}

function DesignPreview({ item }: { item: ArtifactListItem }) {
  const outline = useOutline(item);
  const design = outline.data?.design;
  const cover = design?.boards.find((b) => b.path === design.cover) ?? design?.boards[0];
  const preview = useLivePreview();
  useEffect(() => {
    if (outline.error || (outline.data && !cover)) preview.fail();
  }, [outline.error, outline.data, cover, preview]);
  if (!cover) return null;
  return (
    <PreviewScale width={cover.w} height={cover.h} fit="fill">
      <PreviewFrame item={item} document={cover.path} title={cover.title || item.title} />
    </PreviewScale>
  );
}

function PagePreview({ item }: { item: ArtifactListItem }) {
  return <PreviewFrame item={item} title={item.title} />;
}

/**
 * A page framed for a preview: a ticket minted as a preview (no open is
 * recorded, and it grants only what drawing needs), the design system's frame
 * embedding it exactly as minted, and on its port at most the READ-ONLY
 * preview host (`previewHost.ts`): a page whose frame is granted a datastore
 * is answered its reads, so it draws its real content; a page that keeps each
 * viewer's own state is handed a newcomer's, empty and held by the frame
 * alone, so it draws as somebody opening it for the first time sees it; and
 * every other call is refused. A page granted none of these has nothing on
 * its port at all.
 *
 * THE HOST IS BUILT FROM THE MINTED FRAME'S GRANT, never from the gallery
 * item: the origin tells the page's runtime that grant as it loads, so a
 * host that told it the item's declaration at the handshake (more than an
 * engine granting a preview no datastore, say) answered a page that asked
 * after the handshake differently from one that asked before it.
 */
function PreviewFrame({
  item,
  document,
  title,
}: {
  item: ArtifactListItem;
  document?: string;
  title: string;
}) {
  const frame = useRest<ArtifactFrame>(
    `gallery-frame:${item.id}:${item.head_number}:${document ?? ""}`,
    () => mintFrame(item.id, item.head_number, document, { preview: true }),
    ONCE,
  );
  const { socket } = useClient();
  const minted = frame.data;
  const host = useMemo(
    () =>
      minted && hostAnswers(minted.capabilities)
        ? new PreviewHost({ artifact: item.id, socket, granted: minted.capabilities })
        : null,
    [minted, item.id, socket],
  );
  const [mode] = useTheme();
  const [palette] = usePalette();
  const [density] = useDensity();
  const theme = useMemo(
    () => ({ mode, palette: palette as ThemeId, density }),
    [mode, palette, density],
  );
  const preview = useLivePreview();
  useEffect(() => {
    if (frame.error) preview.fail();
  }, [frame.error, preview]);
  if (!minted) return null;
  return (
    <div className="artifacts-preview-frame">
      <SandboxedFrame
        src={minted.frame_url}
        origin={minted.origin}
        mode={minted.mode}
        sandbox={minted.sandbox}
        allow={minted.allow}
        title={title}
        theme={theme}
        nonce={minted.nonce}
        {...(host
          ? {
              onReady: (port: MessagePort) => host.attach(port),
              onMessage: (data: unknown, port: MessagePort) => void host.receive(data, port),
            }
          : {})}
        onStateChange={(next) => {
          if (next === "ready") preview.ready();
          else if (next !== "loading") preview.fail();
        }}
      />
    </div>
  );
}
