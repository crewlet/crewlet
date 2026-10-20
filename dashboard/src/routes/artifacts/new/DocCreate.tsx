/**
 * A new doc: its own create page, in the shared frame (`CreateShell`).
 * The body is the doc kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function DocCreate() {
  return (
    <CreateShell kind="doc">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="doc" size="lg" />}
          title="This page is on its way"
          description="Starting a doc here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
