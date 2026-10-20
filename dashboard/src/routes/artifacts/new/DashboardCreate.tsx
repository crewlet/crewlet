/**
 * New dashboard: a board a person composes from the company's own data, data
 * first, in the shared frame (`CreateShell`).
 *
 * # Data first, figures shown, not imagined
 *
 * The Data panel lists the engine's source catalogue in a person's words.
 * Opening a source reads its rows AS THIS READER (`usePreviews`) and draws
 * the figures it suggests with the board's own widget (`DashboardWidget`),
 * live, so a person adds a figure they can already read. A source the reader
 * may not read says so in the engine's words and offers nothing to add. The
 * board beside it is the real grid, arranged as Home is: moved by drag or the
 * arrow keys, sized from its edge, taken off from its toolbar. An empty board
 * offers the ready-made boards to start from.
 *
 * # Creating is publishing a spec
 *
 * Create composes `dashboard.json` from the closed choices made here
 * (`composeSpec`), brings it in an upload and creates the artifact with it as
 * the first version, one keyed write. The engine holds the spec to the board
 * format at publish; a refusal is said where the reader is looking and keeps
 * the draft. A retry after an answer nobody could confirm sends the same
 * upload with the same key, so it lands once.
 */

import { useMemo, useRef, useState } from "react";
import {
  ArrangeBoard,
  ArrangeFrame,
  ArrangeGrid,
  ArtifactKindGlyph,
  Button,
  Callout,
  Card,
  DatasetSource,
  DatasetSourceList,
  EmptyState,
  FormField,
  IconButton,
  Input,
  Modal,
  SegmentedControl,
  Select,
  Skeleton,
  VisuallyHidden,
  useReorder,
  useToast,
} from "@crewlethq/ui";
import { CheckGlyph, PlusGlyph, RefreshCwGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { DASHBOARD_COLUMNS } from "~/contract/artifacts.ts";
import { createFromUpload, readCatalogue, uploadTexts } from "~/lib/artifacts/api.ts";
import { refreshWords } from "~/lib/artifacts/dashboard.ts";
import { useKeyChords } from "~/lib/keys.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { plural } from "~/lib/format.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { useRest } from "~/lib/useRest.ts";
import {
  newActOpID,
  type DashboardBoardDataset,
  type DashboardEngineSource,
} from "~/protocol/index.ts";
import { DashboardWidget } from "../DashboardWidget.tsx";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useWorkItem } from "./create.ts";
import {
  EMPTY_DRAFT,
  SPEC_PATH,
  STARTERS,
  composeSpec,
  datasetOf,
  fromStarter,
  hasRoom,
  place,
  recipesOf,
  reorder as reorderDraft,
  resize,
  resolve,
  setSetting,
  settingValue,
  settingsOf,
  shapeOf,
  sourceWords,
  starterSources,
  tooManyDatasets,
  unplace,
  type Draft,
  type DraftDataset,
  type Recipe,
  type Starter,
} from "./dashboard/model.ts";
import { previewRows, previewStatus, usePreviews, type Previews } from "./dashboard/usePreviews.ts";

/** One board row's height in pixels: the board's own (`DashboardView`). */
const ROW = 16;

/** Below this width the board draws every figure across the page, as a published board does. */
const STACKS_BELOW = 720;

/** Below this width the board and the Data panel are two tabs rather than side by side. */
const NARROW_BELOW = 1000;

/** The one tab panel the narrow page's tabs control. */
const PANE = "dashboard-create-pane";

/** The narrowest a figure is sized to on the draft board, in columns. */
const MIN_COLUMNS = 2;

/** The rail's suggestions and the board's figures, drawn by the board's own widget. */
function boardDataset(d: DraftDataset): DashboardBoardDataset {
  return {
    id: d.key,
    title: d.title,
    source: "engine",
    shape: shapeOf(d.source),
    freshness: d.source.signal ? { mode: "signal", signal: d.source.signal } : { mode: "manual" },
    engine: d.source,
  };
}

