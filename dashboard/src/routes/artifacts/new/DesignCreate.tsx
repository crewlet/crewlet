/**
 * A new design: the canvas a person lays out before an agent draws it.
 *
 * # Shape first, then style
 *
 * A design has a shape before it has a look: which screens, at which sizes,
 * in which order. So the page opens on starting layouts (a landing page, app
 * screens, one screen at three sizes, a presentation, or a blank canvas),
 * each pictured as the gallery pictures a design, and picking one lays its
 * boards on the design system's own canvas (`CanvasBoard`), editable: boards
 * move and resize by their names and corners, Frame (`F`) places a new one,
 * Text and Sticky place notes, Delete removes the selected item.
 *
 * # The panel follows the selection, as a design tool's does
 *
 * With nothing selected the panel says what the canvas holds and how to
 * shape a board. The design's name, brief and references are the hand-off's,
 * since they are what the agent is asked, and on a phone the hand-off is the
 * screen: every field the ask waits on is in it. With a board selected it is the board's: its name, its
 * frame (a preset or a custom size), its place in the order and what it is
 * for. The list of boards beside the canvas is the canvas's other half: the
 * board picked on one is marked on the other.
 *
 * # Asking is the primary action
 *
 * Only an agent draws a design, so the hand-off is the page's one way to make
 * it: the page arrives with the hand-off panel open beside the canvas
 * (`AskAgent.tsx`), holding the design's brief, its references, who builds it
 * and the boards that go with them, and the bar's "Ask an agent" is its
 * primary while the panel is closed. The canvas stays the page.
 *
 * # Created, it is a real design waiting for its first draft
 *
 * Create is one keyed write: the engine's design layout starter publishes
 * the boards as placeholder pages of their sizes, each saying what it is for,
 * so the page opens on the canvas the person just laid out. An agent then
 * publishes each board's real page over its placeholder.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import {
  ArtifactKindTile,
  Button,
  Callout,
  CanvasBoard,
  CanvasSchematic,
  ConfirmModal,
  FormField,
  IconButton,
  Input,
  Link,
  LayersList,
  ListInput,
  Menu,
  Select,
  Textarea,
  useMediaMatch,
  useToast,
  type CanvasBoardSelection,
  type CanvasHandle,
  type MenuEntry,
} from "@crewlethq/ui";
import {
  ChevronDownGlyph,
  ChevronUpGlyph,
  FileTextGlyph,
  LaptopGlyph,
  LayoutGridGlyph,
  MonitorGlyph,
  PlusGlyph,
  PresentationGlyph,
  SmartphoneGlyph,
  TabletGlyph,
  TrashGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { plural, textBudget } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { createDesign } from "~/lib/artifacts/api.ts";
import { CANVAS_MAX_SIDE, CANVAS_MIN_SIDE } from "~/lib/artifacts/canvas.ts";
import {
  BOARD_BRIEF_MAX_BYTES,
  DESIGN_MAX_BOARDS,
  FRAME_PRESETS,
  STARTER_LAYOUTS,
  addBoard,
  boardProblems,
  changeBoard,
  draftFrom,
  draftHold,
  dropNote,
  layoutOf,
  linkProblem,
  placeNote,
  presetOf,
  removeBoard,
  reorderBoard,
  retypeNote,
  sizeWords,
  taskBrief,
  tidy,
  type DesignDraft,
  type DraftBoard,
  type FramePreset,
  type StarterLayout,
} from "~/lib/artifacts/designLayout.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleProblem, useSaveIn, useWorkItem } from "./create.ts";
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { useWhoBuildsIt } from "./WhoBuildsIt.tsx";
import { createDraft, linesOf } from "./assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { AskAgentPanel, askTool, chosenAgent, useAskPanel } from "./AskAgent.tsx";

/** Briefs to start from, one press each. */
const DESIGN_BRIEFS = [
  {
    key: "launch",
    label: "Product launch",
    brief:
      "A landing page for the new product: a headline that says what it does, three benefits, a screenshot and one call to action. Follow our brand.",
  },
  {
    key: "firstrun",
    label: "First-run screens",
    brief:
      "The first-run screens of the mobile app: a welcome, the two permissions we need and why, and the empty home screen with a first step.",
  },
  {
    key: "settings",
    label: "Calmer settings",
    brief:
      "A calmer version of our settings screen: the same options grouped by what people come to change, with clearer labels and fewer borders.",
  },
] as const;

