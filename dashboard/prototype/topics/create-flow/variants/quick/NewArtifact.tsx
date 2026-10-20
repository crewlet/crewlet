/**
 * Quick's New: the editor and nothing else. A slim line names the kind and
 * the draft's state and holds Create (also Command and Enter); under it the
 * kind's editor at the page's full width, the caret already in it. For what
 * agents build, one centred ask with its builder in the same line.
 */

import { useEffect, type ReactNode } from "react";
import { ArtifactKindMark, Button, Input, Kbd, Select } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { KIND, isWritten, type WrittenKind } from "../../shared/kinds.ts";
import { DOC_TEMPLATES, SHEET_TEMPLATE_DEFS } from "../../shared/templates.ts";
import { useDrafts } from "../../shared/drafts.ts";
import { DocCanvas } from "../../shared/DocCanvas.tsx";
import { SheetCanvas } from "../../shared/SheetCanvas.tsx";
import { SlidesCanvas } from "../../shared/SlidesCanvas.tsx";
import { BriefCanvas } from "../../shared/BriefCanvas.tsx";

export function NewArtifact() {
  const [kindParam] = useParam("kind", "doc");
  const kind: ArtifactKind = isArtifactKind(kindParam) ? kindParam : "doc";
  return isWritten(kind) ? <Focus key={kind} kind={kind} /> : <Ask key={kind} kind={kind} />;
}

function Line({ kind, children }: { kind: ArtifactKind; children?: ReactNode }) {
  return (
    <div className="cfq-line">
      <ArtifactKindMark kind={kind} size="sm" />
      <span className="cfq-line-kind">{KIND[kind].heading}</span>
      <span className="cfq-line-state">Draft, not saved yet</span>
      {children}
    </div>
  );
}

function Focus({ kind }: { kind: WrittenKind }) {
  const nav = useNavigator();
  const drafts = useDrafts();
  const { commit } = drafts;
  // COMMAND AND ENTER MAKES IT, from anywhere on the page: the editor is
  // the whole screen, so the key belongs to the screen.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        void commit(kind);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [commit, kind]);
  const starts =
    kind === "doc"
      ? DOC_TEMPLATES.map((t) => ({ value: t.id, label: t.title }))
      : kind === "sheet"
        ? SHEET_TEMPLATE_DEFS.map((t) => ({ value: t.id, label: t.title }))
        : null;
  return (
    <div className={`cfq-focus cfq-focus--${kind}`}>
      <PageActions>
        <span className="cfq-keys">
          <Kbd keys={["Mod", "Enter"]} subtle />
        </span>
        <Button size="small" variant="ghost" onClick={() => nav.back()}>
          Discard
        </Button>
        <Button
          size="small"
          variant="primary"
          loading={drafts.busy}
          onClick={() => void drafts.commit(kind)}
        >
          {KIND[kind].action}
        </Button>
      </PageActions>
      <Line kind={kind}>
        {starts && (
          <Select
            size="sm"
            width="auto"
            appearance="quiet"
            ariaLabel="Start from"
            value={kind === "doc" ? drafts.docTemplate : drafts.sheet.template}
            onChange={(v) =>
              kind === "doc" ? drafts.chooseDoc(String(v)) : drafts.chooseSheet(String(v))
            }
            options={starts}
          />
        )}
      </Line>
      {drafts.refusal && <p className="artifact-refusal">{drafts.refusal}</p>}
      {kind === "doc" && (
        <DocCanvas
          key={drafts.docTemplate}
          draft={drafts.doc}
          onChange={drafts.setDoc}
          className="cfq-doc"
        />
      )}
      {kind === "sheet" && (
        <div className="cfq-sheet">
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
      {kind === "slides" && (
        <SlidesCanvas draft={drafts.deck} onChange={drafts.setDeck} showTitle={false} />
      )}
    </div>
  );
}

function Ask({ kind }: { kind: "design" | "dashboard" | "html" }) {
  const nav = useNavigator();
  return (
    <div className="cfq-focus cfq-focus--ask">
      <PageActions>
        <Button size="small" variant="ghost" onClick={() => nav.back()}>
          Discard
        </Button>
      </PageActions>
      <div className="cfq-ask">
        <ArtifactKindMark kind={kind} size="xl" surface="bare" />
        <h1 className="cfq-ask-title">{`What should ${KIND[kind].one} show?`}</h1>
        <BriefCanvas kind={kind} layout="inline" />
      </div>
    </div>
  );
}
