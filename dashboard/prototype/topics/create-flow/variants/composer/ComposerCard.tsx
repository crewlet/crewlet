/**
 * The composer: one card asking what to make. Its kinds are a row of chips,
 * and the chosen kind's create experience opens inside the card: the doc's
 * page, the sheet's grid, the deck's cover or a brief to an agent. The card
 * is the whole flow, so a person never leaves the work they came from until
 * what they make is made.
 */

import { ArtifactKindTile, Button, Input, SegmentedControl } from "@crewlethq/ui";
import type { ArtifactKind } from "~/protocol/index.ts";
import { KIND, MAKE_ORDER, isWritten, type WrittenKind } from "../../shared/kinds.ts";
import { SHEET_TEMPLATE_DEFS, DOC_TEMPLATES } from "../../shared/templates.ts";
import { useDrafts, type Drafts } from "../../shared/drafts.ts";
import { DocCanvas } from "../../shared/DocCanvas.tsx";
import { SheetCanvas } from "../../shared/SheetCanvas.tsx";
import { SlidesCanvas } from "../../shared/SlidesCanvas.tsx";
import { BriefCanvas } from "../../shared/BriefCanvas.tsx";

export function ComposerCard({
  kind,
  onKind,
  standalone = false,
}: {
  /** The kind open in the card, or "" for none. */
  kind: ArtifactKind | "";
  onKind: (kind: ArtifactKind | "") => void;
  /** On its own page (New), where the card is the page and cannot close. */
  standalone?: boolean;
}) {
  const drafts = useDrafts();
  return (
    <section
      className={`cfc-card${kind ? " cfc-card--open" : ""}${standalone ? " cfc-card--page" : ""}`}
      aria-labelledby="cfc-title"
    >
      <div className="cfc-head">
        <h2 className="cfc-title" id="cfc-title">
          {kind ? KIND[kind].heading : "What do you want to make?"}
        </h2>
        <div className="cfc-kinds" role="group" aria-label="Kind">
          {MAKE_ORDER.map((k) => (
            <ArtifactKindTile
              key={k}
              appearance="chip"
              kind={k}
              label={KIND[k].tile}
              selected={k === kind}
              onSelect={() => onKind(k === kind && !standalone ? "" : k)}
            />
          ))}
        </div>
      </div>
      {kind === "" ? (
        <p className="cfc-hint">
          Docs, decks and sheets open right here for you to write. Designs, dashboards and pages are
          built by an agent from a short brief.
        </p>
      ) : isWritten(kind) ? (
        <Written kind={kind} drafts={drafts} onClose={standalone ? undefined : () => onKind("")} />
      ) : (
        <div className="cfc-body">
          <BriefCanvas
            key={kind}
            kind={kind}
            layout="inline"
            {...(standalone ? {} : { onCancel: () => onKind("") })}
          />
        </div>
      )}
    </section>
  );
}

function Written({
  kind,
  drafts,
  onClose,
}: {
  kind: WrittenKind;
  drafts: Drafts;
  onClose: (() => void) | undefined;
}) {
  const starts =
    kind === "doc"
      ? DOC_TEMPLATES.map((t) => ({ value: t.id, label: t.title }))
      : kind === "sheet"
        ? SHEET_TEMPLATE_DEFS.map((t) => ({ value: t.id, label: t.title }))
        : null;
  const chosen = kind === "doc" ? drafts.docTemplate : drafts.sheet.template;
  return (
    <div className={`cfc-body cfc-body--${kind}`}>
      {starts && (
        <div className="cfc-starts">
          <span className="cfc-starts-label">Start from</span>
          <SegmentedControl
            label="Start from"
            semantics="radio"
            size="sm"
            options={starts}
            value={chosen}
            onValueChange={(v: string) =>
              kind === "doc" ? drafts.chooseDoc(v) : drafts.chooseSheet(v)
            }
          />
        </div>
      )}
      <div className="cfc-editor">
        {kind === "doc" && (
          <DocCanvas
            key={drafts.docTemplate}
            draft={drafts.doc}
            onChange={drafts.setDoc}
            size="sm"
          />
        )}
        {kind === "sheet" && (
          <>
            <Input
              appearance="title"
              width="full"
              aria-label="Title"
              placeholder="Untitled sheet"
              autoFocus
              className="cfc-sheet-title"
              value={drafts.sheet.title}
              onChange={(e) => drafts.setSheet({ ...drafts.sheet, title: e.target.value })}
            />
            <SheetCanvas
              key={drafts.sheet.template}
              draft={drafts.sheet}
              onChange={drafts.setSheet}
            />
          </>
        )}
        {kind === "slides" && (
          <SlidesCanvas
            draft={drafts.deck}
            onChange={drafts.setDeck}
            showTitle={false}
            orientation="horizontal"
          />
        )}
      </div>
      {drafts.refusal && <p className="artifact-refusal">{drafts.refusal}</p>}
      <div className="cfc-foot">
        <span className="cfc-foot-note">
          {`Private to you until you share it. ${drafts.titleFor(kind)} opens on its own page once it is made.`}
        </span>
        {onClose && (
          <Button size="small" variant="ghost" onClick={onClose}>
            Close
          </Button>
        )}
        <Button
          size="small"
          variant="primary"
          loading={drafts.busy}
          onClick={() => void drafts.commit(kind)}
        >
          {KIND[kind].action}
        </Button>
      </div>
    </div>
  );
}
