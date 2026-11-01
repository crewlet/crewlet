/**
 * A slide deck, drawn here: Edit (or read), Grid, Present, the Presenter
 * window, and print to a 16:9 PDF.
 *
 * NATIVE, NEVER FRAMED. A deck runs no author code (D57): the engine answers
 * each version as the canonical tree its publish was held to, and
 * `lib/slides.tsx` draws it with elements it constructs itself, so nothing a
 * slide carries can become script on this origin.
 *
 * WHERE THE READER IS, IN THE ADDRESS. The slide on show is `slide=<id>` and
 * the view is `mode=` (grid, present, presenter), both queries on the
 * artifact's own address, so a link opens on a slide and following somebody
 * (`follow=`) moves this reader to the slide they are on: the slide is said to
 * the room as the region `slide:<id>`, which is how a phone follows the
 * presenter with no new transport.
 *
 * A PERSON'S EDIT IS A VERSION (D69): text in place, notes, order, deleting a
 * slide and the deck's title make a draft, and Save publishes it against the
 * version it started from. A save refused because somebody published since
 * is merged onto their version slide by slide (`lib/artifacts/deck.ts`); what
 * both changed differently is the person's to resolve before saving again.
 *
 * THE PRESENTER WINDOW IS THE DASHBOARD'S OWN, opened by the reader's press,
 * never by artifact code: the same artifact address with `mode=presenter`.
 * The two windows keep one position over a `BroadcastChannel`, so pressing
 * next at the lectern moves the room's screen.
 */

import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import {
  announce,
  Button,
  Callout,
  ConflictNote,
  Eyebrow,
  FormField,
  IconButton,
  Input,
  Menu,
  PresenterView,
  Skeleton,
  SlideFilmstrip,
  SlideGrid,
  SlideNav,
  SlideStage,
  Textarea,
  useFullscreen,
  useIdle,
  useSlideKeys,
  useToast,
  type SlideGridSection,
  useScreenBelow,
} from "@crewlethq/ui";
import type {
  ArtifactDeck,
  ArtifactDeckAnswer,
  ArtifactDetail,
  DeckSlide,
} from "~/protocol/index.ts";
import { newActOpID } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { PrintOnly } from "~/components/PrintOnly.tsx";
import { useParam } from "~/app/router.tsx";
import { useRest } from "~/lib/useRest.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import {
  publishVersion,
  readDeck,
  readSlidesStarters,
  uploadTexts,
  versionText,
} from "~/lib/artifacts/api.ts";
import { sampleSlide } from "~/lib/artifacts/deckPlan.ts";
import { useDeckAssets } from "~/lib/artifacts/deckAssets.ts";
import {
  addedFiles,
  changed,
  changesOf,
  deckSlide,
  draftOf,
  duplicateSlide,
  insertSlide,
  mergeDeck,
  publishOf,
  removeSlide,
  resolve,
  sourcesNeeded,
  type DeckConflict,
  type DeckDraft,
  type DeckMerge,
} from "~/lib/artifacts/deck.ts";
import {
  nodeAt,
  renderSlide,
  titlePath,
  withText,
  type SlideRenderContext,
} from "~/lib/slides.tsx";
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { SLIDE_TRANSITIONS } from "~/contract/artifacts.ts";
import {
  EyeOffGlyph,
  FileTextGlyph,
  MaximizeGlyph,
  MinimizeGlyph,
  MonitorGlyph,
  PlusGlyph,
} from "@crewlethq/icons/glyphs";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import type { PresenceReport } from "./presence.ts";
import { useKindControls, useRegionNames } from "~/lib/artifacts/kindControls.ts";

/** The views of a deck, as `mode=` names them; "" is the editor (or the reader's slides). */
type DeckMode = "" | "grid" | "present" | "presenter";

const MODES: readonly DeckMode[] = ["", "grid", "present", "presenter"];

/** A presence region naming the slide a member is on. */
export function slideRegion(id: string): string {
  return `slide:${id}`;
}