/** Each frame preset's glyph. */
const PRESET_GLYPHS: Record<FramePreset["id"], ReactNode> = {
  desktop: <MonitorGlyph size="sm" />,
  laptop: <LaptopGlyph size="sm" />,
  tablet: <TabletGlyph size="sm" />,
  phone: <SmartphoneGlyph size="sm" />,
  presentation: <PresentationGlyph size="sm" />,
};

/** What a design's brief asks for, in the panel's helper line. */
const BRIEF_HINT = "What it is for, who it is for, the brand to follow and what done looks like.";

/** The most a design's brief holds, as a task's description holds it. */
const DESIGN_BRIEF_MAX_BYTES = 8000;

/** What a new design holds beyond what every create page does. */
interface DesignState {
  readonly draft: DesignDraft | null;
  readonly links: readonly string[];
}

/** The starting layouts by id, as a value names one. */
const LAYOUT_IDS: readonly string[] = STARTER_LAYOUTS.map((l) => l.id);

/**
 * A new design's own fields: its starting layout, picked only before any
 * board is laid out (a pick lays new boards and drops the old), its
 * references, each held to the link rule its field holds, and each board's
 * name and purpose by its place, held to the board's own rules.
 */
export function designFields(draft: DesignDraft | null): DraftFieldSpec<DesignState>[] {
  const board = (
    i: number,
    key: "title" | "brief",
  ): Pick<DraftFieldSpec<DesignState>, "read" | "write"> => ({
    read: (d) => d.draft?.boards[i]?.[key] ?? "",
    write: (d, value) => {
      const now = d.draft?.boards[i];
      if (!d.draft || !now) return refuse("That board is no longer on the canvas.");
      if (typeof value !== "string") return refuse("A board's name and purpose are text.");
      const next = changeBoard(d.draft, now.id, { [key]: value });
      const wrong = boardProblems(next.boards[i]!)[key];
      return wrong ? refuse(wrong) : { ...d, draft: next };
    },
  });
  return [
    {
      path: "layout",
      label: "Starting layout",
      hint: `One of ${LAYOUT_IDS.join(", ")}, picked before any board is laid out`,
      read: (d) => (d.draft ? "laid out" : ""),
      write: (d, id) => {
        if (d.draft) return refuse("Boards are laid out: picking another layout is the person's.");
        const layout = STARTER_LAYOUTS.find((l) => typeof id === "string" && l.id === id.trim());
        return layout
          ? { ...d, draft: draftFrom(layout) }
          : refuse(`Choose one of the layouts: ${LAYOUT_IDS.join(", ")}.`);
      },
    },
    {
      path: "references",
      label: "References",
      hint: "Web addresses, one per line",
      read: (d) => d.links,
      write: (d, value) => {
        const next = linesOf(value);
        if (next === null) return refuse("References are web addresses, one per line.");
        const wrong = next.map(linkProblem).find(Boolean);
        return wrong ? refuse(wrong) : { ...d, links: next };
      },
    },
    ...(draft?.boards ?? []).flatMap((b, i): DraftFieldSpec<DesignState>[] => [
      {
        path: `boards.${i + 1}.name`,
        label: `Board ${i + 1} name`,
        hint: sizeWords(b),
        ...board(i, "title"),
      },
      {
        path: `boards.${i + 1}.purpose`,
        label: `Board ${i + 1} purpose`,
        hint: "What the board is for, for the agent who draws it",
        ...board(i, "brief"),
      },
    ]),
  ];
}

