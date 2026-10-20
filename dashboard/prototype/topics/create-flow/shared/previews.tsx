/**
 * Starting points drawn small, by the renderers the artifacts themselves use
 * (`Document` over the shipped Markdown renderer, `SheetGrid` read only, the
 * shipped slide renderer on a `SlideStage`), so what a person picks is what
 * they get. Decorative: the card around each says what it is.
 */

import { useId, useMemo } from "react";
import { Document, PreviewScale, SheetGrid, SlideStage } from "@crewlethq/ui";
import { renderMarkdown } from "~/lib/markdown.ts";
import { renderSlide } from "~/lib/slides.tsx";
import { sheetColumns } from "~/lib/artifacts/sheet.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import type { DocTemplate, SheetTemplateDef } from "./templates.ts";
import { rowsOf } from "./SheetCanvas.tsx";
import { starterSlides } from "./starterDeck.ts";

const noImages = () => null;

export function DocTemplatePreview({ template }: { template: DocTemplate }) {
  const md = `# ${template.suggested || "Untitled doc"}\n\n${template.markdown || "Start writing."}`;
  const body = useMemo(() => renderMarkdown(md, { image: noImages, links: "text" }), [md]);
  return (
    <span className="cf-preview cf-preview--doc">
      <PreviewScale width={560}>
        <span className="cf-preview-page">
          <Document measure="none" size="sm">
            {body}
          </Document>
        </span>
      </PreviewScale>
    </span>
  );
}

export function SheetTemplatePreview({ template }: { template: SheetTemplateDef }) {
  const people = useArtifactPeople();
  const collection = useMemo(() => template.schema()[0]!, [template]);
  const columns = useMemo(() => sheetColumns(collection, "owner").slice(0, 5), [collection]);
  const rows = useMemo(() => rowsOf(template), [template]);
  return (
    <span className="cf-preview cf-preview--sheet">
      <PreviewScale width={760}>
        <SheetGrid
          label={template.title}
          mode="read"
          fit="content"
          columns={columns}
          rows={rows}
          people={people.sheetPeople}
        />
      </PreviewScale>
    </span>
  );
}

export function DeckPreview({ title, slide = 0 }: { title: string; slide?: number }) {
  const prefix = useId().replace(/:/g, "");
  const s = useMemo(() => starterSlides(title || "Untitled deck")[slide]!, [title, slide]);
  return (
    <span className="cf-preview cf-preview--deck">
      <SlideStage label={`${title}, cover`} fit="contain">
        {renderSlide(s, { prefix, image: noImages, links: "text" })}
      </SlideStage>
    </span>
  );
}