export function SlidesView({
  detail,
  version,
  presence,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  presence: PresenceReport;
  /** Absent for a secondary drawing (a side of a compare): read only, and silent to the room. */
  onPublished?: ((version: number) => void) | undefined;
}) {
  const deck = useRest<ArtifactDeckAnswer>(
    version > 0 ? `deck:${detail.id}:${version}` : null,
    (signal) => readDeck(detail.id, version, signal),
  );
  if (!deck.data) {
    return deck.loading ? (
      <Skeleton variant="text" rows={6} label={`Loading ${detail.title}`} />
    ) : (
      <QueryState error={deck.code} refusal={deck.refusal} loading={false} />
    );
  }
  return (
    <Deck
      key={`${detail.id}:${version}`}
      detail={detail}
      version={version}
      deck={deck.data.deck}
      presence={presence}
      {...(onPublished ? { onPublished } : {})}
    />
  );
}

function Deck({
  detail,
  version,
  deck,
  presence,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  deck: ArtifactDeck;
  presence: PresenceReport;
  onPublished?: ((version: number) => void) | undefined;
}) {
  const primary = onPublished !== undefined;
  const [modeParam, setMode] = useParam("mode", "");
  const mode: DeckMode = MODES.includes(modeParam as DeckMode) ? (modeParam as DeckMode) : "";
  const [slideParam, setSlide] = useParam("slide", "");
  const publish = useArtifactAccess({ app: true, role: "editor", held: detail.role });

  const base = useMemo(() => draftOf(deck, version), [deck, version]);
  const [draft, setDraft] = useState<DeckDraft | null>(null);
  const [merge, setMerge] = useState<DeckMerge | null>(null);
  // ONLY THE HEAD IS EDITED, since a save is a version based on the head and
  // an older version is history to read or restore. A draft already open
  // stays open when somebody else publishes: its save is refused and merged
  // onto their version, rather than the editor being taken from under it.
  const editable = primary && publish.can && (version === detail.head_number || draft !== null);
  const shown = draft ?? base;
  const dirty = draft !== null && changed(changesOf(merge?.live ?? base, draft));

  const slides = useMemo(() => shown.order.map((id) => deckSlide(shown.slides[id]!)), [shown]);
  const current = slides.find((s) => s.id === slideParam) ?? slides[0]!;

  // WHAT IS OPEN, for the assistant, while the deck is edited: its title and
  // each slide's title and speaker notes, over the version the edit is based
  // on, set through the draft its own controls write; Save stays the
  // editor's. Read or presented, the deck holds no draft.
  useAssistantDraft<DeckDraft>(
    primary && editable && mode === ""
      ? {
          kind: "client",
          id: detail.id,
          revision: shown.base,
          step: `slides.${shown.order.indexOf(current.id) + 1}`,
          help:
            "You can set the deck's title and each slide's title and speaker notes. Other " +
            "text is edited on the stage; adding, moving, hiding and deleting slides are the " +
            "person's, and you make no images. Saving makes a new version as them.",
          scope: `Your edit of “${detail.title}” (not saved)`,
          value: shown,
          fields: deckEditFields(shown.order),
          apply: setDraft,
          blocked: () =>
            merge && merge.conflicts.length > 0 ? "Settle where both edits differ first." : null,
        }
      : null,
  );
  const image = useDeckAssets(detail.id, version, deck);

  // WHERE THIS READER IS, said to the room: the slide, and whether they are
  // drafting a change to it.
  const { at } = presence;
  useEffect(() => {
    if (primary) at({ region: slideRegion(current.id) }, dirty);
  }, [at, primary, current.id, dirty]);
  // AND WHAT THOSE REGIONS ARE CALLED, so the room says "Viewing slide 3"
  // rather than the slide's id.
  useRegionNames(
    primary
      ? Object.fromEntries(slides.map((s, i) => [slideRegion(s.id), `slide ${i + 1}`]))
      : null,
  );

  const go = useCallback(
    (id: string) => setSlide(id === slides[0]?.id ? "" : id),
    [setSlide, slides],
  );
  const channel = useShowChannel(detail.id, go);
  const goAndTell = useCallback(
    (id: string) => {
      go(id);
      channel.post(id);
    },
    [go, channel],
  );

  // THE DECK'S VIEWS AND ITS TWO ACTIONS ARE THE PAGE BAR'S (`lib/artifacts/kindControls.ts`):
  // Edit, Grid and Present in its middle, the presenter's window and Print in
  // its More. A side of a comparison publishes none.
  useKindControls(
    primary
      ? {
          modes: {
            label: "View",
            value: mode === "presenter" ? "present" : mode,
            options: [
              { value: "", label: editable ? "Edit" : "Slides" },
              { value: "grid", label: "Grid" },
              { value: "present", label: "Present" },
            ],
            onValueChange: (v: string) => setMode(v),
          },
          menu: [
            {
              key: "presenter",
              label: "Open the presenter view",
              icon: <MonitorGlyph size="sm" />,
              onSelect: () => openPresenter(current.id),
            },
            {
              key: "print",
              label: "Print or save as PDF",
              icon: <FileTextGlyph size="sm" />,
              onSelect: () => window.print(),
            },
          ],
        }
      : null,
  );

  return (
    <div className="slides-view">
      {mode === "" && (
        <DeckEditor
          detail={detail}
          theme={deck.theme ?? ""}
          base={base}
          shown={shown}
          slides={slides}
          current={current}
          editable={editable}
          dirty={dirty}
          merge={merge}
          image={image}
          onSelect={goAndTell}
          onDraft={setDraft}
          onChange={(fn) => setDraft((d) => fn(d ?? base))}
          onMerge={setMerge}
          onPublished={(n) => {
            setDraft(null);
            setMerge(null);
            onPublished?.(n);
          }}
        />
      )}
      {mode === "grid" && (
        <SlideGrid
          sections={sectionsOf(deck, slides)}
          renderSlide={(id) =>
            renderSlide(
              slides.find((s) => s.id === id)!,
              { prefix: `g-${id}-`, image },
            )
          }
          current={current.id}
          onOpen={(id) => {
            goAndTell(id);
            setMode("");
          }}
        />
      )}
      {mode === "present" && (
        <DeckShow
          slides={slides}
          current={current}
          image={image}
          onGo={goAndTell}
          onExit={() => setMode("")}
        />
      )}
      {mode === "presenter" && (
        <DeckPresenter slides={slides} current={current} image={image} onGo={goAndTell} />
      )}
      {primary && <DeckPrint title={deck.title} slides={slides} image={image} />}
    </div>
  );
}

