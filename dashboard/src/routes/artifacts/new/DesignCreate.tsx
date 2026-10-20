/**
 * A new design: its own create page, in the shared frame (`CreateShell`).
 * The body is the design kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function DesignCreate() {
  return (
    <CreateShell kind="design">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="design" size="lg" />}
          title="This page is on its way"
          description="Starting a design here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