export function DashboardCreate() {
  const nav = useNavigator();
  const toast = useToast();
  const leave = useLeaveCreate();
  const workItem = useWorkItem();
  const [title, setTitle] = useState("");
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT);
  const [open, setOpen] = useState<string | null>(null);
  const [fresh, setFresh] = useState<string | null>(null);
  const [confirmLeave, setConfirmLeave] = useState(false);
  const [uploadFailure, setUploadFailure] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const uploaded = useRef<{
    text: string;
    upload: string;
    files: Awaited<ReturnType<typeof uploadTexts>>["files"];
  } | null>(null);
  const gesture = useKeyedGesture();
  const root = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(root, NARROW_BELOW);
  const [pane, setPane] = useState<"board" | "data">("board");
  const catalogue = useRest("artifacts/sources", readCatalogue);
  const sources = useMemo(() => catalogue.data?.sources ?? [], [catalogue.data]);
  const resolved = useMemo(() => resolve(draft, sources), [draft, sources]);

  // EVERY READ THE PAGE DRAWS: each source's own rows (the panel's count),
  // the open source's suggestions, and every figure on the board.
  const needed = useMemo(() => {
    const out = new Map<string, DraftDataset>();
    for (const src of sources) {
      const set = draft.settings[src.name] ?? {};
      const base = datasetOf(src, set, undefined);
      out.set(base.key, base);
      if (src.name === open) {
        for (const r of recipesOf(src)) {
          const d = datasetOf(src, set, r);
          out.set(d.key, d);
        }
      }
    }
    for (const r of resolved) out.set(r.dataset.key, r.dataset);
    return [...out.values()];
  }, [sources, draft.settings, open, resolved]);
  const previews = usePreviews(needed);

  const busy = uploading || gesture.busy;
  const hold =
    titleHold(title) ??
    (resolved.length === 0 ? "Add at least one figure to the board." : undefined) ??
    (tooManyDatasets(resolved)
      ? "This board reads more datasets than one board holds."
      : undefined);

  const add = (src: DashboardEngineSource, r: Recipe) => {
    if (!hasRoom(draft)) return;
    const made = place(draft, src.name, r.key, r.widget.w);
    setDraft(made.draft);
    setFresh(made.id);
    toast.ok(`Added “${r.title}” to the board`);
  };

  const start = (s: Starter) => {
    setDraft(fromStarter(s, sources));
    setOpen(starterSources(s)[0] ?? null);
  };

  const create = async () => {
    if (hold || busy) return;
    setUploadFailure(null);
    const name = title.trim();
    const text = `${JSON.stringify(composeSpec(name, draft, sources), null, 2)}\n`;
    // THE SAME SPEC IS THE SAME UPLOAD: a retry after an answer nobody could
    // confirm sends the very request again, so the engine lands it once.
    let brought = uploaded.current?.text === text ? uploaded.current : null;
    if (!brought) {
      setUploading(true);
      try {
        const got = await uploadTexts(null, [{ path: SPEC_PATH, text }], newActOpID());
        brought = { text, ...got };
        uploaded.current = brought;
      } catch (err) {
        setUploadFailure(refusalText(err));
        return;
      } finally {
        setUploading(false);
      }
    }
    const answer = await gesture.run(
      createFromUpload({
        kind: "dashboard",
        title: name,
        upload: brought.upload,
        files: brought.files,
        entry: SPEC_PATH,
        ...(workItem ? { workItem } : {}),
      }),
    );
    if (answer?.kind === "refused") uploaded.current = null;
    const id =
      answer?.kind === "done" && typeof answer.body.artifact === "string"
        ? answer.body.artifact
        : "";
    if (!id) return;
    toast.ok(`Created “${name}”`);
    nav.to(artifactPath(id));
  };

  useKeyChords([{ key: "enter", meta: true, whileTyping: true, run: () => void create() }]);

  const dirty = title.trim() !== "" || draft.placed.length > 0;
  const refusal =
    uploadFailure ??
    (gesture.answer && gesture.answer.kind !== "done" ? gesture.answer.text : null);
  const problem = titleProblem(title);

  return (
    <CreateShell
      kind="dashboard"
      status={
        // A NARROW BAR has room for its title and its actions alone; the
        // Board tab says how many figures there are.
        busy
          ? "Creating…"
          : !narrow && resolved.length > 0
            ? plural(resolved.length, "figure")
            : undefined
      }
      tools={[
        {
          key: "refresh",
          label: "Refresh the data",
          icon: <RefreshCwGlyph size="sm" />,
          onSelect: () => previews.refresh(),
        },
      ]}
      action={{
        label: "Create dashboard",
        icon: <CheckGlyph size="sm" />,
        onSelect: () => void create(),
        keys: ["Mod", "Enter"],
        ...(hold
          ? { disabledReason: hold }
          : busy
            ? { disabledReason: "Creating the dashboard…" }
            : {}),
      }}
      onCancel={() => (dirty ? setConfirmLeave(true) : leave())}
    >
      <div className="dashboard-create" ref={root} data-narrow={narrow || undefined}>
        {narrow ? (
          <div className="dashboard-create-tabs">
            <SegmentedControl
              label="Show"
              semantics="tabs"
              panelId={PANE}
              fill
              value={pane}
              onValueChange={setPane}
              options={[
                { value: "board", label: "Board", count: resolved.length || null },
                { value: "data", label: "Data" },
              ]}
            />
          </div>
        ) : null}
        <div
          className="dashboard-create-main"
          hidden={narrow && pane !== "board"}
          {...(narrow && pane === "board" ? { id: PANE, role: "tabpanel" } : {})}
        >
          <div className="dashboard-create-head">
            <FormField
              label="Title"
              className="dashboard-create-title"
              error={problem}
              helper="Every figure is read as whoever opens the dashboard, so each reader sees only what they may."
            >
              {(field) => (
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  // A PAGE WHOSE FIRST QUESTION IS ITS NAME starts there.
                  autoFocus
                  value={title}
                  error={problem !== undefined}
                  placeholder="Delivery pulse"
                  onChange={(e) => setTitle(e.target.value)}
                />
              )}
            </FormField>
          </div>
          {refusal ? (
            <Callout variant="danger" title="The dashboard was not created" live="polite">
              {refusal}
            </Callout>
          ) : null}
          {catalogue.data === null ? (
            catalogue.loading ? (
              <Skeleton variant="grid" count={4} label="Loading the data you can use" />
            ) : (
              <QueryState error={catalogue.code} refusal={catalogue.refusal} loading={false} />
            )
          ) : (
            <BoardDraft
              draft={draft}
              resolved={resolved}
              previews={previews}
              fresh={fresh}
              onDraft={setDraft}
              onStart={start}
              boardTitle={title.trim() || "New dashboard"}
            />
          )}
        </div>
        <DataPanel
          hidden={narrow && pane !== "data"}
          panelId={narrow && pane === "data" ? PANE : undefined}
          sources={sources}
          loading={catalogue.loading && catalogue.data === null}
          draft={draft}
          open={open}
          onOpen={setOpen}
          previews={previews}
          onSetting={(source, key, value) => setDraft((d) => setSetting(d, source, key, value))}
          onAdd={add}
          boardTitle={title.trim() || "New dashboard"}
        />
      </div>
      <Modal
        open={confirmLeave}
        onClose={() => setConfirmLeave(false)}
        role="alertdialog"
        size="sm"
        title="Discard this dashboard?"
        footer={
          <>
            <Button variant="secondary" onClick={() => setConfirmLeave(false)}>
              Keep editing
            </Button>
            <Button variant="danger" onClick={leave}>
              Discard
            </Button>
          </>
        }
      >
        Its title and the figures on its board are not saved anywhere yet.
      </Modal>
    </CreateShell>
  );
}