/** The deck's slides by its outline's sections, for the grid. */
function sectionsOf(deck: ArtifactDeck, slides: readonly DeckSlide[]): SlideGridSection[] {
  const starts = new Map((deck.sections ?? []).map((s) => [s.start, s.description]));
  const out: SlideGridSection[] = [];
  for (const s of slides) {
    if (out.length === 0 || starts.has(s.id))
      out.push({ title: starts.get(s.id) ?? "", slides: [] });
    (out[out.length - 1]!.slides as { id: string; title: string; hidden?: boolean }[]).push({
      id: s.id,
      title: s.title,
      hidden: !!s.hidden,
    });
  }
  return out;
}

/** Open the presenter's window: this artifact's own address with `mode=presenter`, by the reader's press. */
function openPresenter(slide: string) {
  const url = new URL(window.location.href);
  const [path, query = ""] = url.hash.slice(1).split("?");
  const params = new URLSearchParams(query);
  params.set("mode", "presenter");
  params.set("slide", slide);
  params.delete("follow");
  url.hash = `${path}?${params.toString()}`;
  window.open(url.toString(), "crewlet-presenter", "popup,width=1280,height=800");
}

/**
 * The two windows of one show keep one position: a move in either is posted
 * to the other. Same browser, same origin, nothing over the network.
 */
function useShowChannel(
  artifact: string,
  onGo: (id: string) => void,
): { post: (id: string) => void } {
  const ref = useRef<BroadcastChannel | null>(null);
  const goRef = useRef(onGo);
  goRef.current = onGo;
  useEffect(() => {
    if (typeof BroadcastChannel === "undefined") return undefined;
    const ch = new BroadcastChannel(`crewlet-slides:${artifact}`);
    ch.onmessage = (event: MessageEvent<unknown>) => {
      const data = event.data as { slide?: unknown } | null;
      if (data && typeof data.slide === "string") goRef.current(data.slide);
    };
    ref.current = ch;
    return () => {
      ch.close();
      ref.current = null;
    };
  }, [artifact]);
  return useMemo(() => ({ post: (id: string) => ref.current?.postMessage({ slide: id }) }), []);
}

