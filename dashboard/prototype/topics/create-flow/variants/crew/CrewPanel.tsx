/**
 * The panel a kind's tile opens: a side sheet beside the gallery,
 * holding that kind's create experience whole. Its head is the kind's mark
 * and name; its foot the create, for what a person writes, while a brief to
 * an agent carries its own send.
 */

import { ArtifactKindMark, Button, Input, Modal, SegmentedControl } from "@crewlethq/ui";
import type { ArtifactKind } from "~/protocol/index.ts";
import { KIND, isWritten } from "../../shared/kinds.ts";
import { DOC_TEMPLATES, SHEET_TEMPLATE_DEFS } from "../../shared/templates.ts";
import { useDrafts } from "../../shared/drafts.ts";
import { DocCanvas } from "../../shared/DocCanvas.tsx";
import { SheetCanvas } from "../../shared/SheetCanvas.tsx";
import { SlidesCanvas } from "../../shared/SlidesCanvas.tsx";
import { BriefCanvas } from "../../shared/BriefCanvas.tsx";

export function CrewPanel({ kind, onClose }: { kind: ArtifactKind | ""; onClose: () => void }) {
  const drafts = useDrafts();
  const open = kind !== "";
  const k: ArtifactKind = kind || "doc";
  const written = isWritten(k);
  return (
    <Modal
      open={open}
      onClose={onClose}
      variant="sheet"
      size="lg"
      className="cfr-panel"
      title={KIND[k].heading}
      subtitle={KIND[k].about}
      icon={<ArtifactKindMark kind={k} size="sm" />}
      {...(written
        ? {
            footerStart: drafts.refusal ? (
              <span className="artifact-refusal">{drafts.refusal}</span>
            ) : (
              <span className="cfr-foot-note">Private to you until you share it.</span>
            ),
            footer: (
              <>
                <Button size="small" variant="ghost" onClick={onClose}>
                  Cancel
                </Button>
                <Button
                  size="small"
                  variant="primary"
                  loading={drafts.busy}
                  onClick={() => void drafts.commit(k)}
                >
                  {KIND[k].action}
                </Button>
              </>
            ),
          }
        : {})}
    >
      {open && (
        <div className={`cfr-body cfr-body--${k}`}>
          {k === "doc" && (
            <>
              <SegmentedControl
                label="Start from"
                semantics="radio"
                size="sm"
                options={DOC_TEMPLATES.map((t) => ({ value: t.id, label: t.title }))}
                value={drafts.docTemplate}
                onValueChange={(v: string) => drafts.chooseDoc(v)}
              />
              <div className="cfr-paper">
                <DocCanvas
                  key={drafts.docTemplate}
                  draft={drafts.doc}
                  onChange={drafts.setDoc}
                  size="sm"
                />
              </div>
            </>
          )}
          {k === "sheet" && (
            <>
              <SegmentedControl
                label="Start from"
                semantics="radio"
                size="sm"
                options={SHEET_TEMPLATE_DEFS.map((t) => ({ value: t.id, label: t.title }))}
                value={drafts.sheet.template}
                onValueChange={(v: string) => drafts.chooseSheet(v)}
              />
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
            </>
          )}
          {k === "slides" && (
            <SlidesCanvas
              draft={drafts.deck}
              onChange={drafts.setDeck}
              showTitle={false}
              orientation="horizontal"
            />
          )}
          {!written && <BriefCanvas key={k} kind={k} layout="stack" onCancel={onClose} />}
        </div>
      )}
    </Modal>
  );
}
