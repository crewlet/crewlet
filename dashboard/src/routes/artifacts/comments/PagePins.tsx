/** Comment pins over a page's frame, from what its cross-origin runtime reports while asked. */

import { useEffect, useMemo, useRef, useState } from "react";
import { LayerHost } from "@crewlethq/ui";
import type { CommentLayout, CommentMode } from "~/runtime/bridge.ts";
import {
  anchorBox,
  anchorLabel,
  pagePin,
  phrase,
  pinned,
  placeOf,
} from "~/lib/artifacts/comments.ts";
import type { ArtifactCommentAnchor } from "~/protocol/index.ts";
import type { PageHost } from "../host.ts";
import { DraftPin, Outline, Repositioned, ThreadPin, usePlacingCursor } from "./layer.tsx";
import { useComments } from "./state.tsx";

/** The page's anchor a thread is pinned to on this page, or null. */
function onPage(a: ArtifactCommentAnchor | null | undefined, page: string) {
  return a && a.kind === "html" && a.page === page ? a : null;
}

export function PagePins({ host, page }: { host: PageHost; page: string }) {
  const c = useComments();
  const [layout, setLayout] = useState<CommentLayout | null>(host.commentLayout);
  // THE PICTURE ASKED FOR, and the draft it is for: an answer to an older
  // pick is not the newer draft's.
  const capture = useRef<{ id: number; seq: number } | null>(null);

  const pins = useMemo(
    () =>
      c ? c.threads.filter((t) => pinned(t, c.filter, c.selected) && onPage(placeOf(t), page)) : [],
    [c, page],
  );
  const draft = c?.draft && onPage(c.draft.anchor, page) ? c.draft : null;
  const mode: CommentMode = !c
    ? "off"
    : c.placing
      ? "place"
      : pins.length > 0 || draft
        ? "show"
        : "off";
  const cursor = usePlacingCursor(mode === "place");
  useEffect(() => host.setCommentMode(mode, cursor), [host, mode, cursor]);
  useEffect(() => () => host.setCommentMode("off"), [host]);

  useEffect(() => {
    if (!c) return undefined;
    return host.onComments((news) => {
      switch (news.kind) {
        case "layout":
          setLayout(news.layout);
          return;
        case "escape":
          c.setPlacing(false);
          return;
        case "crop":
          if (capture.current?.id === news.crop.id) {
            c.setDraftCrop(capture.current.seq, news.crop.image);
            capture.current = null;
          }
          return;
        case "pick": {
          const { name, quote, point } = news.pick;
          const anchor: ArtifactCommentAnchor = name
            ? { kind: "html", page, name }
            : { kind: "html", page, point };
          const seq = c.startDraft({
            anchor,
            label: anchorLabel(anchor),
            quote: quote || undefined,
            crop: undefined,
          });
          capture.current = { id: host.captureComment(name ? { name } : { point }), seq };
          // ON A PHONE THE DRAFT IS WRITTEN IN THE PANEL, which covers the page;
          // elsewhere it stands beside its pin, and a panel drawn over the page makes way.
          if (c.narrow) c.showPanel();
          else c.stepAside();
        }
      }
    });
  }, [host, c, page]);

  // JUMP TO: the page scrolls its own document to the part.
  const setRevealer = c?.setRevealer;
  useEffect(() => {
    if (!setRevealer) return undefined;
    setRevealer((t) => {
      const a = onPage(placeOf(t), page);
      if (a?.name) host.revealComment({ name: a.name });
      else if (a?.point) host.revealComment({ point: a.point });
    });
    return () => setRevealer(null);
  }, [setRevealer, host, page]);

  if (!c || !layout) return null;
  const selected = c.threads.find((t) => t.id === c.selected);
  const focus = onPage(selected ? placeOf(selected) : null, page);
  const focusBox = focus?.name ? anchorBox(layout, focus.name) : null;
  const hoverBox = c.placing && layout.hover ? anchorBox(layout, layout.hover) : null;
  const draftAt = draft ? pagePin(layout, draft.anchor) : null;
  return (
    <div className="artifact-comment-layer" data-placing={c.placing || undefined}>
      <LayerHost>
        {focusBox && <Outline box={focusBox} />}
        {hoverBox && layout.hover && <Outline box={hoverBox} label={phrase(layout.hover)} />}
        {pins.map((t) => {
          const at = pagePin(layout, placeOf(t)!);
          return at ? <ThreadPin key={t.id} t={t} x={at.x} y={at.y} /> : null;
        })}
        {draftAt && <DraftPin x={draftAt.x} y={draftAt.y} />}
        <Repositioned stamp={`${layout.scroll.x},${layout.scroll.y},${draftAt?.x},${draftAt?.y}`} />
      </LayerHost>
    </div>
  );
}
