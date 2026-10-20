/**
 * Every starting point the shelf offers, kind by kind, each with the live
 * drawing that pictures it: the templates a person writes from, and for the
 * kinds agents build, the briefs whose examples the company already holds.
 */

import { useMemo, type ReactNode } from "react";
import { ArtifactKindGround } from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { GalleryPreview } from "~/routes/artifacts/GalleryPreview.tsx";
import type { ArtifactKind, ArtifactListItem } from "~/protocol/index.ts";
import { KIND, MAKE_ORDER, STARTERS, isWritten } from "../../shared/kinds.ts";
import { DOC_TEMPLATES, SHEET_TEMPLATE_DEFS } from "../../shared/templates.ts";
import { DeckPreview, DocTemplatePreview, SheetTemplatePreview } from "../../shared/previews.tsx";

/** A deck's starting points: the engine's starter under a title it opens with. */
export const DECK_STARTS = [
  {
    id: "starter",
    title: "Starter deck",
    detail: "A cover, an agenda, two columns and a close.",
    deckTitle: "",
  },
  {
    id: "quarterly",
    title: "Quarterly plan",
    detail: "Where we are, what we will do, what we need.",
    deckTitle: "Q1 plan",
  },
  {
    id: "update",
    title: "Weekly update",
    detail: "The week's progress for the team.",
    deckTitle: "Weekly update, week 43",
  },
] as const;

export interface Start {
  id: string;
  kind: ArtifactKind;
  title: string;
  detail: string;
  /** Where choosing it goes: New on its kind, from it. */
  query: Record<string, string>;
  /** The artifact an agent made from a brief like it, for the kinds agents build. */
  example?: string;
}

export function startsOf(kind: ArtifactKind): Start[] {
  switch (kind) {
    case "doc":
      return DOC_TEMPLATES.map((t) => ({
        id: t.id,
        kind,
        title: t.id === "blank" ? "Blank doc" : t.title,
        detail: t.detail,
        query: { kind, from: t.id },
      }));
    case "sheet":
      return SHEET_TEMPLATE_DEFS.map((t) => ({
        id: t.id,
        kind,
        title: t.id === "blank" ? "Blank sheet" : t.title,
        detail: t.detail,
        query: { kind, from: t.id },
      }));
    case "slides":
      return DECK_STARTS.map((t) => ({
        id: t.id,
        kind,
        title: t.title,
        detail: t.detail,
        query: { kind, from: t.id },
      }));
    default:
      return STARTERS[kind].map((s) => ({
        id: s.id,
        kind,
        title: s.label,
        detail: `${KIND[kind].tile}, built by an agent`,
        query: { kind, from: s.id },
        ...(s.example ? { example: s.example } : {}),
      }));
  }
}

/** The shelf with no kind chosen: one strong start per kind. */
export function featured(): Start[] {
  const pick: Record<ArtifactKind, string> = {
    doc: "meeting",
    slides: "starter",
    sheet: "review",
    design: "settle",
    dashboard: "beta",
    html: "calculator",
  };
  return MAKE_ORDER.map((k) => startsOf(k).find((s) => s.id === pick[k])!);
}

/** The company's artifacts by title, for drawing an example. */
export function useExamples(): Map<string, ArtifactListItem> {
  const listing = useQuery("artifacts", { scope: "all", sort: "updated", limit: 50 });
  return useMemo(
    () => new Map((listing.data?.items ?? []).map((i) => [i.title, i])),
    [listing.data],
  );
}

/** A start, drawn: its template live, or the example an agent made. */
export function StartPreview({
  start,
  examples,
}: {
  start: Start;
  examples: Map<string, ArtifactListItem>;
}): ReactNode {
  const people = useArtifactPeople();
  if (start.kind === "doc") {
    const t = DOC_TEMPLATES.find((x) => x.id === start.id)!;
    return <DocTemplatePreview template={t} />;
  }
  if (start.kind === "sheet") {
    const t = SHEET_TEMPLATE_DEFS.find((x) => x.id === start.id)!;
    return <SheetTemplatePreview template={t} />;
  }
  if (start.kind === "slides") {
    const t = DECK_STARTS.find((x) => x.id === start.id)!;
    return <DeckPreview title={t.deckTitle || "Untitled deck"} />;
  }
  const item = start.example ? examples.get(start.example) : undefined;
  return item && !isWritten(start.kind) ? (
    <GalleryPreview item={item} people={people} />
  ) : (
    <ArtifactKindGround kind={start.kind} />
  );
}
