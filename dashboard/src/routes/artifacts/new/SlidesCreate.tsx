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

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
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
  useShortcut,
} from "@crewlethq/ui";
import { PlusGlyph, PresentationGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard } from "~/app/router.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { QueryState } from "~/components/common.tsx";
import { readSlidesStarters } from "~/lib/artifacts/api.ts";
import {
  COVER_LAYOUT,
  addAfter,
  duplicate,
  planHold,
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
import { useRest } from "~/lib/useRest.ts";
import type { DeckLayout, DeckSlide, DeckTheme, SlidesStartersAnswer } from "~/protocol/index.ts";
import { CreateShell, useLeaveCreate } from "./CreateShell.tsx";
import { titleHold, titleProblem, useCreateArtifact, useWorkItem } from "./create.ts";

/**
 * The theme a new deck opens on: Crewlet's own. The engine's default plan
 * keeps the look its starter always had, for a create that names none.
 */
const OPENING_THEME = "nightshade";

/** Nothing on a starter slide is a file of a version: no image to resolve. */
const noImage = () => null;

export function SlidesCreate() {
  const starters = useRest<SlidesStartersAnswer>("slides-starters", (signal) =>
    readSlidesStarters(signal),
  );
  if (!starters.data) {
    return (
      <CreateShell kind="slides">
        {starters.loading ? (
          <div className="slides-create" aria-busy="true">
            <div className="slides-create-strip">
              <Skeleton variant="list" rows={4} label="Loading themes and layouts" />
            </div>
            <div className="slides-create-main">
              <Skeleton variant="box" height="var(--size-control-md)" />
              <div className="slides-create-stage-hold">
                <Skeleton variant="box" height="100%" />
              </div>
            </div>
            <div className="slides-create-side">
              <Skeleton variant="grid" rows={3} columns={2} />
            </div>
          </div>
        ) : (
          <div className="slides-create-state">
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
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const opening = useMemo(() => rowsOf(starters.default), [starters.default]);
  const firstTheme =
    starters.themes.find((t) => t.id === OPENING_THEME)?.id ??
    starters.default.theme ??
    starters.themes[0]?.id ??
    "";

  const [title, setTitle] = useState("");
  const [themeId, setThemeId] = useState(firstTheme);
  const [rows, setRows] = useState<PlanRow[]>(opening);
  const [selected, setSelected] = useState(opening[0]?.key ?? "");
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);
  const creating = useRef(false);
  const leave = useLeaveCreate();
  const titleField = useRef<HTMLInputElement>(null);

  const theme = starters.themes.find((t) => t.id === themeId) ?? starters.themes[0]!;
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

  useEffect(() => {
    titleField.current?.focus();
  }, []);

  const hold = titleHold(title) ?? planHold(rows);
  const start = async () => {
    if (hold || create.busy) return;
    creating.current = true;
    try {
      await create.fromStarter("slides", title, planOf(theme.id, rows));
    } finally {
      creating.current = false;
    }
  };
  useShortcut({ keys: [{ key: "Enter", mod: true }], onKey: () => void start() });

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
      orientation={phone ? "horizontal" : "vertical"}
      slides={slides.map((s) => ({ id: s.id, title: s.title }))}
      selected={current?.key ?? ""}
      onSelect={setSelected}
      renderSlide={(id) => {
        const s = slides.find((x) => x.id === id);
        return s ? renderSlide(s, { prefix: `new-t-${id}-`, image: noImage }) : null;
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
        phone ? undefined : (
          <span className="slides-create-status">
            {rows.length === 1 ? "1 slide" : `${rows.length} slides`} · {theme.name}
          </span>
        )
      }
      action={{
        label: "Create deck",
        icon: <PresentationGlyph size="sm" />,
        onSelect: () => void start(),
        keys: ["Mod", "Enter"],
        ...(hold
          ? { disabledReason: hold }
          : create.busy
            ? { disabledReason: "Creating the deck." }
            : {}),
      }}
    >
      <div className="slides-create">
        {!phone && strip}
        <div className="slides-create-main">
          <FormField label="Title" error={titleWrong}>
            {(field) => (
              <Input
                ref={titleField}
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid}
                className="slides-create-title"
                width="full"
                value={title}
                placeholder="Quarterly plan"
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
                {renderSlide(currentSlide, { prefix: "new-s-", image: noImage })}
              </SlideStage>
            </div>
          )}
          <p className="slides-create-caption">
            <span className="slides-create-position">
              Slide {position + 1} of {rows.length}
            </span>
            <span>{layout?.name}</span>
            <span className="slides-create-hint">
              Every word is yours to change once the deck opens.
            </span>
          </p>
          {phone && strip}
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
              themes={starters.themes.map((t) => ({
                id: t.id,
                name: t.name,
                tag: t.tag,
                group: "",
              }))}
              groups={[]}
              columns={2}
              renderPreview={(t) => (
                <ThemeCover theme={starters.themes.find((x) => x.id === t.id)!} title={title} />
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
        title="Leave without creating the deck?"
        message="The title, theme and slides you chose will be lost."
        confirmLabel="Discard and leave"
        cancelLabel="Keep editing"
        destructive
      />
    </CreateShell>
  );
}

/** A theme's card: its cover drawn by the engine, with the title being typed. */
function ThemeCover({ theme, title }: { theme: DeckTheme; title: string }) {
  const slide = previewSlide(
    theme,
    { key: `theme-${theme.id}`, layout: COVER_LAYOUT, title: "" },
    title || theme.name,
    coverLayout,
  );
  if (!slide) return null;
  return (
    <SlideStage label={`${theme.name} cover`}>
      {renderSlide(slide, { prefix: `new-th-${theme.id}-`, image: noImage })}
    </SlideStage>
  );
}

/** The cover's layout as the plan reads it: its title is the deck's. */
const coverLayout: DeckLayout = { id: COVER_LAYOUT, name: "Cover", description: "", heading: "" };
