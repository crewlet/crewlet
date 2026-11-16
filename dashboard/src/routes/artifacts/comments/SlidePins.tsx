/**
 * The comment layer over a deck's stage: a pin stands on the element its
 * thread's path names (`data-node`), measured when the slide, the threads or
 * the stage's size change, and a click anchors to the element under it.
 */

import { useCallback, useEffect, useLayoutEffect, useMemo, useState, type RefObject } from "react";
import { SLIDE_STAGE_HEIGHT, SLIDE_STAGE_WIDTH } from "~/contract/artifactsworkspace.ts";
import { nodeAt } from "~/lib/slides.tsx";
import {
  anchorLabel,
  elementText,
  partPin,
  pinned,
  placeOf,
  type Box,
} from "~/lib/artifacts/comments.ts";
import type { ArtifactCommentAnchor, DeckSlide } from "~/protocol/index.ts";
import { COMMENT_QUOTE_MAX_BYTES } from "~/runtime/bridge.ts";
import { clipBytes } from "~/runtime/comments.ts";
import { drawRegion, padRegion } from "~/runtime/crop.ts";
import { DraftPin, Outline, Repositioned, ThreadPin } from "./layer.tsx";
import { useComments } from "./state.tsx";

const SLIDE = { w: SLIDE_STAGE_WIDTH, h: SLIDE_STAGE_HEIGHT };

/** The slide anchor a thread or a draft has on this slide, or null. */
function onSlide(a: ArtifactCommentAnchor | null | undefined, slide: string) {
  return a && a.kind === "slides" && a.slide === slide ? a : null;
}

/** An element's box in the layer's own pixels. */
function boxIn(el: Element, origin: DOMRect): Box {
  const r = el.getBoundingClientRect();
  return { x: r.left - origin.left, y: r.top - origin.top, w: r.width, h: r.height };
}