export function DesignCreate() {
  const nav = useNavigator();
  const toast = useToast();
  const leave = useLeaveCreate();
  const workItem = useWorkItem();
  const { folder } = useSaveIn();
  const access = useArtifactAccess({ app: true });
  const create = useKeyedGesture();
  const who = useWhoBuildsIt("design");
  const ask = useFileArtifactTask();
  const panel = useAskPanel(true);
  // THE DESIGN ONCE IT IS MADE: a task refused after it was made is filed
  // again on its own, so a second press never makes a second design.
  const [made, setMade] = useState<string | null>(null);

  const [title, setTitle] = useState("");
  const [brief, setBrief] = useState("");
  const [links, setLinks] = useState<string[]>([]);
  const [linkError, setLinkError] = useState<string | undefined>();
  const [draft, setDraft] = useState<DesignDraft | null>(null);
  const [picking, setPicking] = useState(true);
  const [selected, setSelected] = useState<CanvasBoardSelection | null>(null);
  const [confirmLeave, setConfirmLeave] = useState(false);
  const canvasRef = useRef<CanvasHandle>(null);

  const board =
    selected?.kind === "board" ? draft?.boards.find((b) => b.id === selected.id) : undefined;
  const dirty = draft !== null || title.trim() !== "" || brief.trim() !== "" || links.length > 0;

  const pick = (layout: StarterLayout) => {
    setDraft(draftFrom(layout));
    setSelected(null);
    setPicking(false);
  };

  const update = (change: (d: DesignDraft) => DesignDraft) => setDraft((d) => (d ? change(d) : d));

  /** Add a board of a preset's size after the last one, select it and bring it into view. */
  const addPreset = (p: FramePreset) => {
    if (!draft) return;
    const added = addBoard(draft, p);
    if (!added.id) return;
    setDraft(added.draft);
    setSelected({ kind: "board", id: added.id });
    const b = added.draft.boards.find((x) => x.id === added.id)!;
    // After the canvas has drawn the new board, so its bounds include it.
    requestAnimationFrame(() =>
      canvasRef.current?.reveal({ x: b.x, y: b.y, width: b.w, height: b.h }),
    );
  };

  const hold =
    (title.trim() === "" ? "Name the design first." : titleProblem(title)) ??
    (draft ? draftHold(draft) : "Pick a layout.");
  const briefBudget = textBudget(brief, DESIGN_BRIEF_MAX_BYTES, "a design's brief");
  const builder = chosenAgent(who);
  const blocked = !access.can
    ? access.reason
    : (ask.held ??
      (builder ? undefined : "Add an agent to the chart to build it.") ??
      (briefBudget.over ? "Shorten the brief." : hold));
  const busy = create.busy || ask.busy;
  // WHAT IS OPEN, for the assistant: the name, the references, each board's
  // name and purpose, and the hand-off. A layout is picked as a press of it
  // would, while nothing is laid out yet; a board's place and size are the
  // person's, on the canvas.
  useAssistantDraft(
    createDraft<DesignState>({
      what: "design",
      help:
        "You can set the design's name, its references, and each board's name and what it " +
        "is for, and pick a starting layout before any board is laid out. Moving, resizing, " +
        "adding and removing boards are the person's, and you make no images.",
      step: panel.open ? "ask" : picking ? "layouts" : "canvas",
      title,
      setTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: { draft, links },
        fields: designFields(draft),
        apply: (next) => {
          if (next.draft !== draft) {
            setDraft(next.draft);
            if (draft === null) {
              setSelected(null);
              setPicking(false);
            }
          }
          if (next.links !== links) setLinks([...next.links]);
        },
      },
      blocked: () => (busy ? "The design is being made." : null),
    }),
  );
  // A PHONE'S BAR KEEPS ITS WIDTH FOR THE NAME AND THE ACTION: the board
  // count is the boards list's own there.
  const narrow = useMediaMatch("(width < 40rem)");

  const submit = useCallback(async () => {
    if (!draft || blocked || busy || !builder) return;
    const name = title.trim();
    let id = made;
    if (!id) {
      const answer = await create.run(
        createDesign({
          title: name,
          layout: layoutOf(draft),
          // WHO BUILDS IT MAY: the agent publishes each board over its
          // placeholder, which takes the editor's role on the design.
          shares: [{ audience: { kind: "seat", id: builder.seat.handle }, role: "editor" }],
          ...(workItem ? { workItem } : {}),
          ...(folder ? { folder } : {}),
        }),
      );
      if (answer?.kind !== "done" || typeof answer.body.artifact !== "string") return;
      id = answer.body.artifact;
      setMade(id);
    }
    const filed = await ask.fileArtifactTask({
      kind: "design",
      title: name,
      brief: taskBrief({ brief, draft, links }),
      seat: builder.seat.handle,
      project: who.project || undefined,
      artifact: id,
    });
    // REFUSED, THE PAGE STAYS: the refusal is drawn in the panel, and the
    // next press files the task for the design already made.
    if (!filed) return;
    if (typeof filed.linked === "string") {
      toast.show({
        variant: "warning",
        title: "The task was filed, and the design is not on its Artifacts card",
        message: filed.linked,
        action: { label: "Open the task", href: href(["worklet", filed.item]) },
      });
    }
    nav.replace(artifactPath(id));
  }, [
    draft,
    blocked,
    busy,
    builder,
    title,
    made,
    create,
    workItem,
    folder,
    ask,
    brief,
    links,
    who.project,
    toast,
    nav,
  ]);

  const count = draft ? plural(draft.boards.length, "board") : undefined;
  const tools = draft
    ? [
        {
          key: "layouts",
          label: "Layouts",
          icon: <LayoutGridGlyph size="sm" />,
          onSelect: () => setPicking(true),
          pressed: picking,
        },
        {
          key: "tidy",
          label: "Tidy up",
          icon: <WandSparklesGlyph size="sm" />,
          onSelect: () => {
            update((d) => ({ ...d, boards: tidy(d.boards) }));
            requestAnimationFrame(() => canvasRef.current?.fit());
          },
          disabledReason:
            picking || draft.boards.length < 2 ? "Tidy up lines up two or more boards." : undefined,
        },
      ]
    : [];

  return (
    <CreateShell
      kind="design"
      status={
        create.busy
          ? "Creating the design…"
          : ask.busy
            ? `Asking ${builder?.seat.name ?? "the agent"}…`
            : narrow
              ? undefined
              : count
      }
      tools={tools}
      onCancel={() => (dirty ? setConfirmLeave(true) : leave())}
      handoff={{
        tool: askTool(who, panel, true),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={`The design is made from the boards you laid out and shared with ${builder?.seat.name ?? "the agent"}, who draws each board over its placeholder.`}
            brief={brief}
            onBrief={setBrief}
            briefLabel="What is the design for?"
            briefPlaceholder="A landing page for the new product, in our brand."
            briefHelper={briefBudget.line ?? BRIEF_HINT}
            starters={DESIGN_BRIEFS}
            carried={[
              ...(title.trim()
                ? [
                    {
                      key: "title",
                      icon: <FileTextGlyph size="sm" />,
                      label: `“${title.trim()}”`,
                      detail: "The design's name",
                    },
                  ]
                : []),
              ...(draft
                ? [
                    {
                      key: "boards",
                      icon: <LayoutGridGlyph size="sm" />,
                      label: plural(draft.boards.length, "board"),
                      detail:
                        draft.boards.map((b) => b.title.trim() || "Untitled board").join(", ") ||
                        "Place a board on the canvas.",
                    },
                  ]
                : []),
            ]}
            alone="Pick a starting layout and its boards go with the brief."
            task={
              draft
                ? {
                    title: title.trim() || "Untitled design",
                    body: artifactTaskBody(
                      "design",
                      taskBrief({ brief, draft, links }),
                      made ?? "created when you send",
                    ),
                  }
                : undefined
            }
            steps={[
              `The design is made from your boards and shared with ${builder?.seat.name ?? "the agent"} as an editor.`,
              `A task is filed to ${builder?.seat.name ?? "the agent"}${who.project ? ` in ${who.project}` : ""}, with the design linked.`,
              `${builder?.seat.name ?? "The agent"} draws each board and publishes it over its placeholder.`,
              "You land on the design, and follow the work from it and the task.",
            ]}
            notice={
              <>
                {create.answer && create.answer.kind !== "done" && (
                  <Callout variant="danger" title="The design was not created" live="polite">
                    <p>{create.answer.text}</p>
                  </Callout>
                )}
                {ask.refusal && (
                  <Callout
                    live="polite"
                    variant={made ? "warning" : "danger"}
                    title={
                      made
                        ? "The design is made, and its task was not filed"
                        : "The task was not filed"
                    }
                  >
                    <p>{ask.refusal.sentence}</p>
                    {made && (
                      <p>
                        Press Ask again to file it, or{" "}
                        <Link href={href(artifactPath(made))}>open the design</Link>.
                      </p>
                    )}
                  </Callout>
                )}
              </>
            }
            sendLabel={(n) => (made ? `Ask ${n}` : `Create and ask ${n}`)}
            onSend={() => void submit()}
            hold={blocked}
            busy={busy}
          >
            <FormField label="Design name" required error={titleProblem(title)}>
              {(field) => (
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  aria-invalid={field.invalid || undefined}
                  value={title}
                  placeholder="Launch site"
                  autoComplete="off"
                  onChange={(event) => setTitle(event.currentTarget.value)}
                />
              )}
            </FormField>
            <ListInput
              label="References"
              itemName="link"
              optional
              value={links}
              onChange={(next) => {
                const added = next.find((l) => !links.includes(l));
                const wrong = added === undefined ? undefined : linkProblem(added);
                setLinkError(wrong);
                if (!wrong) setLinks(next.map((l) => l.trim()));
              }}
              error={linkError}
              placeholder="https://"
              helper="Sites, files or other artifacts to look at."
              enterHint
            />
          </AskAgentPanel>
        ) : null,
      }}
    >
      {picking || !draft ? (
        <div className="design-create">
          <LayoutPicker
            onPick={pick}
            onBack={draft ? () => setPicking(false) : undefined}
            boards={draft?.boards.length ?? 0}
          />
        </div>
      ) : (
        <div className="design-create">
          <div className="design-create-grid">
            <div className="design-create-layers">
              <LayersList
                label="Boards"
                heading={
                  <div className="design-create-layers-head">
                    <span>{count}</span>
                    <AddBoardMenu
                      disabled={draft.boards.length >= DESIGN_MAX_BOARDS}
                      onAdd={addPreset}
                    />
                  </div>
                }
                items={draft.boards.map((b) => ({
                  id: b.id,
                  label: b.title.trim() || "Untitled board",
                }))}
                selected={board?.id ?? null}
                onSelect={(id) => {
                  setSelected({ kind: "board", id });
                  const b = draft.boards.find((x) => x.id === id);
                  if (b) canvasRef.current?.reveal({ x: b.x, y: b.y, width: b.w, height: b.h });
                }}
              />
            </div>
            <div className="design-create-canvas">
              <CanvasBoard
                label={title.trim() || "New design"}
                boards={draft.boards.map((b) => ({
                  id: b.id,
                  x: b.x,
                  y: b.y,
                  w: b.w,
                  h: b.h,
                  title: b.title.trim() || "Untitled board",
                }))}
                notes={draft.notes}
                renderBoard={(item) => {
                  const b = draft.boards.find((x) => x.id === item.id);
                  return b ? <BoardPlaceholder board={b} /> : null;
                }}
                // A placeholder is markup, not a page: every board is drawn.
                maxLive={DESIGN_MAX_BOARDS}
                minLiveWidth={0}
                editable
                minSide={CANVAS_MIN_SIDE}
                maxSide={CANVAS_MAX_SIDE}
                selected={selected}
                onSelectedChange={setSelected}
                onBoardChange={(id, rect) => update((d) => changeBoard(d, id, rect))}
                onBoardCreate={(at) => {
                  const added = addBoard(
                    draft,
                    FRAME_PRESETS.find((p) => p.id === "phone")!,
                    at,
                  );
                  if (added.id) setDraft(added.draft);
                  return added.id;
                }}
                onBoardRemove={(id) => update((d) => removeBoard(d, id))}
                onNoteCreate={(kind, at) => {
                  const placed = placeNote(draft, kind, at);
                  if (placed.id) setDraft(placed.draft);
                  return placed.id;
                }}
                onNoteChange={(note) => update((d) => retypeNote(d, note))}
                onNoteRemove={(id) => update((d) => dropNote(d, id))}
                labels={{ paused: "Zoom in to read this board" }}
                canvasRef={canvasRef}
              />
              {draft.boards.length === 0 && (
                <p className="design-create-empty">
                  Press F and click the canvas to place a board, or use Add board.
                </p>
              )}
            </div>
            <div className="design-create-panel">
              {board ? (
                <BoardPanel
                  key={board.id}
                  board={board}
                  index={draft.boards.indexOf(board)}
                  count={draft.boards.length}
                  onChange={(fields) => update((d) => changeBoard(d, board.id, fields))}
                  onMove={(by) => update((d) => reorderBoard(d, board.id, by))}
                  onRemove={() => {
                    update((d) => removeBoard(d, board.id));
                    setSelected(null);
                  }}
                  onDone={() => setSelected(null)}
                />
              ) : (
                <DesignPanel
                  boards={draft.boards.length}
                  addBoard={
                    <AddBoardMenu
                      disabled={draft.boards.length >= DESIGN_MAX_BOARDS}
                      onAdd={addPreset}
                    />
                  }
                />
              )}
            </div>
          </div>
        </div>
      )}
      {confirmLeave && (
        <ConfirmModal
          open
          destructive
          title="Discard this design?"
          message="Its name, its boards and any brief you wrote are not saved anywhere yet."
          cancelLabel="Keep editing"
          confirmLabel="Discard"
          onClose={() => setConfirmLeave(false)}
          onConfirm={leave}
        />
      )}
    </CreateShell>
  );
}

