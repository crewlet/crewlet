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
} from "@crewlethq/ui";
import type {
  ArtifactDeck,
  ArtifactDeckAnswer,
  ArtifactDetail,
  DeckSlide,
} from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { PrintOnly } from "~/components/PrintOnly.tsx";
import { useParam } from "~/app/router.tsx";
import { useRest } from "~/lib/useRest.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { publishVersion, readDeck, versionText } from "~/lib/artifacts/api.ts";
import { useDeckAssets } from "~/lib/artifacts/deckAssets.ts";
import {
  changed,
  changesOf,
  deckSlide,
  draftOf,
  mergeDeck,
  publishOf,
  resolve,
  sourcesNeeded,
  type DeckConflict,
  type DeckDraft,
  type DeckMerge,
} from "~/lib/artifacts/deck.ts";
import { renderSlide, withText, type SlideRenderContext } from "~/lib/slides.tsx";
import { SLIDE_TRANSITIONS } from "~/contract/artifacts.ts";
import {
  EyeOffGlyph,
  FileTextGlyph,
  MaximizeGlyph,
  MinimizeGlyph,
  MonitorGlyph,
} from "@crewlethq/icons/glyphs";
import { useMediaQuery } from "~/lib/media.ts";
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

// ---- the editor ----------------------------------------------------------- //

function DeckEditor({
  detail,
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
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);

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
      const answer = await save.run(
        publishVersion(detail.id, against.base, publishOf(against, shown, sources, label.trim())),
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
      {...(editable ? { onReorder: (order: string[]) => change((d) => ({ ...d, order })) } : {})}
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
                onClick={() => {
                  const gone = current.id;
                  const rest = shown.order.filter((id) => id !== gone);
                  onSelect(rest[Math.min(position, rest.length - 1)] ?? rest[0]!);
                  change((d) => ({ ...d, order: d.order.filter((id) => id !== gone) }));
                }}
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
              Click any text on the slide to change it. Drag a slide in the strip, or use the arrow
              keys on its handle, to move it. Saving publishes a new version.
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
