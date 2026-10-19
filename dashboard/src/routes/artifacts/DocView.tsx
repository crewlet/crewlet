/**
 * A doc, rendered here.
 *
 * A doc is Markdown and runs no author code, so it is not framed on the
 * artifact origin: the engine answers its source as JSON
 * (`GET /artifacts/{id}/versions/{n}/source`) and the dashboard renders it
 * with its own element-constructing renderer (`lib/markdown.ts`), which builds
 * React elements and never parses markup, so nothing a document carries can
 * become script on this origin (X8). A link in it goes through the renderer's
 * own scheme check. It reads like a Knowledge page because it is drawn by the
 * same renderer and the same document styles.
 */

import { Document, Skeleton } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactSource } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useRest } from "~/lib/useRest.ts";
import { readSource } from "~/lib/artifacts/api.ts";
import { fmtBytes } from "~/lib/format.ts";

export function DocView({ detail, version }: { detail: ArtifactDetail; version: number }) {
  const source = useRest<ArtifactSource>(version > 0 ? `${detail.id}:${version}` : null, (signal) =>
    readSource(detail.id, version, signal),
  );
  if (!source.data) {
    return source.loading ? (
      <Skeleton variant="text" rows={8} label={`Loading ${detail.title}`} />
    ) : (
      <QueryState error={source.code} refusal={source.refusal} loading={false} />
    );
  }
  return (
    <article className="artifact-document" aria-label={detail.title}>
      <Document>{renderMarkdown(source.data.text, { anchors: true })}</Document>
      <p className="artifact-note">
        {source.data.path}, {fmtBytes(source.data.size)}
      </p>
    </article>
  );
}