// ---- what the assistant reads and sets ------------------------------------ //

/** The most a deck's title and a slide's notes hold, as their fields hold them. */
const DECK_TITLE_MAX = 120;
const NOTES_MAX = 4000;

/**
 * The fields of a deck's edit: its title, and each slide's title (the first
 * run of its heading, as the engine reads a slide's title) and speaker
 * notes, by the slide's place in the order.
 */
export function deckEditFields(order: readonly string[]): DraftFieldSpec<DeckDraft>[] {
  const slideAt = (d: DeckDraft, i: number) => d.slides[order[i]!];
  return [
    {
      path: "title",
      label: "Deck title",
      read: (d) => d.title,
      write: (d, title) =>
        typeof title !== "string"
          ? refuse("The deck's title is a line of text.")
          : [...title].length > DECK_TITLE_MAX
            ? refuse(`The deck's title holds at most ${DECK_TITLE_MAX} characters.`)
            : { ...d, title },
    },
    ...order.flatMap((id, i): DraftFieldSpec<DeckDraft>[] => [
      {
        path: `slides.${i + 1}.title`,
        label: `Slide ${i + 1} title`,
        read: (d) => {
          const slide = slideAt(d, i);
          const at = slide ? titlePath(slide.root) : null;
          return slide && at ? (nodeAt(slide.root, at)?.text ?? "") : "";
        },
        write: (d, title) => {
          const slide = slideAt(d, i);
          if (!slide) return refuse("That slide is no longer in the deck.");
          if (typeof title !== "string") return refuse("A slide's title is a line of text.");
          const at = titlePath(slide.root);
          if (!at) return refuse(`Slide ${i + 1} has no title; its text is edited on the stage.`);
          return {
            ...d,
            slides: { ...d.slides, [id]: { ...slide, root: withText(slide.root, at, title) } },
          };
        },
      },
      {
        path: `slides.${i + 1}.notes`,
        label: `Slide ${i + 1} speaker notes`,
        hint: "Everyone who can open the deck can read them",
        read: (d) => slideAt(d, i)?.notes ?? "",
        write: (d, notes) => {
          const slide = slideAt(d, i);
          if (!slide) return refuse("That slide is no longer in the deck.");
          if (typeof notes !== "string") return refuse("Speaker notes are text.");
          if ([...notes].length > NOTES_MAX) {
            return refuse(`Speaker notes hold at most ${NOTES_MAX} characters.`);
          }
          return { ...d, slides: { ...d.slides, [id]: { ...slide, notes } } };
        },
      },
    ]),
  ];
}

// ---- the editor ----------------------------------------------------------- //

