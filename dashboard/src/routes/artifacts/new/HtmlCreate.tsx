/**
 * A new HTML page: its own create page, in the shared frame (`CreateShell`).
 * The body is the html kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function HtmlCreate() {
  return (
    <CreateShell kind="html">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="html" size="lg" />}
          title="This page is on its way"
          description="Starting an HTML page here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
