/**
 * A new slide deck: a theme and the slides it opens with, laid out on a real
 * stage before anything is made, then made in one write and opened in Edit.
 *
 * # Why the page lays the slides out
 *
 * The deck editor edits a slide's words in place, its notes and its order; the
 * slides a deck begins with are the starting set a person works from. So the
 * page asks for what a presentation editor's first minute asks for (Google
 * Slides' theme panel beside a real slide, Keynote's grid of rendered themes,
 * Gamma's outline before the deck): the title, the theme, and the run of
 * slides, each a layout and its title, added, duplicated, removed and
 * reordered in a filmstrip exactly as in the editor.
 *
 * # What is shown is what the engine writes
 *
 * The engine composes the deck from the plan (`slideformat.Compose`) and holds
 * it to the format like any version. Every slide here is the engine's own
 * drawing of that layout in that theme (`GET /artifacts/starters/slides`),
 * with the typed title written into its title run, drawn by the same renderer
 * and `SlideStage` the editor uses, so the deck that opens is the deck that was
 * shown, word for word but for what the person changes next.
 */

import { useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  DocumentTitleField,
  FormField,
  Input,
  Kbd,
  isApplePlatform,
  Menu,
  Select,
  Skeleton,
  SlideFilmstrip,
  SlideStage,
  ThemeGallery,
  useMediaMatch,
  useToast,
} from "@crewlethq/ui";
import { FileTextGlyph, PaletteGlyph, PlusGlyph, PresentationGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useNavigator } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { QueryState } from "~/components/common.tsx";
import { createFromStarter, readSlidesStarters } from "~/lib/artifacts/api.ts";
import {
  COVER_LAYOUT,
  addAfter,
  duplicate,
  planHold,
  planBrief,
  planOf,
  previewSlide,
  remove,
  reorder,
  rowsOf,
  slideTitleProblem,
  update,
  type PlanRow,
} from "~/lib/artifacts/deckPlan.ts";
import { renderSlide } from "~/lib/slides.tsx";
import { useWidth } from "~/lib/useWidth.ts";
import { useRest } from "~/lib/useRest.ts";
import type { DeckLayout, DeckSlide, DeckTheme, SlidesStartersAnswer } from "~/protocol/index.ts";
import { useAdoptedBrand, type BrandAssets } from "~/lib/brand/useBrand.ts";
import { logoSlotOf } from "~/lib/brand/brand.ts";
import { BRAND_DECK_THEME } from "~/components/brand.tsx";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useCreateArtifact, useSaveIn, useWorkItem } from "./create.ts";
import { artifactTaskBody, useFileArtifactTask } from "./fileTask.ts";
import { useWhoBuildsIt } from "./WhoBuildsIt.tsx";
import { createDraft } from "./assistantDraft.ts";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { AskAgentPanel, askTitleHold, askTool, chosenAgent, useAskPanel } from "./AskAgent.tsx";

/** Briefs to start from, one press each. */
const DECK_BRIEFS = [
  {
    key: "board",
    label: "Board update",
    brief:
      "A ten-minute board update on the support desk: what changed this quarter, the numbers, and the one decision we need.",
  },
  {
    key: "kickoff",
    label: "Project kickoff",
    brief:
      "A kickoff deck for the new project: the problem, the goal, who is on it, the plan by month and how we will know it worked.",
  },
  {
    key: "review",
    label: "Quarterly review",
    brief:
      "A quarterly review for the whole company: what we shipped, what we learned, where we missed and the priorities for next quarter.",
  },
] as const;

/** The longest brief, in characters: a task's description holds it and the plan. */
const BRIEF_MAX = 4000;

/** The engine's themes with the company's own, where it offers one, first. */
function brandFirst(themes: readonly DeckTheme[]): DeckTheme[] {
  return [
    ...themes.filter((t) => t.id === BRAND_DECK_THEME),
    ...themes.filter((t) => t.id !== BRAND_DECK_THEME),
  ];
}

/**
 * The theme a new deck opens on where the company has adopted no brand:
 * Crewlet's own. The engine's default plan keeps the look its starter always
 * had, for a create that names none.
 */
const OPENING_THEME = "nightshade";

/**
 * The composer's width under which the strip leaves its column for a row
 * under the stage: three columns there would leave the stage under 480px.
 */
const SPLIT_BELOW = 960;