function DeckEditor({
  detail,
  theme,
  base,
  shown,
  slides,
  current,
  editable,
  dirty,
  merge,
  image,
  onSelect,
  onDraft,
  onChange,
  onMerge,
  onPublished,
}: {
  detail: ArtifactDetail;
  /** The theme the deck's index names, whose layouts New slide adds from; "" for none. */
  theme: string;
  base: DeckDraft;
  shown: DeckDraft;
  slides: readonly DeckSlide[];
  current: DeckSlide;
  editable: boolean;
  dirty: boolean;
  merge: DeckMerge | null;
  image: (path: string) => string | null;
  onSelect: (id: string) => void;
  onDraft: (d: DeckDraft | null) => void;
  /**
   * Change the draft as it stands when the change lands, not as this render
   * saw it: a control that keeps its callback (the strip's reorder) must not
   * write back a draft older than an edit made since.
   */
  onChange: (fn: (d: DeckDraft) => DeckDraft) => void;
  onMerge: (m: DeckMerge | null) => void;
  onPublished: (version: number) => void;
}) {
  const toast = useToast();
  const save = useKeyedGesture();
  const [label, setLabel] = useState("");
  const [saving, setSaving] = useState(false);
  const prefix = useId().replace(/:/g, "");
  const position = slides.findIndex((s) => s.id === current.id);
  // ON A PHONE THE STRIP IS A ROW UNDER THE STAGE, where the next slide is a
  // swipe away, rather than a column of full-width slides under the notes.
  const phone = useScreenBelow(PHONE_BREAKPOINT);

  const change = onChange;
  const edit: SlideRenderContext["edit"] = editable
    ? {
        onText: (path, text) => {
          const id = current.id;
          change((d) => ({
            ...d,
            slides: {
              ...d.slides,
              [id]: { ...d.slides[id]!, root: withText(d.slides[id]!.root, path, text) },
            },
          }));
        },
      }
    : undefined;

  // A DECK GROWS IN THE STRIP, as a presentation editor's does: New slide at
  // its end adds one from the layouts of the theme the deck's index names,
  // drawn by the engine (`GET /artifacts/starters/slides`) as the slides it
  // began with were; a deck that names none (written by hand or by an agent)
  // adds a copy of the slide on the stage, as Keynote adds from the current
  // slide's master. Command+D copies and Delete removes, on a thumbnail.
  const starters = useRest(editable && theme ? "slides-starters" : null, (signal) =>
    readSlidesStarters(signal),
  );
  const themed = starters.data?.themes.find((t) => t.id === theme);
  const nameOf = (id: string) => {
    const i = shown.order.indexOf(id);
    return `slide ${i + 1}`;
  };
  // FROM THE DRAFT THIS RENDER SHOWS, since the new slide's id is the one the
  // stage moves to: a press is never a stale callback the way a kept one is.
  const grow = (fn: (d: DeckDraft) => { draft: DeckDraft; id: string }, said: string) => {
    const out = fn(shown);
    change(() => out.draft);
    onSelect(out.id);
    announce(said);
  };
  const deleteSlide = (id: string) => {
    if (shown.order.length <= 1) {
      announce("A deck keeps at least one slide.");
      return;
    }
    const { select } = removeSlide(shown, id);
    onSelect(select);
    change((d) => removeSlide(d, id).draft);
    announce(`Deleted ${nameOf(id)}.`);
  };
  const newSlide = themed ? (
    <Menu
      label="New slide"
      trigger="New slide"
      icon={<PlusGlyph size="sm" />}
      triggerVariant="ghost"
      items={(starters.data?.layouts ?? []).map((l) => ({
        key: l.id,
        label: l.name,
        description: l.description,
        onSelect: () => {
          const sample = sampleSlide(themed.sample, l.id);
          if (!sample) return;
          grow(
            (d) =>
              insertSlide(d, base, current.id, l.id, {
                root: sample.root,
                notes: sample.notes ?? "",
                hidden: false,
                transition: "",
              }),
            `Added a ${l.name} slide after ${nameOf(current.id)}.`,
          );
        },
      }))}
    />
  ) : (
    <Button
      size="small"
      variant="ghost"
      leadingIcon={<PlusGlyph size="sm" />}
      onClick={() =>
        grow(
          (d) => duplicateSlide(d, base, current.id),
          `Added a copy of ${nameOf(current.id)}, to change in place.`,
        )
      }
    >
      New slide
    </Button>
  );

  const saveNow = async () => {
    const against = merge?.live ?? base;
    setSaving(true);
    try {
      const sources: Record<string, string> = {};
      await Promise.all(
        sourcesNeeded(against, shown).map(async (path) => {
          sources[path] = await versionText(detail.id, against.base, path);
        }),
      );
      const added = addedFiles(against, shown);
      const brought = added.length > 0 ? await uploadTexts(detail.id, added, newActOpID()) : null;
      const answer = await save.run(
        publishVersion(
          detail.id,
          against.base,
          publishOf(against, shown, sources, label.trim(), brought),
        ),
      );
      if (answer?.kind === "done") {
        const n = Number(answer.body.number ?? against.base + 1);
        toast.ok(`Saved as version ${n}`);
        setLabel("");
        onPublished(n);
        return;
      }
      if (answer?.kind === "refused" && answer.error?.code === "version_moved") {
        // THE LIVE VERSION the refusal names (`live.number`), which the
        // draft is merged onto and the next save is based on.
        const live = answer.error.body.live as { number?: unknown } | undefined;
        const head = Number(live?.number);
        if (!Number.isInteger(head) || head < 1) {
          toast.show({ variant: "danger", title: "Could not save the deck", message: answer.text });
          return;
        }
        const newer = await readDeck(detail.id, head, new AbortController().signal);
        const merged = mergeDeck(against, shown, draftOf(newer.deck, head));
        onMerge(merged);
        onDraft(merged.draft);
        return;
      }
      if (answer)
        toast.show({ variant: "danger", title: "Could not save the deck", message: answer.text });
    } catch (err) {
      toast.show({
        variant: "danger",
        title: "Could not save the deck",
        message: err instanceof Error ? err.message : "The engine did not answer.",
      });
    } finally {
      setSaving(false);
    }
  };

  const strip = (
    <SlideFilmstrip
      className="slides-strip"
      orientation={phone ? "horizontal" : "vertical"}
      slides={slides.map((s) => ({ id: s.id, title: s.title, hidden: !!s.hidden }))}
      selected={current.id}
      onSelect={onSelect}
      renderSlide={(id) =>
        renderSlide(
          slides.find((s) => s.id === id)!,
          { prefix: `${prefix}-t-${id}-`, image },
        )
      }
      {...(editable
        ? {
            onReorder: (order: string[]) => change((d) => ({ ...d, order })),
            onDuplicate: (id: string) =>
              grow((d) => duplicateSlide(d, base, id), `Added a copy of ${nameOf(id)}.`),
            onDelete: deleteSlide,
            footer: newSlide,
          }
        : {})}
    />
  );

  return (
    <div className="slides-edit">
      {merge && (
        <MergeNote
          merge={merge}
          onResolve={(c, choice) => {
            const next = resolve(merge, c, choice);
            onMerge(next);
            onDraft(next.draft);
          }}
          onDismiss={() => onMerge(merge.conflicts.length === 0 ? null : merge)}
        />
      )}
      {!phone && strip}
      <div className="slides-main">
        <SlideStage
          label={`Slide ${position + 1} of ${slides.length}, ${current.title || "Untitled"}`}
          framed
        >
          {renderSlide(current, { prefix: `${prefix}-s-`, image, edit })}
        </SlideStage>
        <p className="slides-stage-caption">
          <span className="slides-stage-position">
            Slide {position + 1} of {slides.length}
          </span>
          {current.hidden && (
            <span className="slides-stage-hidden">
              <EyeOffGlyph size="sm" />
              Hidden when presenting
            </span>
          )}
        </p>
        {phone && strip}
        {editable ? (
          <div className="slides-panel">
            <FormField label="Speaker notes" helper="Everyone who can open the deck can read them.">
              {(field) => (
                <Textarea
                  id={field.id}
                  aria-describedby={field.describedBy}
                  value={current.notes ?? ""}
                  autoResize
                  maxLength={4000}
                  onChange={(e) => {
                    const notes = e.target.value;
                    const id = current.id;
                    change((d) => ({
                      ...d,
                      slides: { ...d.slides, [id]: { ...d.slides[id]!, notes } },
                    }));
                  }}
                />
              )}
            </FormField>
            <FormField label="Deck title">
              {(field) => (
                <Input
                  id={field.id}
                  aria-describedby={field.describedBy}
                  value={shown.title}
                  maxLength={120}
                  onChange={(e) => {
                    const title = e.target.value;
                    change((d) => ({ ...d, title }));
                  }}
                />
              )}
            </FormField>
            <div className="slides-actions">
              <Button
                size="small"
                variant="ghost"
                {...(slides.length <= 1
                  ? { disabledReason: "A deck keeps at least one slide." }
                  : {})}
                onClick={() => deleteSlide(current.id)}
              >
                Delete slide
              </Button>
              {dirty && (
                <>
                  <Input
                    aria-label="Version label"
                    placeholder="Label (optional)"
                    value={label}
                    maxLength={60}
                    onChange={(e) => setLabel(e.target.value)}
                  />
                  <Button
                    size="small"
                    variant="ghost"
                    onClick={() => {
                      onDraft(null);
                      onMerge(null);
                    }}
                  >
                    Discard
                  </Button>
                  <Button
                    size="small"
                    variant="primary"
                    loading={saving}
                    {...(merge && merge.conflicts.length > 0
                      ? { disabledReason: "Resolve each change both of you made first." }
                      : {})}
                    onClick={() => void saveNow()}
                  >
                    Save version
                  </Button>
                </>
              )}
            </div>
            <p className="artifact-note">
              Click any text on the slide to change it. New slide, at the end of the strip, adds one
              after this slide; on a thumbnail, Command or Control+D copies it and Delete removes
              it. Drag a slide, or use the arrow keys on its handle, to move it. Saving publishes a
              new version.
            </p>
          </div>
        ) : (
          current.notes && (
            <section className="slides-notes" aria-label="Speaker notes">
              <Eyebrow as="p">Speaker notes</Eyebrow>
              <p className="slides-notes-body">{current.notes}</p>
            </section>
          )
        )}
      </div>
    </div>
  );
}

