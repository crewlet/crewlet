/**
 * A written kind's draft and its create, in one hook every proposal frames
 * its own way: the doc's page, the sheet's grid and the deck's slides as the
 * person left them, and the one press that makes the artifact from them.
 *
 * NEVER BLOCKED ON A TITLE: a draft with none is made as "Untitled doc" (or
 * deck, or sheet), as every document editor names a new file, and renamed
 * from its page like any artifact.
 */

import { useState } from "react";
import { DOC_TEMPLATES } from "./templates.ts";
import type { DocDraftState } from "./DocCanvas.tsx";
import { sheetCommitOf, sheetDraftOf, type SheetDraftState } from "./SheetCanvas.tsx";
import { deckDraftOf, type DeckDraftState } from "./SlidesCanvas.tsx";
import { commitDeck, commitDoc, commitSheet, useCreate } from "./commit.ts";
import type { WrittenKind } from "./kinds.ts";

const UNTITLED: Readonly<Record<WrittenKind, string>> = {
  doc: "Untitled doc",
  slides: "Untitled deck",
  sheet: "Untitled sheet",
};

/** A doc's draft from one of its starting points. */
export function docDraftFrom(templateId: string, title = ""): DocDraftState {
  const t = DOC_TEMPLATES.find((x) => x.id === templateId) ?? DOC_TEMPLATES[0]!;
  return { title: title || t.suggested, body: t.markdown };
}

/** Where a draft starts: each kind's starting point, and a deck's title. */
export interface DraftStart {
  doc?: string;
  sheet?: string;
  deckTitle?: string;
}

export function useDrafts(start: DraftStart = {}) {
  const [doc, setDoc] = useState<DocDraftState>(() => docDraftFrom(start.doc ?? "blank"));
  const [docTemplate, setDocTemplate] = useState(start.doc ?? "blank");
  const [sheet, setSheet] = useState<SheetDraftState>(() => sheetDraftOf(start.sheet ?? "blank"));
  const [deck, setDeck] = useState<DeckDraftState>(() => deckDraftOf(start.deckTitle ?? ""));
  const create = useCreate();

  /** Swap a doc's starting point, keeping a title the person typed. */
  const chooseDoc = (templateId: string) => {
    const typed = doc.title !== (DOC_TEMPLATES.find((t) => t.id === docTemplate)?.suggested ?? "");
    setDocTemplate(templateId);
    setDoc(docDraftFrom(templateId, typed ? doc.title : ""));
  };
  /** Swap a sheet's starting point, keeping a title the person typed. */
  const chooseSheet = (templateId: string) => {
    const was = sheetDraftOf(sheet.template);
    const typed = sheet.title !== was.title;
    setSheet(sheetDraftOf(templateId, typed ? sheet.title : ""));
  };

  const titleFor = (kind: WrittenKind): string => {
    const typed = kind === "doc" ? doc.title : kind === "sheet" ? sheet.title : deck.title;
    return typed.trim() || UNTITLED[kind];
  };

  const commit = (kind: WrittenKind) => {
    const title = titleFor(kind);
    if (kind === "doc") return create.run(kind, title, () => commitDoc(title, doc.body));
    if (kind === "slides") return create.run(kind, title, () => commitDeck(title, deck.edits));
    const { schema, rows } = sheetCommitOf(sheet);
    return create.run(kind, title, () => commitSheet(title, schema, rows));
  };

  return {
    doc,
    setDoc,
    docTemplate,
    chooseDoc,
    sheet,
    setSheet,
    chooseSheet,
    deck,
    setDeck,
    titleFor,
    commit,
    busy: create.busy,
    refusal: create.refusal,
  };
}

export type Drafts = ReturnType<typeof useDrafts>;
