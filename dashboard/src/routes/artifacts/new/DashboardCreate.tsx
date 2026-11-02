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
 *
 * # Or an agent builds it
 *
 * "Ask an agent" in the bar opens the hand-off panel beside the board
 * (`AskAgent.tsx`), which files a real tracker task to the chosen agent seat
 * with the reader's brief (`useFileArtifactTask`). The board stays live while
 * the brief is written. With figures on it, the board is created first and
 * linked to the task, so the agent refines it and the reader lands on it;
 * with none, the agent builds the board from the brief and the reader lands
 * on the task.
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
  ConfirmModal,
  DatasetSource,
  DatasetSourceList,
  EmptyState,
  FormField,
  IconButton,
  Input,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  Text,
  VisuallyHidden,
  useReorder,
  useToast,
} from "@crewlethq/ui";
import {
  CheckGlyph,
  FileTextGlyph,
  LayoutDashboardGlyph,
  PlusGlyph,
  RefreshCwGlyph,
} from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { DASHBOARD_COLUMNS } from "~/contract/artifacts.ts";
import { createFromUpload, readCatalogue, uploadTexts } from "~/lib/artifacts/api.ts";
import { refreshWords } from "~/lib/artifacts/dashboard.ts";
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
import { titleHold, titleProblem, useSaveIn, useWorkItem } from "./create.ts";
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { useWhoBuildsIt } from "./WhoBuildsIt.tsx";
import { createDraft } from "./assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import {
  AskAgentPanel,
  askTitleHold,
  askTool,
  chosenAgent,
  taskRefusal,
  useAskPanel,
} from "./AskAgent.tsx";
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

/** Briefs to start from, one press each. */
const BOARD_BRIEFS = [
  {
    key: "load",
    label: "Team load",
    brief:
      "Show how much work each team is carrying, what is overdue, and how many tokens each agent used this month.",
  },
  {
    key: "delivery",
    label: "Delivery pulse",
    brief:
      "A weekly delivery pulse: items opened and closed, what is blocked and for how long, and the oldest work still in progress.",
  },
  {
    key: "spend",
    label: "Agent spend",
    brief:
      "Spend by agent and by model over the last 30 days, the busiest seats, and any agent close to its budget.",
  },
] as const;

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

/** What a new dashboard holds beyond what every create page does, as the assistant reads it. */
interface BoardState {
  /** The ready-made board to start from; read empty, since a start is a gesture, not a state. */
  readonly starter: string;
  readonly figures: readonly string[];
}

/** The ready-made boards by key, as a value names one. */
const STARTER_KEYS: readonly string[] = STARTERS.map((st) => st.key);

/**
 * A new dashboard's own fields: a ready-made board to start from, taken only
 * while the board is empty (`empty`), as its card is offered only then, and
 * the figures on the board, read only.
 */
export function boardFields(empty: boolean): DraftFieldSpec<BoardState>[] {
  return [
    {
      path: "starter",
      label: "Start from",
      hint: `One of ${STARTER_KEYS.join(", ")}, while the board is empty`,
      read: (d) => d.starter,
      write: (d, key) => {
        if (!empty) return refuse("The board has figures: a ready-made board starts an empty one.");
        return typeof key === "string" && STARTER_KEYS.includes(key.trim())
          ? { ...d, starter: key.trim() }
          : refuse(`Choose one of the ready-made boards: ${STARTER_KEYS.join(", ")}.`);
      },
    },
    { path: "figures", label: "Figures on the board", read: (d) => d.figures },
  ];
}