/** What merging onto the newer version did, and each change both sides made, to resolve. */
function MergeNote({
  merge,
  onResolve,
  onDismiss,
}: {
  merge: DeckMerge;
  onResolve: (c: DeckConflict, choice: "mine" | "theirs") => void;
  onDismiss: () => void;
}) {
  const name = (c: DeckConflict) =>
    c.kind === "slide"
      ? `the slide "${c.title}"`
      : c.kind === "order"
        ? "the slide order"
        : "the deck's title";
  if (merge.conflicts.length === 0) {
    return (
      <Callout
        className="slides-merged"
        variant="info"
        title={`Moved onto version ${merge.live.base}`}
        action={
          <Button size="small" variant="ghost" onClick={onDismiss}>
            Dismiss
          </Button>
        }
      >
        Somebody published while you were editing. Their changes (
        {merge.theirs.length === 0 ? "none to slides" : merge.theirs.join(", ")}) are kept beside
        yours. Save again to publish both.
      </Callout>
    );
  }
  return (
    <div className="slides-conflicts">
      {merge.conflicts.map((c) => (
        <ConflictNote
          key={c.kind === "slide" ? c.id : c.kind}
          subject={name(c)}
          message={`Version ${merge.live.base} changed ${name(c)} too. Keep your change, or take theirs.`}
          actions={
            <>
              <Button size="small" variant="secondary" onClick={() => onResolve(c, "mine")}>
                Keep mine
              </Button>
              <Button size="small" variant="ghost" onClick={() => onResolve(c, "theirs")}>
                Take theirs
              </Button>
            </>
          }
        />
      ))}
    </div>
  );
}