/**
 * How the composer lays out its three parts: in "three" columns (the strip,
 * the stage, the settings), "split" (the strip a row under the stage, the
 * settings beside it), or one "column" on a phone.
 *
 * THE STAGE IS THE PAGE'S SUBJECT, so a page too narrow for three columns to
 * leave it a readable width lays the strip out under it, as a phone does. The
 * composer's own width decides, since the sidebar and a peek take from it and
 * the window's width says nothing about either. The placeholder drawn while
 * the themes load asks the same question, so the page does not move when they
 * arrive.
 */
function useComposerArrangement() {
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const root = useRef<HTMLDivElement>(null);
  const width = useWidth(root);
  const arrangement = phone ? "column" : width > 0 && width < SPLIT_BELOW ? "split" : "three";
  return { root, phone, arrangement, stripInMain: arrangement !== "three" };
}

/** A slide's placeholder: its hold's whole box, with a slide's corners and none of a box's default margin. */
const SLIDE_HOLD = {
  className: "slides-create-skeleton",
  variant: "box",
  height: "100%",
  marginTop: 0,
  borderRadius: "var(--radius-sm)",
} as const;

/** The composer's shape while the themes and layouts load: its parts where they will stand. */
function ComposerSkeleton() {
  const { root, arrangement, stripInMain } = useComposerArrangement();
  const strip = (
    <div
      className="slides-create-strip slides-create-strip-hold"
      data-orientation={stripInMain ? "horizontal" : "vertical"}
    >
      {[0, 1, 2, 3].map((n) => (
        <div key={n} className="slides-create-thumb-hold">
          <Skeleton {...SLIDE_HOLD} />
        </div>
      ))}
    </div>
  );
  return (
    <div ref={root} className="slides-create" data-arrangement={arrangement} aria-busy="true">
      {!stripInMain && strip}
      <div className="slides-create-main">
        <div className="slides-create-title-hold">
          <Skeleton
            className="slides-create-skeleton"
            variant="box"
            width="40%"
            height="100%"
            marginTop={0}
            label="Loading themes and layouts"
          />
        </div>
        <div className="slides-create-stage-hold">
          <Skeleton {...SLIDE_HOLD} />
        </div>
        {stripInMain && strip}
      </div>
      <div className="slides-create-side">
        <Skeleton variant="grid" rows={3} columns={2} />
      </div>
    </div>
  );
}

/** Nothing on a starter slide is a file of a version: no image to resolve. */
/**
 * What a theme's preview draws an image from: the brand theme's cover logo,
 * which its sample names by path and holds no bytes of, from the adopted
 * brand's own file; nothing for any other image a sample names.
 */
export function previewImages(
  theme: DeckTheme,
  assets: BrandAssets,
): (path: string) => string | null {
  const logo = theme.logo;
  if (!logo) return () => null;
  const slot = logoSlotOf(logo.slot);
  return (path) => (slot && path === logo.path ? assets.logo(slot) : null);
}

/** What a new deck holds beyond what every create page does. */
interface DeckDraft {
  readonly theme: string;
  readonly rows: readonly PlanRow[];
}

/**
 * A new deck's own fields: its theme, chosen from the engine's, and each
 * slide's title by its place in the deck, held to the slide title's own
 * bound. A slide's layout is said in its hint and never set: adding, moving
 * and removing slides are the person's, on the strip.
 */
export function deckFields(
  themes: readonly DeckTheme[],
  layouts: ReadonlyMap<string, DeckLayout>,
  rows: readonly PlanRow[],
): DraftFieldSpec<DeckDraft>[] {
  const ids = themes.map((t) => t.id);
  return [
    {
      path: "theme",
      label: "Theme",
      hint: `One of ${themes.map((t) => `${t.id} (${t.name})`).join(", ")}`,
      read: (d) => d.theme,
      write: (d, theme) =>
        typeof theme === "string" && ids.includes(theme.trim())
          ? { ...d, theme: theme.trim() }
          : refuse(`Choose one of the themes: ${ids.join(", ")}.`),
    },
    ...rows.map((row, i): DraftFieldSpec<DeckDraft> => ({
      path: `slides.${i + 1}.title`,
      label: `Slide ${i + 1} title`,
      hint: `${layouts.get(row.layout)?.name ?? row.layout} layout; empty for its own heading`,
      read: (d) => d.rows[i]?.title ?? "",
      write: (d, title) => {
        if (typeof title !== "string") return refuse("A slide's title is a line of text.");
        const wrong = slideTitleProblem(title);
        if (wrong) return refuse(wrong);
        return {
          ...d,
          rows: d.rows.map((r, at) => (at === i ? { ...r, title: title.trim() } : r)),
        };
      },
    })),
  ];
}