/**
 * The starting layouts, each pictured as the gallery pictures a design: its
 * boards where they would sit, on the design kind's ground.
 */
function LayoutPicker({
  onPick,
  onBack,
  boards,
}: {
  onPick: (layout: StarterLayout) => void;
  onBack: (() => void) | undefined;
  boards: number;
}) {
  const first = useRef<HTMLLIElement>(null);
  useEffect(() => {
    first.current?.querySelector("button")?.focus();
  }, []);
  return (
    <section className="design-create-start" aria-labelledby="design-create-start-title">
      <div className="design-create-start-head">
        <h2 id="design-create-start-title">Start with a layout</h2>
        <p>
          Pick the boards to begin with. Every board can be renamed, resized, moved or removed next.
        </p>
      </div>
      <ul className="design-create-start-grid">
        {STARTER_LAYOUTS.map((layout, i) => {
          const drawn = draftFrom(layout).boards;
          return (
            <li key={layout.id} ref={i === 0 ? first : undefined}>
              <ArtifactKindTile
                kind="design"
                label={layout.label}
                detail={layout.detail}
                face={
                  drawn.length > 0 ? (
                    <CanvasSchematic
                      className="design-create-start-face"
                      label={layout.label}
                      boards={drawn.map((b) => ({ id: b.id, x: b.x, y: b.y, w: b.w, h: b.h }))}
                      cover={drawn[0]!.id}
                    />
                  ) : (
                    // AN EMPTY CANVAS, drawn as one: a dashed slot where the
                    // first board will go, not the kind's own picture.
                    <span className="design-create-start-blank" />
                  )
                }
                onSelect={() => onPick(layout)}
              />
            </li>
          );
        })}
      </ul>
      {onBack && (
        <div className="design-create-start-back">
          <Button variant="secondary" size="small" onClick={onBack}>
            Back to your {plural(boards, "board")}
          </Button>
          <span>Picking a layout replaces the boards you have.</span>
        </div>
      )}
    </section>
  );
}

