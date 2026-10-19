/**
 * A doc, drawn here: its tabs, the one open with its outline beside it, Edit
 * for the people who may publish, and Print.
 *
 * NATIVE, NEVER FRAMED. A doc is Markdown and runs no author code: the engine
 * answers each version tab by tab (`GET /artifacts/{id}/versions/{n}/doc`),
 * read through the format its publish was held to, and the dashboard draws
 * every tab with its own element-constructing renderer (`lib/markdown.ts`),
 * which builds React elements and never parses markup, so nothing a doc
 * carries can become script on this origin (X8). A picture is a file of the
 * version, fetched with the reader's credential and shown by object URL; an
 * address the text names is never loaded. It reads like a Knowledge page
 * because it is drawn by the same renderer, the same document styles and the
 * same outline.
 *
 * WHERE THE READER IS, IN THE ADDRESS. The tab on show is `doctab=<path>`, a
 * query on the artifact's own address, so a link opens on a tab and following
 * somebody moves this reader to the tab they are on: the tab is said to the
 * room as the region `tab:<path>`.
 *
 * A PERSON'S EDIT IS A VERSION (D54, D69): Edit turns the doc itself into the
 * editor, each tab's text edited in place and its tabs renamed, reordered,
 * added and removed beside it, and the bar's Edit into the editing state,
 * Cancel and Save, which publishes the whole change as one version against
 * the version it started from (`DocEditor`). ONE CONTROL ON THE BAR becomes
 * the other, so the bar stays one slim row either way.
 */

import { useEffect, useId, useMemo, useState } from "react";
import { Button, Document, Skeleton, TabPanel, Tabs, preloadRichEditor } from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import type { ArtifactDetail, ArtifactDoc, ArtifactDocAnswer } from "~/protocol/index.ts";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { OnThisPage } from "~/components/OnThisPage.tsx";
import { PrintOnly } from "~/components/PrintOnly.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useRest } from "~/lib/useRest.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { readDoc } from "~/lib/artifacts/api.ts";
import { currentTab, docImagePaths, tabImages, tabRegion } from "~/lib/artifacts/doc.ts";
import { useVersionImages } from "~/lib/artifacts/versionImages.ts";
import { DocEditor } from "./DocEditor.tsx";
import type { PresenceReport } from "./presence.ts";

export function DocView({
  detail,
  version,
  presence,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  presence: PresenceReport;
  /** Absent for a secondary drawing (a side of a compare): read only, and silent to the room. */
  onPublished?: ((version: number) => void) | undefined;
}) {
  const answer = useRest<ArtifactDocAnswer>(
    version > 0 ? `doc:${detail.id}:${version}` : null,
    (signal) => readDoc(detail.id, version, signal),
  );
  if (!answer.data) {
    return answer.loading ? (
      <Skeleton variant="text" rows={8} label={`Loading ${detail.title}`} />
    ) : (
      <QueryState error={answer.code} refusal={answer.refusal} loading={false} />
    );
  }
  return (
    <Doc
      key={`${detail.id}:${version}`}
      detail={detail}
      version={version}
      doc={answer.data.doc}
      presence={presence}
      onPublished={onPublished}
    />
  );
}

function Doc({
  detail,
  version,
  doc,
  presence,
  onPublished,
}: {
  detail: ArtifactDetail;
  version: number;
  doc: ArtifactDoc;
  presence: PresenceReport;
  onPublished?: ((version: number) => void) | undefined;
}) {
  const primary = onPublished !== undefined;
  const [tabParam, setTab] = useParam("doctab", "");
  const tab = currentTab(doc, tabParam);
  const panel = useId();
  const publish = useArtifactAccess({ app: true, role: "editor", held: detail.role });
  // ONLY THE HEAD IS EDITED, since a save is a version based on the head and
  // an older version is history to read or restore.
  const editable = primary && publish.can && version === detail.head_number;
  const [editing, setEditing] = useState(false);
  const [drafting, setDrafting] = useState(false);
  const paths = useMemo(() => docImagePaths(doc), [doc]);
  const loaded = useVersionImages(detail.id, version, paths);

  // WHERE THIS READER IS, said to the room: the tab, and whether they are
  // drafting a change to it.
  // While editing, the tab is the one the editor has open, which may be one
  // it added and no version holds yet.
  const { at } = presence;
  const where = editing && tabParam ? tabParam : tab.path;
  useEffect(() => {
    if (primary) at({ region: tabRegion(where) }, editing && drafting);
  }, [at, primary, where, editing, drafting]);

  const choose = (path: string) => setTab(path === doc.tabs[0]?.path ? "" : path);
  const many = doc.tabs.length > 1;
  const body = (
    <Document as="article" aria-label={many ? `${detail.title}, ${tab.title}` : detail.title}>
      {renderMarkdown(tab.text, { anchors: true, image: tabImages(tab, loaded) })}
    </Document>
  );

  if (editing) {
    return (
      <div className="doc-view">
        <DocEditor
          detail={detail}
          version={version}
          doc={doc}
          selected={tabParam}
          onSelect={choose}
          image={(path) => tabImages({ path }, loaded)}
          imageVersion={loaded}
          reading={(path) => {
            const t = doc.tabs.find((x) => x.path === path);
            return t ? (
              <Document>{renderMarkdown(t.text, { image: tabImages(t, loaded) })}</Document>
            ) : null;
          }}
          onDraft={setDrafting}
          onDone={() => {
            setEditing(false);
            setDrafting(false);
          }}
          onPublished={(n) => {
            setEditing(false);
            setDrafting(false);
            onPublished?.(n);
          }}
        />
        {primary && <DocPrint doc={doc} loaded={loaded} />}
      </div>
    );
  }

  return (
    <div className="doc-view">
      <div className="doc-bar">
        {many && (
          <Tabs
            variant="underline"
            size="sm"
            ariaLabel="Tabs"
            panelId={panel}
            items={doc.tabs.map((t) => ({ value: t.path, label: t.title }))}
            value={tab.path}
            onValueChange={choose}
          />
        )}
        <span className="doc-bar-actions">
          {editable && (
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<PencilGlyph size="sm" />}
              onPointerEnter={preloadRichEditor}
              onFocus={preloadRichEditor}
              onClick={() => setEditing(true)}
            >
              Edit
            </Button>
          )}
          <Button size="small" variant="ghost" onClick={() => window.print()}>
            Print
          </Button>
        </span>
      </div>
      <div className="doc-read">
        <div className="doc-read-side">
          <OnThisPage body={tab.text} />
        </div>
        <div className="doc-read-page">
          {many ? (
            <TabPanel id={panel} value={tab.path}>
              {body}
            </TabPanel>
          ) : (
            body
          )}
        </div>
      </div>
      {primary && <DocPrint doc={doc} loaded={loaded} />}
    </div>
  );
}

/**
 * Every tab in order, each under its title when the doc has several: what
 * the browser prints to PDF.
 */
function DocPrint({ doc, loaded }: { doc: ArtifactDoc; loaded: (path: string) => string | null }) {
  return (
    <PrintOnly>
      {doc.tabs.map((t) => (
        <section className="doc-print-tab" key={t.path}>
          {doc.tabs.length > 1 && <h1 className="doc-print-title">{t.title}</h1>}
          <Document>{renderMarkdown(t.text, { image: tabImages(t, loaded) })}</Document>
        </section>
      ))}
    </PrintOnly>
  );
}