export function DashboardCreate() {
  const nav = useNavigator();
  const toast = useToast();
  const leave = useLeaveCreate();
  const workItem = useWorkItem();
  const { folder } = useSaveIn();
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
  const panel = useAskPanel();
  const [brief, setBrief] = useState("");
  const who = useWhoBuildsIt("dashboard");
  const filer = useFileArtifactTask();
  const agentName = chosenAgent(who)?.seat.name ?? "";
  // THE BOARD AN AGENT IS ASKED TO REFINE, once made: a task that could not be
  // filed is asked again without making a second board.
  const madeFor = useRef<{ text: string; id: string } | null>(null);
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

  const busy = uploading || gesture.busy || filer.busy;
  const tooMany = tooManyDatasets(resolved)
    ? "This board reads more datasets than one board holds."
    : undefined;
  const hold =
    titleHold(title) ??
    (resolved.length === 0 ? "Add at least one figure to the board." : undefined) ??
    tooMany;
  const askHold =
    filer.held ??
    (brief.trim() === "" ? "Say what the board should answer." : undefined) ??
    askTitleHold(title, "dashboard") ??
    (who.seat === "" ? "Add an agent to the chart first." : undefined) ??
    (who.project === "" ? "Choose the project to file the task in." : undefined) ??
    tooMany;

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

  /** Make the board as composed, or answer null with the refusal said on the page. */
  const createBoard = async (): Promise<string | null> => {
    setUploadFailure(null);
    const name = title.trim();
    const text = `${JSON.stringify(composeSpec(name, draft, sources), null, 2)}\n`;
    if (madeFor.current?.text === text) return madeFor.current.id;
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
        return null;
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
        ...(folder ? { folder } : {}),
      }),
    );
    if (answer?.kind === "refused") uploaded.current = null;
    const id =
      answer?.kind === "done" && typeof answer.body.artifact === "string"
        ? answer.body.artifact
        : "";
    if (!id) return null;
    madeFor.current = { text, id };
    return id;
  };

  const create = async () => {
    if (hold || busy) return;
    const id = await createBoard();
    if (!id) return;
    toast.ok(`Created “${title.trim()}”`);
    nav.to(artifactPath(id));
  };

  const ask = async () => {
    if (askHold || busy) return;
    const name = title.trim();
    // AN AGENT ASKED WITH FIGURES ON THE BOARD refines that board: it is made
    // first and handed over with the task. With none, the brief is the whole
    // ask and the agent publishes the board to the task.
    const id = resolved.length > 0 ? await createBoard() : null;
    if (resolved.length > 0 && !id) return;
    const filed = await filer.fileArtifactTask({
      kind: "dashboard",
      title: name,
      brief,
      seat: who.seat,
      project: who.project,
      ...(id ? { artifact: id } : {}),
    });
    if (!filed) return;
    if (typeof filed.linked === "string") {
      toast.show({
        variant: "warning",
        title: "The board was not linked to the task",
        message: filed.linked,
      });
    }
    nav.to(id ? artifactPath(id) : ["worklet", filed.item]);
  };

  const dirty = title.trim() !== "" || draft.placed.length > 0 || brief.trim() !== "";
  // WHAT IS OPEN, for the assistant: the title, the figures on the board and
  // the hand-off. A ready-made board is started as a press of it would, while
  // the board is empty; figures are the person's to add, size and move.
  useAssistantDraft(
    createDraft<BoardState>({
      what: "dashboard",
      help:
        "You can set the title and start an empty board from a ready-made one. Figures are " +
        "added, sized and moved by the person from Data, where each reads the company's " +
        "data as them.",
      step: panel.open ? "ask" : narrow ? pane : open ? "data" : "board",
      title,
      setTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: {
          starter: "",
          figures: resolved.map((r) => `${r.recipe.title} (${r.source.name})`),
        },
        fields: boardFields(draft.placed.length === 0),
        apply: (next) => {
          const chosen = STARTERS.find((st) => st.key === next.starter);
          if (chosen) start(chosen);
        },
      },
      blocked: () => (busy ? "The dashboard is being made." : null),
    }),
  );
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
        filer.busy
          ? "Asking…"
          : busy
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
        // A PHONE'S BAR HOLDS THE VERB ALONE, so the page's title is not cut.
        short: "Create",
        icon: <CheckGlyph size="sm" />,
        onSelect: () => void create(),
        ...(hold
          ? { disabledReason: hold }
          : busy
            ? { disabledReason: "Creating the dashboard…" }
            : {}),
      }}
      handoff={{
        tool: askTool(who, panel),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={
              resolved.length > 0
                ? `The board as it stands is created and handed to ${agentName || "the agent"}, who refines it from your brief.`
                : `${agentName || "The agent"} builds the board from your brief and the data it can read.`
            }
            brief={brief}
            onBrief={setBrief}
            briefLabel="What should the board answer?"
            briefPlaceholder="How much work each team carries, and what is overdue."
            briefHelper="The questions it answers and who looks at it. Every figure is read as whoever opens the dashboard."
            starters={BOARD_BRIEFS}
            carried={[
              ...(title.trim()
                ? [
                    {
                      key: "title",
                      icon: <FileTextGlyph size="sm" />,
                      label: `“${title.trim()}”`,
                      detail: "The dashboard's title",
                    },
                  ]
                : []),
              ...(resolved.length > 0
                ? [
                    {
                      key: "board",
                      icon: <LayoutDashboardGlyph size="sm" />,
                      label: `The board, with ${plural(resolved.length, "figure")}`,
                      detail: resolved.map((r) => r.recipe.title).join(", "),
                    },
                  ]
                : []),
            ]}
            alone="Add figures from Data, or start from a ready-made board, and the agent refines them."
            task={{
              title: title.trim() || "Untitled dashboard",
              body: artifactTaskBody(
                "dashboard",
                brief,
                resolved.length > 0 ? (madeFor.current?.id ?? "created when you send") : undefined,
              ),
            }}
            steps={
              resolved.length > 0
                ? [
                    "The board as it stands is created as the dashboard.",
                    `A task is filed to ${agentName || "the agent"}${who.project ? ` in ${who.project}` : ""}, with the dashboard linked.`,
                    `${agentName || "The agent"} refines it and publishes each change as a new version.`,
                    "You land on the dashboard, and follow the work from it and the task.",
                  ]
                : [
                    `A task is filed to ${agentName || "the agent"}${who.project ? ` in ${who.project}` : ""}, with your brief.`,
                    `${agentName || "The agent"} builds the board from the data it can read.`,
                    "It publishes the board to the task, where it appears on the Artifacts card.",
                  ]
            }
            notice={taskRefusal(filer.refusal)}
            sendLabel={(n) => (resolved.length > 0 ? `Create and ask ${n}` : `Ask ${n}`)}
            onSend={() => void ask()}
            hold={askHold}
            busy={busy}
          />
        ) : null,
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
              // THE BOARD'S OWN SHAPE while the catalogue loads: figures in rows.
              <Skeleton variant="grid" rows={2} columns={3} label="Loading the data you can use" />
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
              optional={panel.open}
              boardTitle={title.trim() || "New dashboard"}
            />
          )}
        </div>
        <DataPanel
          hidden={narrow && pane !== "data"}
          panelId={narrow && pane === "data" ? PANE : undefined}
          sources={sources}
          loading={catalogue.loading && catalogue.data === null}
          failed={!catalogue.loading && catalogue.data === null}
          narrow={narrow}
          draft={draft}
          open={open}
          onOpen={setOpen}
          previews={previews}
          onSetting={(source, key, value) => setDraft((d) => setSetting(d, source, key, value))}
          onAdd={add}
          boardTitle={title.trim() || "New dashboard"}
        />
      </div>
      <ConfirmModal
        open={confirmLeave}
        destructive
        title="Discard this dashboard?"
        message="Its title, its figures and any brief you wrote are not saved anywhere yet."
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        onClose={() => setConfirmLeave(false)}
        onConfirm={leave}
      />
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
  optional,
  boardTitle,
}: {
  draft: Draft;
  resolved: ReturnType<typeof resolve>;
  previews: Previews;
  fresh: string | null;
  onDraft: (d: Draft) => void;
  onStart: (s: Starter) => void;
  /** The board is the agent's starting point, which it may do without. */
  optional: boolean;
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
            size="compact"
            icon={<ArtifactKindGlyph kind="dashboard" size="lg" />}
            title={optional ? "Give the agent a starting point" : "Start your board"}
            description={
              optional
                ? "Optional. Add figures from Data or start from a ready-made board, and the agent refines them. Without any, it builds the board from your brief."
                : "Open a source under Data to see the figures it offers, or start from a ready-made board."
            }
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
  failed,
  narrow,
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
  /** The catalogue could not be read; the board says why. */
  failed: boolean;
  /** The panel is the narrow page's Data tab, which names it already. */
  narrow: boolean;
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
        {narrow ? (
          <VisuallyHidden as="h2" id="dashboard-create-data-title">
            Data
          </VisuallyHidden>
        ) : (
          <h2 id="dashboard-create-data-title">Data</h2>
        )}
        <p>Open a source to see the figures it offers, drawn from its live rows.</p>
      </div>
      {loading ? (
        // A SOURCE'S OWN SHAPE: a title over its lines, one under the other.
        <Skeleton variant="grid" rows={5} columns={1} label="Loading the data you can use" />
      ) : failed ? (
        // THE BOARD SAYS WHY, in its own place; the panel says where the
        // sources will be, so it is not left blank under its heading.
        <p className="dashboard-create-data-note">The sources appear here once they can be read.</p>
      ) : (
        // GROUPED BY AREA, each group a list named by its area: every source
        // here reads the engine, and a tag on every row naming its area (the
        // kind's place, borrowed) was a column a reader learned to skip.
        <Stack gap={4}>
          {groupByArea(sources).map(({ area, sources: inArea }) => (
            <Stack gap={2} key={area}>
              <Text as="h3" variant="caption">
                {area}
              </Text>
              <DatasetSourceList label={area}>
                {inArea.map((src) => {
                  const words = sourceWords(src);
                  const set = draft.settings[src.name] ?? {};
                  const base = datasetOf(src, set, undefined);
                  const state = previews.states.get(base.key);
                  const isOpen = open === src.name;
                  return (
                    <DatasetSource
                      key={src.name}
                      title={words.label}
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
            </Stack>
          ))}
        </Stack>
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

/** The sources in their own order, gathered under their areas in the order each area first appears. */
export function groupByArea(
  sources: readonly DashboardEngineSource[],
): { area: string; sources: DashboardEngineSource[] }[] {
  const groups = new Map<string, DashboardEngineSource[]>();
  for (const src of sources) {
    const area = sourceWords(src).area;
    const group = groups.get(area);
    if (group) group.push(src);
    else groups.set(area, [src]);
  }
  return [...groups].map(([area, inArea]) => ({ area, sources: inArea }));
}