/** Add board: the frame presets, each with its size. */
function AddBoardMenu({ disabled, onAdd }: { disabled: boolean; onAdd: (p: FramePreset) => void }) {
  const items: MenuEntry[] = FRAME_PRESETS.map((p) => ({
    key: p.id,
    label: p.label,
    icon: PRESET_GLYPHS[p.id],
    hint: `${p.w} by ${p.h}`,
    disabled,
    onSelect: () => onAdd(p),
  }));
  return (
    <Menu
      label="Add board"
      items={items}
      align="end"
      renderTrigger={(trigger) => (
        <IconButton
          {...trigger}
          size="sm"
          variant="ghost"
          label="Add board"
          icon={<PlusGlyph size="sm" />}
          disabledReason={
            disabled ? `A design holds at most ${DESIGN_MAX_BOARDS} boards.` : undefined
          }
        />
      )}
    />
  );
}

/**
 * A board on the canvas before it is drawn: its size and what it is for, set
 * at the board's own scale, as the placeholder page the engine publishes says.
 */
function BoardPlaceholder({ board }: { board: DraftBoard }) {
  const text = board.brief.trim();
  return (
    <div className="design-create-board">
      <div className="design-create-board-card">
        <span className="design-create-board-size">{sizeWords(board)}</span>
        <span className="design-create-board-title">{board.title.trim() || "Untitled board"}</span>
        {text ? (
          <span className="design-create-board-brief">{text}</span>
        ) : (
          <span className="design-create-board-wait">Say what this board is for in the panel.</span>
        )}
      </div>
    </div>
  );
}