// ---- the board ----------------------------------------------------------- //

function BoardDraft({
  draft,
  resolved,
  previews,
  fresh,
  onDraft,
  onStart,
  boardTitle,
}: {
  draft: Draft;
  resolved: ReturnType<typeof resolve>;
  previews: Previews;
  fresh: string | null;
  onDraft: (d: Draft) => void;
  onStart: (s: Starter) => void;
  boardTitle: string;
}) {
  const root = useRef<HTMLDivElement>(null);
  const stacked = useNarrowerThan(root, STACKS_BELOW);
  const byId = new Map(resolved.map((r) => [r.placed.id, r]));
  const reorder = useReorder({
    keys: resolved.map((r) => r.placed.id),
    onReorder: (next) => onDraft(reorderDraft(draft, next)),
    nameOf: (id) => byId.get(id)?.recipe.title ?? id,
    axis: "grid",
  });

  return (
    <section className="dashboard-create-board" aria-label="Board" ref={root}>
      {resolved.length === 0 ? (
        <div className="dashboard-create-empty">
          <EmptyState
            icon={<ArtifactKindGlyph kind="dashboard" size="lg" />}
            title="Start your board"
            description="Open a source under Data to see the figures it offers, or start from a ready-made board."
            headingLevel={2}
          />
          <ul className="dashboard-create-starters" aria-label="Ready-made boards">
            {STARTERS.map((s) => (
              <li key={s.key}>
                <Card asChild interactive padding="md" className="dashboard-create-starter">
                  <button type="button" onClick={() => onStart(s)}>
                    <span className="dashboard-create-starter-label">{s.label}</span>
                    <span className="dashboard-create-starter-text">{s.description}</span>
                  </button>
                </Card>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <>
          <VisuallyHidden id={reorder.hintId}>{reorder.hint}</VisuallyHidden>
          <ArrangeBoard editing columns={stacked ? 1 : DASHBOARD_COLUMNS}>
            <ArrangeGrid {...reorder.containerProps}>
              {reorder.order.map((id) => {
                const r = byId.get(id);
                if (!r) return null;
                const state = previews.states.get(r.dataset.key);
                return (
                  <ArrangeFrame
                    key={id}
                    {...reorder.itemProps(id)}
                    name={r.recipe.title}
                    editing
                    columns={stacked ? DASHBOARD_COLUMNS : r.placed.w}
                    rows={null}
                    rowHeight={ROW}
                    fresh={fresh === id}
                    handle={reorder.handleProps(id)}
                    resize={
                      stacked
                        ? undefined
                        : {
                            minColumns: MIN_COLUMNS,
                            vertical: false,
                            onResize: ({ columns }) => onDraft(resize(draft, id, columns)),
                          }
                    }
                    onRemove={() => onDraft(unplace(draft, id))}
                    className="dashboard-cell"
                  >
                    <DashboardWidget
                      boardTitle={boardTitle}
                      widget={r.widget}
                      dataset={boardDataset(r.dataset)}
                      answer={state?.answer ?? null}
                      status={previewStatus(state)}
                      detail={state?.failure ?? state?.answer?.detail}
                      asOf={state?.answer?.as_of}
                      onCite={undefined}
                      onFocus={() => {}}
                      inRow={!stacked}
                      actions={null}
                    />
                  </ArrangeFrame>
                );
              })}
            </ArrangeGrid>
          </ArrangeBoard>
        </>
      )}
    </section>
  );
}

// ---- the data panel ------------------------------------------------------ //

function DataPanel({
  hidden,
  panelId,
  sources,
  loading,
  draft,
  open,
  onOpen,
  previews,
  onSetting,
  onAdd,
  boardTitle,
}: {
  hidden: boolean;
  /** The tab panel's id, while the panel is the narrow page's open tab. */
  panelId: string | undefined;
  sources: readonly DashboardEngineSource[];
  loading: boolean;
  draft: Draft;
  open: string | null;
  onOpen: (name: string | null) => void;
  previews: Previews;
  onSetting: (source: string, key: string, value: string) => void;
  onAdd: (src: DashboardEngineSource, r: Recipe) => void;
  boardTitle: string;
}) {
  const now = Date.now();
  return (
    <aside
      className="dashboard-create-data"
      aria-labelledby="dashboard-create-data-title"
      hidden={hidden}
      {...(panelId ? { id: panelId } : {})}
    >
      <div className="dashboard-create-data-head">
        <h2 id="dashboard-create-data-title">Data</h2>
        <p>Open a source to see the figures it offers, drawn from its live rows.</p>
      </div>
      {loading ? (
        <Skeleton variant="list" rows={5} label="Loading the data you can use" />
      ) : (
        <DatasetSourceList label="Data you can use">
          {sources.map((src) => {
            const words = sourceWords(src);
            const set = draft.settings[src.name] ?? {};
            const base = datasetOf(src, set, undefined);
            const state = previews.states.get(base.key);
            const isOpen = open === src.name;
            return (
              <DatasetSource
                key={src.name}
                title={words.label}
                kind="engine"
                // THE SOURCE'S AREA in the kind's place: every source here
                // reads the engine, so the tag says which part of it.
                labels={{ kinds: { engine: words.area } }}
                definition={words.description}
                refresh={refreshWords(boardDataset(base).freshness)}
                asOf={state?.answer?.as_of}
                status={previewStatus(state)}
                detail={state?.failure ?? state?.answer?.detail}
                rows={previewRows(state)}
                selected={isOpen}
                onSelect={() => onOpen(isOpen ? null : src.name)}
                now={now}
              >
                {isOpen && previewStatus(state) !== "refused" ? (
                  <SourceFigures
                    src={src}
                    draft={draft}
                    previews={previews}
                    onSetting={(key, value) => onSetting(src.name, key, value)}
                    onAdd={(r) => onAdd(src, r)}
                    boardTitle={boardTitle}
                  />
                ) : null}
              </DatasetSource>
            );
          })}
        </DatasetSourceList>
      )}
    </aside>
  );
}

/** An open source's settings, and the figures it suggests drawn over its live rows. */
function SourceFigures({
  src,
  draft,
  previews,
  onSetting,
  onAdd,
  boardTitle,
}: {
  src: DashboardEngineSource;
  draft: Draft;
  previews: Previews;
  onSetting: (key: string, value: string) => void;
  onAdd: (r: Recipe) => void;
  boardTitle: string;
}) {
  const words = sourceWords(src);
  const set = draft.settings[src.name] ?? {};
  const settings = settingsOf(src);
  const room = hasRoom(draft);
  return (
    <div className="dashboard-create-source">
      {settings.length > 0 ? (
        <div className="dashboard-create-settings">
          {settings.map((s) => (
            <FormField key={s.key} label={s.label}>
              {(field) => (
                <Select
                  id={field.id}
                  aria-describedby={field.describedBy}
                  size="sm"
                  value={settingValue(src, set, s.key)}
                  options={s.options.map((o) => ({ value: o.value, label: o.label }))}
                  onChange={(v) => onSetting(s.key, String(v))}
                />
              )}
            </FormField>
          ))}
        </div>
      ) : null}
      <ul className="dashboard-create-suggestions" aria-label={`Figures from ${words.label}`}>
        {recipesOf(src).map((r) => {
          const d = datasetOf(src, set, r);
          const state = previews.states.get(d.key);
          const placed = draft.placed.some((p) => p.source === src.name && p.recipe === r.key);
          const half = r.widget.type === "stat" || r.widget.type === "meter";
          const addLabel = placed ? `Add “${r.title}” again` : `Add “${r.title}” to the board`;
          return (
            <li
              key={r.key}
              // A NUMBER STANDS HALF THE PANEL WIDE, two to a row, as stats
              // stand in a row on the board; a chart or a table takes the row.
              className={
                half
                  ? "dashboard-create-suggestion dashboard-create-suggestion--half"
                  : "dashboard-create-suggestion"
              }
            >
              <DashboardWidget
                boardTitle={boardTitle}
                widget={{ ...r.widget, id: `preview_${r.key}`, title: r.title, dataset: d.key }}
                dataset={boardDataset(d)}
                answer={state?.answer ?? null}
                status={previewStatus(state)}
                detail={state?.failure ?? state?.answer?.detail}
                asOf={state?.answer?.as_of}
                onCite={undefined}
                onFocus={() => {}}
                actions={
                  half ? (
                    <IconButton
                      size="sm"
                      variant={placed ? "ghost" : "secondary"}
                      label={addLabel}
                      icon={placed ? <CheckGlyph size="sm" /> : <PlusGlyph size="sm" />}
                      disabled={!room}
                      onClick={() => onAdd(r)}
                    />
                  ) : (
                    <Button
                      size="small"
                      variant={placed ? "ghost" : "secondary"}
                      leadingIcon={placed ? <CheckGlyph size="sm" /> : <PlusGlyph size="sm" />}
                      aria-label={addLabel}
                      disabled={!room}
                      onClick={() => onAdd(r)}
                    >
                      {placed ? "Added" : "Add"}
                    </Button>
                  )
                }
              />
            </li>
          );
        })}
      </ul>
    </div>
  );
}