export function SlidesCreate() {
  const starters = useRest<SlidesStartersAnswer>("slides-starters", (signal) =>
    readSlidesStarters(signal),
  );
  if (!starters.data) {
    return (
      <CreateShell kind="slides">
        {starters.loading ? (
          <ComposerSkeleton />
        ) : (
          <div className="slides-create-state" role="alert">
            <h2 className="slides-create-state-title">The themes and layouts did not load</h2>
            <QueryState error={starters.code} refusal={starters.refusal} loading={false} />
            <Button size="small" variant="secondary" onClick={() => void starters.reload()}>
              Try again
            </Button>
          </div>
        )}
      </CreateShell>
    );
  }
  return <DeckComposer starters={starters.data} />;
}

function DeckComposer({ starters }: { starters: SlidesStartersAnswer }) {
  const create = useCreateArtifact();
  const workItem = useWorkItem();
  const { folder } = useSaveIn();
  const { root, phone, arrangement, stripInMain } = useComposerArrangement();
  const opening = useMemo(() => rowsOf(starters.default), [starters.default]);
  const { adoption, assets } = useAdoptedBrand();
  // THE COMPANY'S BRAND FIRST, AND CHOSEN: the engine offers its theme
  // (`brand`) whenever a brand is adopted, and a new deck opens in it.
  const themes = useMemo(() => brandFirst(starters.themes), [starters.themes]);
  const firstTheme =
    themes.find((t) => t.id === BRAND_DECK_THEME)?.id ??
    themes.find((t) => t.id === OPENING_THEME)?.id ??
    starters.default.theme ??
    starters.themes[0]?.id ??
    "";

  const [title, setTitle] = useState("");
  const [brief, setBrief] = useState("");
  const [themeId, setThemeId] = useState(firstTheme);
  const [rows, setRows] = useState<PlanRow[]>(opening);
  const [selected, setSelected] = useState(opening[0]?.key ?? "");
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);
  const creating = useRef(false);
  const leave = useLeaveCreate();

  const theme = starters.themes.find((t) => t.id === themeId) ?? starters.themes[0]!;
  const image = previewImages(theme, assets);
  const layouts = useMemo(
    () => new Map(starters.layouts.map((l) => [l.id, l])),
    [starters.layouts],
  );
  const slides = rows
    .map((r) => previewSlide(theme, r, title, layouts.get(r.layout)))
    .filter((s): s is DeckSlide => s !== undefined);
  const current = rows.find((r) => r.key === selected) ?? rows[0];
  const currentSlide = slides.find((s) => s.id === current?.key);
  const position = current ? rows.indexOf(current) : -1;

  const changed =
    title.trim() !== "" ||
    brief.trim() !== "" ||
    themeId !== firstTheme ||
    JSON.stringify(planOf("", rows)) !== JSON.stringify(planOf("", opening));
  // WHAT WAS LAID OUT EXISTS NOWHERE ELSE until the deck is made, so leaving
  // asks first: a link, Back, a reload, and Cancel, whose replace no guard
  // holds. The create's own move to the new deck is not a leave.
  useLeaveGuard(
    changed
      ? (_to, leave) => {
          if (creating.current) return false;
          setLeaving({ leave });
          return true;
        }
      : null,
  );

  // ASKING AN AGENT drafts the same deck: the page makes it from the plan,
  // shared with the agent as an editor so its turns can publish versions of
  // it, then files the agent a task the deck is linked to, whose description
  // carries the brief and the slides it starts from. The reader lands on the
  // task, where the deck is listed and the agent's progress is told.
  const panel = useAskPanel();
  const who = useWhoBuildsIt("slides");
  const files = useFileArtifactTask();
  const make = useKeyedGesture();
  const nav = useNavigator();
  const toast = useToast();
  // A DECK MADE FOR A TASK THAT WAS THEN REFUSED is kept, and the next press
  // files the task for it rather than making a second deck.
  const [made, setMade] = useState<{ id: string; seat: string } | null>(null);
  const agent = chosenAgent(who)?.seat;

  const hold = titleHold(title) ?? planHold(rows);
  const askHold =
    files.held ??
    (brief.trim() === ""
      ? "Say what the deck is for."
      : (askTitleHold(title, "deck") ??
        planHold(rows) ??
        (!who.seat
          ? "Add an agent to the chart first."
          : !who.project
            ? "Choose the project to file the task in."
            : undefined)));
  const busy = create.busy || make.busy || files.busy;
  // WHAT IS OPEN, for the assistant: the title, the theme and each slide's
  // title, and the hand-off beside them, set through the composer's own
  // setters; making the deck stays the reader's press.
  useAssistantDraft(
    createDraft<DeckDraft>({
      what: "slide deck",
      help:
        "You can set the deck's theme and each slide's title. Adding, moving or removing " +
        "slides and changing their layouts are the person's, on the page; you make no images.",
      step: panel.open ? "ask" : "compose",
      title,
      setTitle,
      brief,
      setBrief,
      who,
      extra: {
        value: { theme: themeId, rows },
        fields: deckFields(themes, layouts, rows),
        apply: (next) => {
          setThemeId(next.theme);
          setRows([...next.rows]);
        },
      },
      blocked: () => (busy ? "The deck is being made." : null),
    }),
  );
  const handed = `${brief.trim()}\n\n${planBrief(theme, rows, title, layouts)}`;

  const start = async () => {
    if (hold || busy) return;
    creating.current = true;
    try {
      await create.fromStarter("slides", title, { deck: planOf(theme.id, rows) });
    } finally {
      creating.current = false;
    }
  };

  const ask = async () => {
    if (askHold || busy || !agent) return;
    let id = made && made.seat === agent.handle ? made.id : "";
    if (!id) {
      const answer = await make.run(
        createFromStarter({
          kind: "slides",
          title: title.trim(),
          deck: planOf(theme.id, rows),
          shares: [{ audience: { kind: "seat", id: agent.handle }, role: "editor" }],
          ...(folder ? { folder } : {}),
        }),
      );
      id =
        answer?.kind === "done" && typeof answer.body.artifact === "string"
          ? answer.body.artifact
          : "";
      if (!id) return;
      setMade({ id, seat: agent.handle });
    }
    const filed = await files.fileArtifactTask({
      kind: "slides",
      title: title.trim(),
      brief: handed,
      seat: agent.handle,
      project: who.project,
      artifact: id,
    });
    if (!filed) return;
    if (typeof filed.linked === "string") {
      toast.show({
        variant: "warning",
        title: "The deck is not on the task yet",
        message: filed.linked,
      });
    }
    creating.current = true;
    nav.to(filed.item ? ["worklet", filed.item] : artifactPath(id));
  };

  const add = (layout: string) => {
    const next = addAfter(rows, current?.key ?? "", layout);
    setRows(next);
    setSelected(next.find((r) => !rows.includes(r))?.key ?? selected);
  };
  const newSlide = (
    <Menu
      label="New slide"
      trigger="New slide"
      icon={<PlusGlyph size="sm" />}
      triggerVariant="ghost"
      items={starters.layouts.map((l) => ({
        key: l.id,
        label: l.name,
        description: l.description,
        onSelect: () => add(l.id),
      }))}
    />
  );

  const strip = (
    <SlideFilmstrip
      className="slides-create-strip"
      orientation={stripInMain ? "horizontal" : "vertical"}
      slides={slides.map((s) => ({ id: s.id, title: s.title }))}
      selected={current?.key ?? ""}
      onSelect={setSelected}
      renderSlide={(id) => {
        const s = slides.find((x) => x.id === id);
        return s ? renderSlide(s, { prefix: `new-t-${id}-`, image }) : null;
      }}
      onReorder={(keys) => setRows(reorder(rows, keys))}
      onDuplicate={(key) => {
        const next = duplicate(rows, key);
        setRows(next);
        setSelected(next[next.findIndex((r) => r.key === key) + 1]?.key ?? key);
      }}
      onDelete={(key) => {
        if (rows.length <= 1) return;
        const out = remove(rows, key);
        setRows(out.rows);
        setSelected(out.select);
      }}
      footer={newSlide}
      labels={{ list: "Slides to start with" }}
    />
  );

  const titleWrong = titleProblem(title);
  const slideWrong = current ? slideTitleProblem(current.title) : undefined;
  const layout = current ? layouts.get(current.layout) : undefined;

  return (
    <CreateShell
      kind="slides"
      {...(changed ? { onCancel: () => setLeaving({ leave }) } : {})}
      // ON A PHONE THE BAR KEEPS ITS WIDTH FOR THE NAME AND THE ACTION: the
      // count is said under the stage there.
      status={
        phone
          ? undefined
          : `${rows.length === 1 ? "1 slide" : `${rows.length} slides`} · ${theme.name}`
      }
      action={{
        label: "Create deck",
        icon: <PresentationGlyph size="sm" />,
        onSelect: () => void start(),
        ...(hold ? { disabledReason: hold } : busy ? { disabledReason: "Creating the deck." } : {}),
      }}
      handoff={{
        tool: askTool(who, panel),
        open: () => panel.setOpen(true),
        panel: panel.open ? (
          <AskAgentPanel
            panel={panel}
            who={who}
            intro={`The deck is made from the slides you laid out and shared with ${agent?.name ?? "the agent"}, who writes it from your brief.`}
            brief={brief}
            onBrief={setBrief}
            briefLabel="What is the deck for?"
            briefPlaceholder="A ten-minute board update on what changed this quarter."
            briefHelper="Who it is for, what it should say and where the material is. The agent also reads the slides you laid out."
            briefMaxLength={BRIEF_MAX}
            starters={DECK_BRIEFS}
            carried={[
              ...(title.trim()
                ? [
                    {
                      key: "title",
                      icon: <FileTextGlyph size="sm" />,
                      label: `“${title.trim()}”`,
                      detail: "The deck's title",
                    },
                  ]
                : []),
              {
                key: "slides",
                icon: <PresentationGlyph size="sm" />,
                label: rows.length === 1 ? "1 slide" : `${rows.length} slides`,
                detail: rows.map((r) => layouts.get(r.layout)?.name ?? r.layout).join(", "),
              },
              {
                key: "theme",
                icon: <PaletteGlyph size="sm" />,
                label: `The ${theme.name} theme`,
              },
            ]}
            task={{
              title: title.trim() || "Untitled deck",
              body: artifactTaskBody("slides", handed, made?.id ?? "created when you send"),
            }}
            steps={[
              `The deck is made from your slides and shared with ${agent?.name ?? "the agent"} as an editor.`,
              `A task is filed to ${agent?.name ?? "the agent"}${who.project ? ` in ${who.project}` : ""}, with the deck linked.`,
              `${agent?.name ?? "The agent"} writes the slides and publishes each change as a version.`,
            ]}
            notice={
              <>
                {make.answer && make.answer.kind !== "done" ? (
                  <Callout variant="danger" title="The deck was not created">
                    {make.answer.text}
                  </Callout>
                ) : null}
                {files.refusal ? (
                  <Callout
                    variant="danger"
                    title={
                      made
                        ? "The deck was made, and the task was not filed"
                        : "The task was not filed"
                    }
                  >
                    {files.refusal.sentence}
                    {files.refusal.hint ? ` ${files.refusal.hint}` : ""}
                    {made && (
                      <>
                        {" "}
                        <Button
                          size="small"
                          variant="ghost"
                          onClick={() => nav.to(artifactPath(made.id))}
                        >
                          Open the deck
                        </Button>
                      </>
                    )}
                  </Callout>
                ) : null}
              </>
            }
            sendLabel={(n) => (made ? `Ask ${n}` : `Create and ask ${n}`)}
            onSend={() => void ask()}
            hold={askHold}
            busy={make.busy || files.busy}
          />
        ) : null,
      }}
    >
      <div ref={root} className="slides-create" data-arrangement={arrangement}>
        {!stripInMain && strip}
        <div className="slides-create-main">
          <FormField error={titleWrong}>
            {(field) => (
              <DocumentTitleField
                autoFocus
                id={field.id}
                aria-describedby={field.describedBy}
                aria-label="Title"
                error={field.invalid}
                autoComplete="off"
                value={title}
                placeholder="Untitled deck"
                onChange={(e) => setTitle(e.target.value)}
              />
            )}
          </FormField>
          {currentSlide && (
            <div className="slides-create-stage">
              <SlideStage
                framed
                label={`Slide ${position + 1} of ${rows.length}, ${currentSlide.title || "Untitled"}`}
              >
                {renderSlide(currentSlide, { prefix: "new-s-", image })}
              </SlideStage>
            </div>
          )}
          <p className="slides-create-caption">
            <span className="slides-create-position">
              Slide {position + 1} of {rows.length}
              {layout ? ` · ${layout.name}` : ""}
            </span>
            <span className="slides-create-hint">
              {panel.open
                ? "The agent starts from these slides and writes them from your brief."
                : "Every word is yours to change once the deck opens."}
            </span>
          </p>
          {stripInMain && strip}
          {create.refusal && (
            <Callout variant="danger" title="The deck was not created">
              {create.refusal}
            </Callout>
          )}
        </div>
        <aside className="slides-create-side" aria-label="Deck settings">
          <section className="slides-create-group" aria-labelledby="slides-create-theme">
            <h2 id="slides-create-theme" className="slides-create-heading">
              Theme
            </h2>
            <ThemeGallery<string>
              label="Theme"
              value={theme.id}
              onValueChange={setThemeId}
              themes={themes.map((t) =>
                t.id === BRAND_DECK_THEME
                  ? {
                      id: t.id,
                      name: adoption?.document.name ?? t.name,
                      tag: "Your company brand",
                      // THE COMPANY'S OWN THEME SAYS SO ON ITS TILE, in the
                      // accent's pill; the product's themes keep their tag
                      // in the accessible name alone.
                      tagTone: "brand",
                      group: "",
                    }
                  : { id: t.id, name: t.name, tag: t.tag, group: "" },
              )}
              groups={[]}
              columns={2}
              renderPreview={(t) => (
                <ThemeCover
                  theme={starters.themes.find((x) => x.id === t.id)!}
                  title={title}
                  assets={assets}
                />
              )}
            />
          </section>
          {current && (
            <section className="slides-create-group" aria-labelledby="slides-create-slide">
              <h2 id="slides-create-slide" className="slides-create-heading">
                Slide {position + 1}
              </h2>
              <FormField label="Layout">
                {(field) => (
                  <Select
                    id={field.id}
                    width="full"
                    value={current.layout}
                    options={starters.layouts.map((l) => ({
                      value: l.id,
                      label: l.name,
                      description: l.description,
                    }))}
                    onChange={(v) => setRows(update(rows, current.key, { layout: String(v) }))}
                  />
                )}
              </FormField>
              {current.layout === COVER_LAYOUT ? (
                <p className="slides-create-note">The cover shows the deck's title.</p>
              ) : (
                <FormField label="Slide title" error={slideWrong}>
                  {(field) => (
                    <Input
                      id={field.id}
                      aria-describedby={field.describedBy}
                      aria-invalid={field.invalid}
                      width="full"
                      value={current.title}
                      placeholder={layout?.heading ?? ""}
                      onChange={(e) =>
                        setRows(update(rows, current.key, { title: e.target.value }))
                      }
                    />
                  )}
                </FormField>
              )}
              <p className="slides-create-keys">
                <Kbd keys={["Mod", "D"]} /> duplicates the slide in the strip,{" "}
                <Kbd keys={[isApplePlatform() ? "Backspace" : "Delete"]} /> removes it.
              </p>
            </section>
          )}
          {workItem && <p className="slides-create-note">It will be listed on {workItem}.</p>}
        </aside>
      </div>
      <ConfirmModal
        open={leaving !== null}
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving;
          setLeaving(null);
          go?.leave();
        }}
        title="Discard this deck?"
        message="Its title, its theme and the slides you laid out are not saved anywhere yet."
        cancelLabel="Keep editing"
        confirmLabel="Discard"
        destructive
      />
    </CreateShell>
  );
}

/** A theme's card: its cover drawn by the engine, with the title being typed. */
function ThemeCover({
  theme,
  title,
  assets,
}: {
  theme: DeckTheme;
  title: string;
  assets: BrandAssets;
}) {
  const slide = previewSlide(
    theme,
    { key: `theme-${theme.id}`, layout: COVER_LAYOUT, title: "" },
    title || theme.name,
    coverLayout,
  );
  if (!slide) return null;
  return (
    <SlideStage label={`${theme.name} cover`}>
      {renderSlide(slide, { prefix: `new-th-${theme.id}-`, image: previewImages(theme, assets) })}
    </SlideStage>
  );
}

/** The cover's layout as the plan reads it: its title is the deck's. */
const coverLayout: DeckLayout = { id: COVER_LAYOUT, name: "Cover", description: "", heading: "" };
