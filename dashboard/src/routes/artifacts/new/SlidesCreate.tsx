/**
 * A new slide deck: its own create page, in the shared frame (`CreateShell`).
 * The body is the slides kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function SlidesCreate() {
  return (
    <CreateShell kind="slides">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="slides" size="lg" />}
          title="This page is on its way"
          description="Starting a slide deck here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