// ---- presenting ----------------------------------------------------------- //

/** The slides a show steps through: every slide that is not hidden, and the current one even if it is. */
function showable(slides: readonly DeckSlide[], current: DeckSlide): DeckSlide[] {
  return slides.filter((s) => !s.hidden || s.id === current.id);
}

function useShowKeys(
  slides: readonly DeckSlide[],
  current: DeckSlide,
  onGo: (id: string) => void,
  onExit?: () => void,
) {
  const shown = showable(slides, current);
  const at = shown.findIndex((s) => s.id === current.id);
  const to = (i: number) => {
    const s = shown[Math.max(0, Math.min(shown.length - 1, i))];
    if (s && s.id !== current.id) onGo(s.id);
  };
  useSlideKeys({
    onNext: () => to(at + 1),
    onPrevious: () => to(at - 1),
    onFirst: () => to(0),
    onLast: () => to(shown.length - 1),
    onExit,
  });
  // EACH SLIDE IS SAID as it arrives: its number and its title.
  useEffect(() => {
    announce(`Slide ${at + 1} of ${shown.length}, ${current.title || "Untitled"}`);
  }, [at, shown.length, current.title]);
  return { shown, at, to };
}

/**
 * Present, in this window: the slide as large as fits in the view, its foot
 * and the count under it on screen, and full screen on the reader's press.
 */