export function SlidePins({
  stage,
  slide,
  order,
  onGo,
}: {
  /** The box the stage is drawn in, which the layer covers. */
  stage: RefObject<HTMLDivElement | null>;
  slide: DeckSlide;
  /** The deck's slides in order, which number a slide in words. */
  order: readonly string[];
  /** Puts another slide on the stage. */
  onGo: (id: string) => void;
}) {
  const c = useComments();
  const [boxes, setBoxes] = useState<ReadonlyMap<string, Box>>(new Map());
  const [hover, setHover] = useState<{ path: string; box: Box } | null>(null);

  const pins = useMemo(
    () =>
      c
        ? c.threads.filter((t) => pinned(t, c.filter, c.selected) && onSlide(placeOf(t), slide.id))
        : [],
    [c, slide.id],
  );
  const draft = c?.draft && onSlide(c.draft.anchor, slide.id) ? c.draft : null;
  const selected = c?.threads.find((t) => t.id === c.selected);
  const focus = onSlide(selected ? placeOf(selected) : null, slide.id);
  // THE PATHS TO MEASURE, as one key, so a new answer naming the same ones measures nothing.
  const wanted = useMemo(() => {
    const paths = new Set(pins.map((t) => placeOf(t)!.path ?? ""));
    if (draft) paths.add(draft.anchor.path ?? "");
    if (focus) paths.add(focus.path ?? "");
    return [...paths].sort().join("|");
  }, [pins, draft, focus]);

  const canvasOf = useCallback(
    () => stage.current?.querySelector<HTMLElement>(".slide-canvas") ?? null,
    [stage],
  );
  const elementOf = useCallback(
    (path: string) =>
      path === "" ? canvasOf() : (canvasOf()?.querySelector(`[data-node="${path}"]`) ?? null),
    [canvasOf],
  );
  const measure = useCallback(() => {
    const box = stage.current;
    if (!box) return;
    const origin = box.getBoundingClientRect();
    const next = new Map<string, Box>();
    for (const path of wanted ? wanted.split("|") : []) {
      const el = elementOf(path);
      if (el) next.set(path, boxIn(el, origin));
    }
    setBoxes(next);
  }, [stage, wanted, elementOf]);
  useLayoutEffect(measure, [measure, slide]);
  useEffect(() => {
    const box = stage.current;
    if (!box || typeof ResizeObserver !== "function") return undefined;
    const watch = new ResizeObserver(() => measure());
    watch.observe(box);
    return () => watch.disconnect();
  }, [stage, measure]);
  useEffect(() => {
    if (!c?.placing) setHover(null);
  }, [c?.placing]);

  // JUMP TO: the slide the thread is on comes onto the stage.
  const setRevealer = c?.setRevealer;
  useEffect(() => {
    if (!setRevealer) return undefined;
    setRevealer((t) => {
      const a = placeOf(t);
      if (a?.kind === "slides" && a.slide && a.slide !== slide.id) onGo(a.slide);
    });
    return () => setRevealer(null);
  }, [setRevealer, slide.id, onGo]);

  if (!c) return null;

  /** The innermost element drawn under a point of the window, with its path. */
  const elementAt = (x: number, y: number): { el: Element; path: string } | null => {
    const canvas = canvasOf();
    if (!canvas || typeof document.elementsFromPoint !== "function") return null;
    for (const el of document.elementsFromPoint(x, y)) {
      const path = el.getAttribute("data-node");
      if (path !== null && canvas.contains(el)) return { el, path };
    }
    return null;
  };

  const place = async (x: number, y: number) => {
    const hit = elementAt(x, y);
    const canvas = canvasOf();
    const node = hit ? nodeAt(slide.root, hit.path) : null;
    if (!hit || !node?.tag || !canvas) return;
    const anchor: ArtifactCommentAnchor = {
      kind: "slides",
      slide: slide.id,
      path: hit.path,
      tag: node.tag,
      text: elementText(node),
    };
    const words = clipBytes(elementText(node, Number.MAX_SAFE_INTEGER), COMMENT_QUOTE_MAX_BYTES);
    const seq = c.startDraft({
      anchor,
      label: anchorLabel(anchor, order),
      quote: words || undefined,
      crop: undefined,
    });
    if (c.narrow) c.showPanel();
    // THE PICTURE IS OF THE SLIDE AS DRAWN, at its own size: the element's
    // box on screen, taken back through the stage's scale.
    const at = canvas.getBoundingClientRect();
    const r = hit.el.getBoundingClientRect();
    const scale = at.width / SLIDE.w || 1;
    const region = padRegion(
      {
        x: (r.left - at.left) / scale,
        y: (r.top - at.top) / scale,
        w: r.width / scale,
        h: r.height / scale,
      },
      SLIDE,
    );
    c.setDraftCrop(seq, await drawRegion(canvas, SLIDE, region));
  };

  const draftBox = draft ? boxes.get(draft.anchor.path ?? "") : undefined;
  const draftAt = draftBox ? partPin(draftBox) : null;
  const focusBox = focus ? boxes.get(focus.path ?? "") : undefined;
  return (
    <div className="artifact-comment-layer" data-placing={c.placing || undefined}>
      {c.placing && (
        <div
          className="artifact-comment-catch"
          aria-hidden="true"
          onPointerMove={(event) => {
            const hit = elementAt(event.clientX, event.clientY);
            const box = stage.current;
            if (!hit || !box) return setHover(null);
            if (hover?.path === hit.path) return;
            setHover({ path: hit.path, box: boxIn(hit.el, box.getBoundingClientRect()) });
          }}
          onPointerLeave={() => setHover(null)}
          onClick={(event) => void place(event.clientX, event.clientY)}
        />
      )}
      {focusBox && <Outline box={focusBox} />}
      {hover && (
        <Outline
          box={hover.box}
          label={anchorLabel(
            {
              kind: "slides",
              slide: slide.id,
              path: hover.path,
              text: elementText(nodeAt(slide.root, hover.path)),
            },
            order,
          )}
        />
      )}
      {pins.map((t) => {
        const box = boxes.get(placeOf(t)!.path ?? "");
        if (!box) return null;
        const at = partPin(box);
        return <ThreadPin key={t.id} t={t} x={at.x} y={at.y} />;
      })}
      {draftAt && <DraftPin x={draftAt.x} y={draftAt.y} />}
      <Repositioned stamp={`${draftAt?.x},${draftAt?.y}`} />
    </div>
  );
}