/** The panel with nothing selected: what the canvas holds, and how to shape a board. */
function DesignPanel({
  boards,
  addBoard,
}: {
  boards: number;
  /** Add board, drawn here where the list of boards that carries it is folded away. */
  addBoard: ReactNode;
}) {
  return (
    <div className="design-create-fields">
      <div className="design-create-panel-head">
        <h2 className="design-create-panel-title">Design</h2>
        <span className="design-create-panel-add">{addBoard}</span>
      </div>
      <p className="design-create-note">
        {boards === 0
          ? "Place a board on the canvas to start."
          : `${plural(boards, "board")} on the canvas. Select one to set its size, its place in the order and what it is for.`}{" "}
        The design's name, brief and references are in Ask an agent.
      </p>
    </div>
  );
}

/** The panel with a board selected: its name, frame, order and brief. */
function BoardPanel({
  board,
  index,
  count,
  onChange,
  onMove,
  onRemove,
  onDone,
}: {
  board: DraftBoard;
  index: number;
  count: number;
  onChange: (fields: Partial<Omit<DraftBoard, "id">>) => void;
  onMove: (by: -1 | 1) => void;
  onRemove: () => void;
  onDone: () => void;
}) {
  const problems = boardProblems(board);
  const preset = presetOf(board.w, board.h);
  const brief = textBudget(board.brief, BOARD_BRIEF_MAX_BYTES, "a board's brief");
  // THE SIZE FIELDS HOLD WHAT IS TYPED, and the board takes it once it is a
  // size the canvas draws: a half-typed "4" is not a 4 pixel board.
  const [w, setW] = useState(String(board.w));
  const [h, setH] = useState(String(board.h));
  useEffect(() => setW(String(board.w)), [board.w]);
  useEffect(() => setH(String(board.h)), [board.h]);
  const side = (value: string, set: (v: string) => void, key: "w" | "h") => {
    set(value);
    const n = Number(value);
    if (Number.isInteger(n) && n >= CANVAS_MIN_SIDE && n <= CANVAS_MAX_SIDE) onChange({ [key]: n });
  };
  const sideError = (value: string) => {
    const n = Number(value);
    return Number.isInteger(n) && n >= CANVAS_MIN_SIDE && n <= CANVAS_MAX_SIDE
      ? undefined
      : `From ${CANVAS_MIN_SIDE} to ${CANVAS_MAX_SIDE}.`;
  };
  return (
    <div className="design-create-fields">
      <div className="design-create-panel-head">
        <h2 className="design-create-panel-title">
          Board {index + 1} of {count}
        </h2>
        <Button variant="ghost" size="small" onClick={onDone}>
          Done
        </Button>
      </div>
      <FormField label="Name" required error={problems.title}>
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            value={board.title}
            autoComplete="off"
            onChange={(event) => onChange({ title: event.currentTarget.value })}
          />
        )}
      </FormField>
      <FormField label="Frame">
        {(field) => (
          <Select
            id={field.id}
            value={preset?.id ?? "custom"}
            options={[
              ...FRAME_PRESETS.map((p) => ({
                value: p.id,
                label: p.label,
                leading: PRESET_GLYPHS[p.id],
                hint: `${p.w} by ${p.h}`,
              })),
              { value: "custom", label: "Custom", hint: `${board.w} by ${board.h}` },
            ]}
            onChange={(value) => {
              const p = FRAME_PRESETS.find((x) => x.id === value);
              if (p) onChange({ w: p.w, h: p.h });
            }}
          />
        )}
      </FormField>
      <div className="design-create-size">
        <FormField label="Width" error={sideError(w)}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              inputMode="numeric"
              value={w}
              onChange={(event) => side(event.currentTarget.value, setW, "w")}
            />
          )}
        </FormField>
        <FormField label="Height" error={sideError(h)}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid || undefined}
              inputMode="numeric"
              value={h}
              onChange={(event) => side(event.currentTarget.value, setH, "h")}
            />
          )}
        </FormField>
      </div>
      <FormField label="What this board is for" optional helper={brief.line} error={problems.brief}>
        {(field) => (
          <Textarea
            id={field.id}
            aria-describedby={field.describedBy}
            autoResize
            rows={4}
            value={board.brief}
            onChange={(event) => onChange({ brief: event.currentTarget.value })}
          />
        )}
      </FormField>
      <div className="design-create-board-actions">
        <IconButton
          size="sm"
          variant="ghost"
          label="Move earlier"
          icon={<ChevronUpGlyph size="sm" />}
          disabledReason={index === 0 ? "This is the first board." : undefined}
          onClick={() => onMove(-1)}
        />
        <IconButton
          size="sm"
          variant="ghost"
          label="Move later"
          icon={<ChevronDownGlyph size="sm" />}
          disabledReason={index === count - 1 ? "This is the last board." : undefined}
          onClick={() => onMove(1)}
        />
        <Button
          variant="ghost"
          size="small"
          leadingIcon={<TrashGlyph size="sm" />}
          onClick={onRemove}
        >
          Delete board
        </Button>
      </div>
    </div>
  );
}