function DeckShow({
  slides,
  current,
  image,
  onGo,
  onExit,
}: {
  slides: readonly DeckSlide[];
  current: DeckSlide;
  image: (path: string) => string | null;
  onGo: (id: string) => void;
  onExit: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const fullscreen = useFullscreen(box);
  // THE SHOW TAKES THE KEYS as it opens: focus left on the control that
  // opened it would spend the arrows on that control instead of the slides.
  useEffect(() => {
    box.current?.focus({ preventScroll: true });
  }, []);
  const { shown, at, to } = useShowKeys(slides, current, onGo, () => {
    if (fullscreen.active) fullscreen.exit();
    else onExit();
  });
  const prefix = useId().replace(/:/g, "");
  // HOW THE LAST SLIDE LEFT is how the next one enters: its transition.
  const previous = useRef(current);
  const left = previous.current.id !== current.id ? (previous.current.transition ?? "") : "";
  // ONLY A TRANSITION THE FORMAT NAMES is played; anything else is a cut.
  const transition = (SLIDE_TRANSITIONS as readonly string[]).includes(left) ? left : "";
  useEffect(() => {
    previous.current = current;
  }, [current]);
  // FULL SCREEN IS THE ROOM'S: the controls and the cursor go once the
  // pointer rests, and come back on its next move.
  const idle = useIdle(box, fullscreen.active);
  return (
    <div
      className="slides-show"
      ref={box}
      role="region"
      aria-label="Slide show"
      tabIndex={-1}
      data-idle={idle || undefined}
    >
      {/* THE STAGE STAYS as the slides change, so the room it measured holds;
          only the slide inside it is new, and plays its entrance. */}
      <SlideStage
        fit="view"
        framed
        label={`Slide ${at + 1} of ${shown.length}, ${current.title || "Untitled"}`}
      >
        <div
          className="slides-show-slide"
          key={current.id}
          data-transition={transition || undefined}
        >
          {renderSlide(current, { prefix: `${prefix}-p-`, image })}
        </div>
      </SlideStage>
      <div className="slides-show-bar">
        <SlideNav
          position={at + 1}
          total={shown.length}
          onPrevious={() => to(at - 1)}
          onNext={() => to(at + 1)}
          tone={fullscreen.active ? "projection" : "page"}
        >
          {fullscreen.supported && (
            <IconButton
              label={fullscreen.active ? "Exit full screen" : "Full screen"}
              icon={fullscreen.active ? <MinimizeGlyph /> : <MaximizeGlyph />}
              variant="ghost"
              size="sm"
              onClick={fullscreen.toggle}
            />
          )}
        </SlideNav>
      </div>
    </div>
  );
}

/** The presenter's window: the room's slide, the next, the notes and the timer. */
function DeckPresenter({
  slides,
  current,
  image,
  onGo,
}: {
  slides: readonly DeckSlide[];
  current: DeckSlide;
  image: (path: string) => string | null;
  onGo: (id: string) => void;
}) {
  const { shown, at, to } = useShowKeys(slides, current, onGo);
  const prefix = useId().replace(/:/g, "");
  const next = shown[at + 1];
  return (
    <div className="slides-presenter">
      <PresenterView
        current={renderSlide(current, { prefix: `${prefix}-c-`, image })}
        currentTitle={current.title || "Untitled"}
        next={next ? renderSlide(next, { prefix: `${prefix}-n-`, image }) : null}
        nextTitle={next?.title ?? ""}
        notes={current.notes ?? ""}
        position={at + 1}
        total={shown.length}
        onNext={() => to(at + 1)}
        onPrevious={() => to(at - 1)}
      />
    </div>
  );
}

/**
 * Every slide the show would step through, one to a 16:9 page: what the
 * browser prints to PDF. Drawn always, shown only in print, so Print is the
 * browser's own dialog with nothing to prepare.
 */
function DeckPrint({
  title,
  slides,
  image,
}: {
  title: string;
  slides: readonly DeckSlide[];
  image: (path: string) => string | null;
}): ReactNode {
  const prefix = useId().replace(/:/g, "");
  return (
    <PrintOnly>
      {slides
        .filter((s) => !s.hidden)
        .map((s, i) => (
          <div className="slides-print-page" key={s.id}>
            <SlideStage scale={1} label={`${title}, slide ${i + 1}`}>
              {renderSlide(s, { prefix: `${prefix}-${s.id}-`, image })}
            </SlideStage>
          </div>
        ))}
    </PrintOnly>
  );
}
