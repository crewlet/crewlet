/**
 * Templates' New: the editor, already filled from the starting point chosen
 * on the shelf, with every other starting point of its kind in a row above
 * it to swap. For the kinds agents build, the brief is written from the
 * chosen start, with what an agent here already made from one like it beside
 * it.
 */

import {
  ArtifactKindMark,
  ArtifactTemplateCard,
  Button,
  Input,
  LivePreviewBudgetProvider,
} from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import type { ArtifactKind } from "~/protocol/index.ts";
import { KIND, STARTERS, isWritten, type BuiltKind } from "../../shared/kinds.ts";
import { useDrafts } from "../../shared/drafts.ts";
import { DocCanvas } from "../../shared/DocCanvas.tsx";
import { SheetCanvas } from "../../shared/SheetCanvas.tsx";
import { SlidesCanvas, deckDraftOf } from "../../shared/SlidesCanvas.tsx";
import { BriefCanvas } from "../../shared/BriefCanvas.tsx";
import { DECK_STARTS, StartPreview, startsOf, useExamples } from "./starts.tsx";

export function NewArtifact() {
  const [kindParam] = useParam("kind", "doc");
  const [from] = useParam("from", "");
  const kind: ArtifactKind = isArtifactKind(kindParam) ? kindParam : "doc";
  return (
    <LivePreviewBudgetProvider>
      {isWritten(kind) ? (
        <WrittenFrom key={kind} kind={kind} from={from} />
      ) : (
        <BuiltFrom key={`${kind}-${from}`} kind={kind} from={from} />
      )}
    </LivePreviewBudgetProvider>
  );
}

function Head({ kind }: { kind: ArtifactKind }) {
  return (
    <header className="cft-page-head">
      <ArtifactKindMark kind={kind} size="md" />
      <h1 className="cft-page-title">{KIND[kind].heading}</h1>
    </header>
  );
}

function WrittenFrom({ kind, from }: { kind: "doc" | "slides" | "sheet"; from: string }) {
  const nav = useNavigator();
  const examples = useExamples();
  const deck = DECK_STARTS.find((d) => d.id === from) ?? DECK_STARTS[0];
  const drafts = useDrafts({
    ...(kind === "doc" && from ? { doc: from } : {}),
    ...(kind === "sheet" && from ? { sheet: from } : {}),
    ...(kind === "slides" ? { deckTitle: deck.deckTitle } : {}),
  });
  const chosen =
    kind === "doc" ? drafts.docTemplate : kind === "sheet" ? drafts.sheet.template : deck.id;
  const choose = (id: string) => {
    if (kind === "doc") drafts.chooseDoc(id);
    else if (kind === "sheet") drafts.chooseSheet(id);
    else {
      const d = DECK_STARTS.find((x) => x.id === id) ?? DECK_STARTS[0];
      drafts.setDeck(deckDraftOf(d.deckTitle));
      nav.replace(["artifacts", "new"], { kind, from: id });
    }
  };
  return (
    <div className="cft-page">
      <PageActions>
        <Button size="small" variant="ghost" onClick={() => nav.back()}>
          Cancel
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
      <Head kind={kind} />
      <ul className="cft-strip" role="list" aria-label="Starting points">
        {startsOf(kind).map((s) => (
          <li key={s.id}>
            <ArtifactTemplateCard
              kind={kind}
              title={s.title}
              detail={s.detail}
              selected={s.id === chosen}
              onSelect={() => choose(s.id)}
              preview={<StartPreview start={s} examples={examples} />}
            />
          </li>
        ))}
      </ul>
      {drafts.refusal && <p className="artifact-refusal">{drafts.refusal}</p>}
      <div className={`cft-editor cft-editor--${kind}`}>
        {kind === "doc" && (
          <DocCanvas key={drafts.docTemplate} draft={drafts.doc} onChange={drafts.setDoc} />
        )}
        {kind === "sheet" && (
          <>
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
        {kind === "slides" && (
          <SlidesCanvas
            key={deck.id}
            draft={drafts.deck}
            onChange={drafts.setDeck}
            showTitle={false}
          />
        )}
      </div>
    </div>
  );
}

function BuiltFrom({ kind, from }: { kind: BuiltKind; from: string }) {
  const nav = useNavigator();
  const examples = useExamples();
  const starter = STARTERS[kind].find((s) => s.id === from);
  const start = startsOf(kind).find((s) => s.id === from);
  const example = start?.example ? examples.get(start.example) : undefined;
  return (
    <div className="cft-page">
      <PageActions>
        <Button size="small" variant="ghost" onClick={() => nav.back()}>
          Cancel
        </Button>
      </PageActions>
      <Head kind={kind} />
      <BriefCanvas
        kind={kind}
        layout="page"
        {...(starter ? { starter: starter.id } : {})}
        aside={
          start && example ? (
            <section className="cf-brief-block" aria-labelledby="cft-example">
              <h2 className="cf-brief-heading" id="cft-example">
                Made here from a brief like this
              </h2>
              <a className="cft-example" href={`#/artifacts/${example.id}`}>
                <span className="cft-example-preview" aria-hidden="true">
                  <StartPreview start={start} examples={examples} />
                </span>
                <span className="cft-example-title">{example.title}</span>
              </a>
            </section>
          ) : null
        }
      />
    </div>
  );
}
