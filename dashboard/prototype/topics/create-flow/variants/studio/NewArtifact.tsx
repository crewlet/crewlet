/**
 * Studio: a full creation studio per kind, reached straight from its tile.
 *
 * The studio's head names the kind by its glyph and colour and lets a person
 * switch kinds without losing a draft. Under it, the kind's own editor on the
 * org chart's dotted canvas: a doc's page with its title focused, a sheet's
 * live grid, the deck's cover on its stage, and for what agents build, a
 * brief to the one best suited. Starting points sit in a rail beside the
 * editor and fill it when chosen, so a template is a shortcut and never a
 * step in the way. Create is in the page bar, where every page keeps its
 * primary action.
 */

import {
  ArtifactKindMark,
  ArtifactKindTile,
  ArtifactTemplateCard,
  Button,
  DottedGround,
  Input,
} from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { KIND, MAKE_ORDER, isWritten, type WrittenKind } from "../../shared/kinds.ts";
import { DOC_TEMPLATES, SHEET_TEMPLATE_DEFS } from "../../shared/templates.ts";
import { useDrafts, type Drafts } from "../../shared/drafts.ts";
import { DocCanvas } from "../../shared/DocCanvas.tsx";
import { SheetCanvas } from "../../shared/SheetCanvas.tsx";
import { SlidesCanvas } from "../../shared/SlidesCanvas.tsx";
import { BriefCanvas } from "../../shared/BriefCanvas.tsx";
import { DocTemplatePreview, SheetTemplatePreview } from "../../shared/previews.tsx";

export function NewArtifact() {
  const nav = useNavigator();
  const [kindParam, setKind] = useParam("kind", "doc");
  const kind: ArtifactKind = isArtifactKind(kindParam) ? kindParam : "doc";
  const drafts = useDrafts();
  const written = isWritten(kind);
  return (
    <div className={`cfs-studio cfs-studio--${kind}`}>
      <PageActions>
        <Button size="small" variant="ghost" onClick={() => nav.back()}>
          Cancel
        </Button>
        {written && (
          <Button
            size="small"
            variant="primary"
            loading={drafts.busy}
            onClick={() => void drafts.commit(kind)}
          >
            {KIND[kind].action}
          </Button>
        )}
      </PageActions>
      <header className="cfs-head">
        <ArtifactKindMark kind={kind} size="lg" />
        <div className="cfs-head-words">
          <h1 className="cfs-title">{KIND[kind].heading}</h1>
          <p className="cfs-about">{KIND[kind].about}</p>
        </div>
        <nav className="cfs-switch" aria-label="Make something else">
          {MAKE_ORDER.map((k) => (
            <ArtifactKindTile
              key={k}
              appearance="chip"
              kind={k}
              label={KIND[k].tile}
              selected={k === kind}
              onSelect={() => setKind(k)}
            />
          ))}
        </nav>
      </header>
      {drafts.refusal && <p className="artifact-refusal">{drafts.refusal}</p>}
      {written ? (
        <WrittenStudio kind={kind} drafts={drafts} />
      ) : (
        <DottedGround kind={kind} className="cfs-canvas cfs-canvas--brief">
          <div className="cfs-paper cfs-paper--brief">
            <BriefCanvas key={kind} kind={kind} layout="page" />
          </div>
        </DottedGround>
      )}
    </div>
  );
}

function WrittenStudio({ kind, drafts }: { kind: WrittenKind; drafts: Drafts }) {
  if (kind === "slides") {
    return (
      <DottedGround kind="slides" className="cfs-canvas cfs-canvas--deck">
        <div className="cfs-paper cfs-paper--deck">
          <SlidesCanvas draft={drafts.deck} onChange={drafts.setDeck} showTitle={false} />
        </div>
      </DottedGround>
    );
  }
  const rail =
    kind === "doc"
      ? DOC_TEMPLATES.map((t) => (
          <li key={t.id}>
            <ArtifactTemplateCard
              layout="row"
              kind="doc"
              title={t.title}
              detail={t.detail}
              selected={drafts.docTemplate === t.id}
              onSelect={() => drafts.chooseDoc(t.id)}
              preview={<DocTemplatePreview template={t} />}
            />
          </li>
        ))
      : SHEET_TEMPLATE_DEFS.map((t) => (
          <li key={t.id}>
            <ArtifactTemplateCard
              layout="row"
              kind="sheet"
              title={t.title}
              detail={t.detail}
              selected={drafts.sheet.template === t.id}
              onSelect={() => drafts.chooseSheet(t.id)}
              preview={<SheetTemplatePreview template={t} />}
            />
          </li>
        ));
  return (
    <div className="cfs-body">
      <aside className="cfs-rail" aria-labelledby="cfs-start">
        <h2 className="cf-brief-heading" id="cfs-start">
          Start from
        </h2>
        <ul className="cfs-rail-list" role="list">
          {rail}
        </ul>
      </aside>
      <DottedGround kind={kind} className="cfs-canvas">
        {kind === "doc" ? (
          <div className="cfs-paper">
            <DocCanvas key={drafts.docTemplate} draft={drafts.doc} onChange={drafts.setDoc} />
          </div>
        ) : (
          <div className="cfs-paper cfs-paper--wide">
            <Input
              appearance="title"
              width="full"
              aria-label="Title"
              placeholder="Untitled sheet"
              autoFocus
              value={drafts.sheet.title}
              onChange={(e) => drafts.setSheet({ ...drafts.sheet, title: e.target.value })}
            />
            <SheetCanvas
              key={drafts.sheet.template}
              draft={drafts.sheet}
              onChange={drafts.setSheet}
            />
          </div>
        )}
      </DottedGround>
    </div>
  );
}
