/**
 * A new deck, open on its cover: the engine's starter slides on the deck
 * editor's own stage and strip, every line editable in place by the shipped
 * slide renderer, so choosing Slides lands on a cover with its title ready
 * to type. The deck's title and the cover's heading are one value: typing
 * either changes both.
 */

import { useEffect, useId, useMemo, useRef } from "react";
import { Input, SlideFilmstrip, SlideStage, useMediaMatch } from "@crewlethq/ui";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import type { DeckSlide } from "~/protocol/index.ts";
import { renderSlide, titleOf, withText } from "~/lib/slides.tsx";
import { COVER_TITLE_PATH, starterSlides, type SlideTextEdit } from "./starterDeck.ts";

export interface DeckDraftState {
  title: string;
  /** Every line changed besides the cover's title, by slide and node path. */
  edits: SlideTextEdit[];
  /** The slide on the stage. */
  current: string;
}

export function deckDraftOf(title = ""): DeckDraftState {
  return { title, edits: [], current: "cover" };
}

/** The draft's slides: the starter under its title, each edit applied. */
export function draftSlides(draft: DeckDraftState): DeckSlide[] {
  const slides = starterSlides(draft.title || "Untitled deck");
  for (const e of draft.edits) {
    const s = slides[e.slide];
    if (s) {
      s.root = withText(s.root, e.path, e.text);
      s.title = titleOf(s.root);
    }
  }
  return slides;
}

const noImages = () => null;

export function SlidesCanvas({
  draft,
  onChange,
  showTitle = true,
  orientation = "vertical",
  className,
}: {
  draft: DeckDraftState;
  onChange: (next: DeckDraftState) => void;
  /** Whether the deck's title is a field above the stage, beside the cover's own heading. */
  showTitle?: boolean;
  orientation?: "vertical" | "horizontal";
  className?: string;
}) {
  const prefix = useId().replace(/:/g, "");
  // ON A PHONE THE STRIP IS A ROW UNDER THE STAGE, as the deck editor draws it.
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const strip = phone ? "horizontal" : orientation;
  const slides = useMemo(() => draftSlides(draft), [draft]);
  // WITH NO TITLE FIELD, THE COVER'S HEADING IS THE TITLE, and the caret
  // starts in it: a new deck opens on its cover ready to type.
  const stage = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (showTitle) return undefined;
    // THE STAGE DRAWS ITS SLIDE ONCE IT HAS MEASURED ITS BOX, and a dialog
    // around it places its own first focus as it opens, so the caret is
    // placed once the heading exists and after either has settled.
    let tries = 0;
    const timer = window.setInterval(() => {
      const first = stage.current?.querySelector<HTMLElement>("[contenteditable]");
      if (!first && ++tries < 40) return;
      window.clearInterval(timer);
      if (!first) return;
      first.focus();
      const range = document.createRange();
      range.selectNodeContents(first);
      const sel = window.getSelection();
      sel?.removeAllRanges();
      sel?.addRange(range);
    }, 50);
    return () => window.clearInterval(timer);
    // Once, on arrival: a later render must not take the caret back.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const index = Math.max(
    0,
    slides.findIndex((s) => s.id === draft.current),
  );
  const current = slides[index]!;
  const onText = (path: string, text: string) => {
    if (index === 0 && path === COVER_TITLE_PATH) {
      onChange({ ...draft, title: text });
      return;
    }
    const edits = draft.edits.filter((e) => !(e.slide === index && e.path === path));
    onChange({ ...draft, edits: [...edits, { slide: index, path, text }] });
  };
  return (
    <div className={["cf-deck", `cf-deck--${strip}`, className].filter(Boolean).join(" ")}>
      <SlideFilmstrip
        className="cf-deck-strip"
        orientation={strip}
        slides={slides.map((s) => ({ id: s.id, title: s.title }))}
        selected={current.id}
        onSelect={(id) => onChange({ ...draft, current: id })}
        renderSlide={(id) =>
          renderSlide(
            slides.find((s) => s.id === id)!,
            {
              prefix: `${prefix}-t-${id}-`,
              image: noImages,
            },
          )
        }
      />
      <div className="cf-deck-stage" ref={stage}>
        {showTitle && (
          <Input
            appearance="title"
            width="full"
            aria-label="Deck title"
            placeholder="Untitled deck"
            autoFocus
            value={draft.title}
            onChange={(e) => onChange({ ...draft, title: e.target.value })}
          />
        )}
        <SlideStage label={`Slide ${index + 1} of ${slides.length}, ${current.title}`} framed>
          {renderSlide(current, { prefix: `${prefix}-s-`, image: noImages, edit: { onText } })}
        </SlideStage>
        <p className="cf-deck-notes">
          {`Slide ${index + 1} of ${slides.length}. ${current.notes ?? ""}`}
        </p>
      </div>
    </div>
  );
}
